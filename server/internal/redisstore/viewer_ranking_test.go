package redisstore

import (
	"context"
	"fmt"
	"net"
	"slices"
	"strings"
	"testing"

	"github.com/jay41chen/danmaku/server/internal/model"
	"github.com/redis/go-redis/v9"
)

// 排行是全域 key：每個 test 開始和結束都清掉（TEST_REDIS_URL 是專用的 test db）
func newTestRanking(t *testing.T) *RedisStore {
	t.Helper()
	s, _ := newTestPresence(t)
	s.client.Del(t.Context(), viewerRankingKey)
	t.Cleanup(func() { s.client.Del(context.Background(), viewerRankingKey) })
	return s
}

// 讓房間「活著」：直接建 presence key
func makeLive(t *testing.T, s *RedisStore, room string) {
	t.Helper()
	mustSync(t, s, []model.PresenceEntry{{Room: room, ConnID: "c1", UserID: 1}}, nil)
	t.Cleanup(func() { s.client.Del(context.Background(), presenceKey(room)) })
}

func mustTop(t *testing.T, s *RedisStore, n int) []model.RoomViewers {
	t.Helper()
	got, err := s.TopRooms(t.Context(), n)
	if err != nil {
		t.Fatalf("TopRooms: %v", err)
	}
	return got
}

func rankingScore(t *testing.T, s *RedisStore, room string) (float64, bool) {
	t.Helper()
	score, err := s.client.ZScore(t.Context(), viewerRankingKey, room).Result()
	if err == redis.Nil {
		return 0, false
	}
	if err != nil {
		t.Fatalf("ZScore: %v", err)
	}
	return score, true
}

// count > 0 → ZADD（更新分數）；count == 0 → ZREM；空 map 不報錯
func TestUpdateViewerRanking(t *testing.T) {
	s := newTestRanking(t)
	if err := s.UpdateViewerRanking(t.Context(), nil); err != nil {
		t.Fatalf("empty map: %v", err)
	}
	if err := s.UpdateViewerRanking(t.Context(), map[string]int{"vr-a": 3, "vr-b": 5}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if sc, ok := rankingScore(t, s, "vr-a"); !ok || sc != 3 {
		t.Errorf("vr-a = %v,%v want 3,true", sc, ok)
	}
	if err := s.UpdateViewerRanking(t.Context(), map[string]int{"vr-a": 0, "vr-b": 7, "vr-never": 0}); err != nil {
		t.Fatalf("update 2: %v", err)
	}
	if _, ok := rankingScore(t, s, "vr-a"); ok {
		t.Errorf("vr-a should be removed when count == 0")
	}
	if sc, ok := rankingScore(t, s, "vr-b"); !ok || sc != 7 {
		t.Errorf("vr-b = %v,%v want 7,true", sc, ok)
	}
}

// 由多到少，且只回 n 間
func TestTopRoomsOrdering(t *testing.T) {
	s := newTestRanking(t)
	counts := map[string]int{"vr-a": 1, "vr-b": 9, "vr-c": 4, "vr-d": 6}
	for room := range counts {
		makeLive(t, s, room)
	}
	if err := s.UpdateViewerRanking(t.Context(), counts); err != nil {
		t.Fatal(err)
	}
	got := mustTop(t, s, 3)
	want := []model.RoomViewers{{PublicID: "vr-b", Count: 9}, {PublicID: "vr-d", Count: 6}, {PublicID: "vr-c", Count: 4}}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := mustTop(t, s, 0); len(got) != 0 {
		t.Errorf("n=0 got %v", got)
	}
}

// 不變量 1：最後一個人離開（presence key 被 Redis 自動刪掉）→ 下次 TopRooms 排除，且從排行 ZREM
func TestTopRoomsLastViewerLeaves(t *testing.T) {
	s := newTestRanking(t)
	e := model.PresenceEntry{Room: "vr-a", ConnID: "c1", UserID: 1}
	mustSync(t, s, []model.PresenceEntry{e}, nil)
	makeLive(t, s, "vr-b")
	s.UpdateViewerRanking(t.Context(), map[string]int{"vr-a": 8, "vr-b": 2})

	mustSync(t, s, nil, []model.PresenceEntry{e})
	if n, _ := s.client.Exists(t.Context(), presenceKey("vr-a")).Result(); n != 0 {
		t.Fatalf("presence key should be gone")
	}

	got := mustTop(t, s, 5)
	if len(got) != 1 || got[0].PublicID != "vr-b" {
		t.Errorf("got %v, want only vr-b", got)
	}
	if _, ok := rankingScore(t, s, "vr-a"); ok {
		t.Errorf("vr-a should be ZREMed from ranking")
	}
}

// 不變量 2：presence key 過期或被刪 → 排除並移出排行
func TestTopRoomsPresenceKeyExpired(t *testing.T) {
	s := newTestRanking(t)
	makeLive(t, s, "vr-a")
	makeLive(t, s, "vr-b")
	s.UpdateViewerRanking(t.Context(), map[string]int{"vr-a": 8, "vr-b": 2})
	s.client.Del(t.Context(), presenceKey("vr-a")) // 等同 EXPIRE 到期

	got := mustTop(t, s, 5)
	if len(got) != 1 || got[0].PublicID != "vr-b" || got[0].Count != 2 {
		t.Errorf("got %v, want only vr-b(2)", got)
	}
	if _, ok := rankingScore(t, s, "vr-a"); ok {
		t.Errorf("vr-a should be removed from ranking")
	}
}

// 分批：stale 房間（分數高）塞滿前面好幾批，後面的活房間仍要找得到
func TestTopRoomsBatchingSkipsManyStale(t *testing.T) {
	s := newTestRanking(t)
	const n = 3
	counts := map[string]int{}
	for i := 0; i < 40; i++ { // 40 > 兩批（n+10 = 13）
		counts[fmt.Sprintf("vr-stale-%02d", i)] = 1000 + i
	}
	for i := 0; i < 5; i++ {
		room := fmt.Sprintf("vr-live-%d", i)
		makeLive(t, s, room)
		counts[room] = 10 - i
	}
	s.UpdateViewerRanking(t.Context(), counts)

	got := mustTop(t, s, n)
	want := []model.RoomViewers{{PublicID: "vr-live-0", Count: 10}, {PublicID: "vr-live-1", Count: 9}, {PublicID: "vr-live-2", Count: 8}}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	card, _ := s.client.ZCard(t.Context(), viewerRankingKey).Result()
	if card > 5 { // 至少前三批 stale 被清掉；40 個 stale 在 3 輪內（每輪 13）全部走過
		t.Errorf("ranking card = %d, want stale entries cleaned (<= 5)", card)
	}
}

// 在第一次 ZREVRANGE 回來之後，把 vr-a 的分數改成最低（模擬別台 server 在兩批之間 ZADD）
type moveAfterFirstBatchHook struct {
	s    *RedisStore
	done bool
}

func (h *moveAfterFirstBatchHook) DialHook(next redis.DialHook) redis.DialHook {
	return func(ctx context.Context, network, addr string) (net.Conn, error) { return next(ctx, network, addr) }
}

func (h *moveAfterFirstBatchHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		if !h.done && strings.HasPrefix(cmd.Name(), "zrevrange") {
			h.done = true
			h.s.client.ZAdd(ctx, viewerRankingKey, redis.Z{Score: 1, Member: "vr-a"})
		}
		return err
	}
}

func (h *moveAfterFirstBatchHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// 跨批去重：vr-a 在第一批被收進結果，兩批之間分數掉到最低，第二批又撈到它 → 不能出現兩次
func TestTopRoomsDedupAcrossBatches(t *testing.T) {
	s := newTestRanking(t)
	const n = 2 // batchSize = 12
	counts := map[string]int{"vr-a": 1000}
	makeLive(t, s, "vr-a")
	for i := 0; i < 11; i++ { // 第一批剩下的 11 個位置全是 stale，會被 ZREM
		counts[fmt.Sprintf("vr-stale-%02d", i)] = 500 + i
	}
	makeLive(t, s, "vr-b")
	counts["vr-b"] = 50
	s.UpdateViewerRanking(t.Context(), counts)
	s.client.AddHook(&moveAfterFirstBatchHook{s: s})

	got := mustTop(t, s, n)
	seen := map[string]int{}
	for _, r := range got {
		seen[r.PublicID]++
	}
	if seen["vr-a"] != 1 {
		t.Errorf("vr-a appears %d times in %v, want exactly 1", seen["vr-a"], got)
	}
}
