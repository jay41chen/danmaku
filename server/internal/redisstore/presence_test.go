package redisstore

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jay41chen/danmaku/server/internal/model"
	"github.com/redis/go-redis/v9"
)

// 每個 test 用自己的房間名（presence-test-<test 名稱>），結束時清掉 key；不 FLUSHDB
func newTestPresence(t *testing.T) (*RedisStore, string) {
	t.Helper()
	url := os.Getenv("TEST_REDIS_URL")
	if url == "" {
		t.Skip("TEST_REDIS_URL not set; skipping Redis integration test")
	}
	s, err := NewRedisStore(t.Context(), url)
	if err != nil {
		t.Fatalf("NewRedisStore: %v", err)
	}
	t.Cleanup(func() { s.client.Close() })

	room := "presence-test-" + t.Name()
	s.client.Del(t.Context(), presenceKey(room))
	t.Cleanup(func() { s.client.Del(context.Background(), presenceKey(room)) })
	return s, room
}

func mustCount(t *testing.T, s *RedisStore, room string) int {
	t.Helper()
	got, err := s.CountPresence(t.Context(), []string{room})
	if err != nil {
		t.Fatalf("CountPresence: %v", err)
	}
	n, ok := got[room]
	if !ok {
		t.Fatalf("CountPresence result %v missing room %q", got, room)
	}
	return n
}

func mustSync(t *testing.T, s *RedisStore, add, remove []model.PresenceEntry) {
	t.Helper()
	if err := s.SyncPresence(t.Context(), add, remove); err != nil {
		t.Fatalf("SyncPresence: %v", err)
	}
}

// add 之後算得到；不同 user 各算 1
func TestPresenceSyncThenCount(t *testing.T) {
	s, room := newTestPresence(t)
	mustSync(t, s, []model.PresenceEntry{
		{Room: room, ConnID: "c1", UserID: 1},
		{Room: room, ConnID: "c2", UserID: 2},
	}, nil)
	if got := mustCount(t, s, room); got != 2 {
		t.Errorf("count = %d, want 2", got)
	}
}

// 同一個 user 兩條連線只算 1（含跨 server 的情況）
func TestPresenceSameUserDedupe(t *testing.T) {
	s, room := newTestPresence(t)
	mustSync(t, s, []model.PresenceEntry{
		{Room: room, ConnID: "c1", UserID: 42},
		{Room: room, ConnID: "c2", UserID: 42},
	}, nil)
	if got := mustCount(t, s, room); got != 1 {
		t.Errorf("count = %d, want 1", got)
	}
}

// 匿名每條連線各算 1
func TestPresenceAnonymousCountedPerConn(t *testing.T) {
	s, room := newTestPresence(t)
	mustSync(t, s, []model.PresenceEntry{
		{Room: room, ConnID: "c1", UserID: 0},
		{Room: room, ConnID: "c2", UserID: 0},
	}, nil)
	if got := mustCount(t, s, room); got != 2 {
		t.Errorf("count = %d, want 2", got)
	}
}

// remove 之後不再被算；匿名 → 登入可以在同一次呼叫換掉 member
func TestPresenceRemove(t *testing.T) {
	s, room := newTestPresence(t)
	e1 := model.PresenceEntry{Room: room, ConnID: "c1", UserID: 0}
	e2 := model.PresenceEntry{Room: room, ConnID: "c2", UserID: 0}
	mustSync(t, s, []model.PresenceEntry{e1, e2}, nil)
	mustSync(t, s, nil, []model.PresenceEntry{e1})
	if got := mustCount(t, s, room); got != 1 {
		t.Errorf("count = %d, want 1", got)
	}
	// 換身分：移除 {c2,0}、加入 {c2,42}
	mustSync(t, s, []model.PresenceEntry{{Room: room, ConnID: "c2", UserID: 42}}, []model.PresenceEntry{e2})
	members, err := s.client.ZRange(t.Context(), presenceKey(room), 0, -1).Result()
	if err != nil {
		t.Fatalf("ZRange: %v", err)
	}
	if len(members) != 1 || members[0] != "c2:42" {
		t.Errorf("members = %v, want [c2:42]", members)
	}
}

// 寫入後 key 有 TTL（約 30 秒），房間沒人續期時整個會消失
func TestPresenceKeyHasTTL(t *testing.T) {
	s, room := newTestPresence(t)
	mustSync(t, s, []model.PresenceEntry{{Room: room, ConnID: "c1", UserID: 1}}, nil)
	ttl, err := s.client.TTL(t.Context(), presenceKey(room)).Result()
	if err != nil {
		t.Fatalf("TTL: %v", err)
	}
	if ttl < 25*time.Second || ttl > presenceTTL {
		t.Errorf("ttl = %v, want about %v", ttl, presenceTTL)
	}
}

// score 比 now-30 還舊的 member 在計數時被剔除（也真的從 Redis 刪掉）
func TestPresenceStaleMembersPruned(t *testing.T) {
	s, room := newTestPresence(t)
	mustSync(t, s, []model.PresenceEntry{{Room: room, ConnID: "fresh", UserID: 1}}, nil)

	tm, err := s.client.Time(t.Context()).Result()
	if err != nil {
		t.Fatalf("TIME: %v", err)
	}
	stale := float64(tm.Unix() - 31)
	if err := s.client.ZAdd(t.Context(), presenceKey(room), redis.Z{Score: stale, Member: "stale:2"}).Err(); err != nil {
		t.Fatalf("ZAdd: %v", err)
	}

	if got := mustCount(t, s, room); got != 1 {
		t.Errorf("count = %d, want 1 (stale pruned)", got)
	}
	n, err := s.client.ZCard(t.Context(), presenceKey(room)).Result()
	if err != nil {
		t.Fatalf("ZCard: %v", err)
	}
	if n != 1 {
		t.Errorf("zcard = %d, want 1", n)
	}
}

// 沒有任何 member 的房間 = 0，而且結果裡一定有這個房間；空 rooms 不出錯
func TestPresenceAbsentRoomIsZero(t *testing.T) {
	s, room := newTestPresence(t)
	if got := mustCount(t, s, room); got != 0 {
		t.Errorf("count = %d, want 0", got)
	}
	got, err := s.CountPresence(t.Context(), nil)
	if err != nil || len(got) != 0 {
		t.Errorf("CountPresence(nil) = %v, %v; want empty, nil", got, err)
	}
}

// 一次算多間房：各算各的
func TestPresenceCountMultipleRooms(t *testing.T) {
	s, room := newTestPresence(t)
	other := room + "-other"
	t.Cleanup(func() { s.client.Del(context.Background(), presenceKey(other)) })
	mustSync(t, s, []model.PresenceEntry{
		{Room: room, ConnID: "c1", UserID: 1},
		{Room: other, ConnID: "c1", UserID: 1},
		{Room: other, ConnID: "c2", UserID: 2},
	}, nil)
	got, err := s.CountPresence(t.Context(), []string{room, other, room + "-none"})
	if err != nil {
		t.Fatalf("CountPresence: %v", err)
	}
	if got[room] != 1 || got[other] != 2 || got[room+"-none"] != 0 || len(got) != 3 {
		t.Errorf("counts = %v", got)
	}
}

// member 解析要從最後一個 ':' 切（user_id 在最後）
func TestParsePresenceMember(t *testing.T) {
	connID, userID, ok := parsePresenceMember("a-b_c:42")
	if !ok || connID != "a-b_c" || userID != 42 {
		t.Errorf("got %q %d %v", connID, userID, ok)
	}
	if _, _, ok := parsePresenceMember("nocolon"); ok {
		t.Error("want !ok for missing colon")
	}
	if _, _, ok := parsePresenceMember("x:1y"); ok {
		t.Error("want !ok for non-numeric user id")
	}
}

// 對已存在的 member 再 ZADD 要更新 score（續期真的有效）；若改成 ZADD NX 這個 test 會失敗
func TestPresenceReAddRenewsScore(t *testing.T) {
	s, room := newTestPresence(t)
	tm, err := s.client.Time(t.Context()).Result()
	if err != nil {
		t.Fatalf("TIME: %v", err)
	}
	stale := float64(tm.Unix() - 31)
	if err := s.client.ZAdd(t.Context(), presenceKey(room), redis.Z{Score: stale, Member: "c1:1"}).Err(); err != nil {
		t.Fatalf("ZAdd: %v", err)
	}

	mustSync(t, s, []model.PresenceEntry{{Room: room, ConnID: "c1", UserID: 1}}, nil)

	if got := mustCount(t, s, room); got != 1 {
		t.Errorf("count = %d, want 1 (score renewed)", got)
	}
}
