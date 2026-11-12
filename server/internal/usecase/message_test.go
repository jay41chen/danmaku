package usecase

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jay41chen/danmaku/server/internal/model"
)

// 訊息 use case 的假 store
type fakeMessageStore struct {
	created      model.Message
	banned       bool
	createErr    error
	createCalls  int
	gotRoom      string
	gotUserID    int64
	gotContent   string
	room         model.Room
	getRoomErr   error
	msgs         []model.Message
	queryErr     error
	queryCalls   int
	gotQueryRoom int64
	gotLimit     int
}

func (f *fakeMessageStore) CreateMessage(ctx context.Context, roomPublicID string, userID int64, content string) (model.Message, bool, error) {
	f.createCalls++
	f.gotRoom, f.gotUserID, f.gotContent = roomPublicID, userID, content
	return f.created, f.banned, f.createErr
}

func (f *fakeMessageStore) QueryMessage(ctx context.Context, roomID int64, limit int) ([]model.Message, error) {
	f.queryCalls++
	f.gotQueryRoom, f.gotLimit = roomID, limit
	return f.msgs, f.queryErr
}

func (f *fakeMessageStore) GetRoomByPublicID(ctx context.Context, publicID string) (model.Room, error) {
	return f.room, f.getRoomErr
}

type fakeMessageNotifier struct {
	calls   int
	gotRoom string
	gotMsg  model.Message
}

func (f *fakeMessageNotifier) NotifyMessage(ctx context.Context, roomPublicID string, msg model.Message) {
	f.calls++
	f.gotRoom, f.gotMsg = roomPublicID, msg
}

// 假 limiter：blocks 的 key → 等待時間；err 不是 nil 就回錯
type fakeMessageLimiter struct {
	blocks map[string]time.Duration
	err    error
	keys   []string
}

func (f *fakeMessageLimiter) Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, time.Duration, error) {
	f.keys = append(f.keys, key)
	if f.err != nil {
		return false, 0, f.err
	}
	if d, ok := f.blocks[key]; ok {
		return false, d, nil
	}
	return true, 0, nil
}

const (
	burstKey   = "rl:message:burst:7"
	sustainKey = "rl:message:sustain:7"
)

func TestPostInvalidContentTouchesNothing(t *testing.T) {
	store, lim, n := &fakeMessageStore{}, &fakeMessageLimiter{}, &fakeMessageNotifier{}
	uc := NewMessageUseCase(store, n, lim)

	_, err := uc.Post(context.Background(), "room", 7, "")

	var coded *model.CodedError
	if !errors.As(err, &coded) {
		t.Fatalf("err = %v, want *model.CodedError", err)
	}
	if len(lim.keys) != 0 || store.createCalls != 0 || n.calls != 0 {
		t.Errorf("limiter %v / store %d / notify %d called, want none", lim.keys, store.createCalls, n.calls)
	}
}

func TestPostRateLimited(t *testing.T) {
	cases := []struct {
		name   string
		blocks map[string]time.Duration
		want   time.Duration
	}{
		{"burst only", map[string]time.Duration{burstKey: time.Second}, time.Second},
		{"sustain only", map[string]time.Duration{sustainKey: 8 * time.Second}, 8 * time.Second},
		{"both uses max (burst longer)", map[string]time.Duration{burstKey: 9 * time.Second, sustainKey: 2 * time.Second}, 9 * time.Second},
		{"both uses max (sustain longer)", map[string]time.Duration{burstKey: time.Second, sustainKey: 8 * time.Second}, 8 * time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, lim, n := &fakeMessageStore{}, &fakeMessageLimiter{blocks: tc.blocks}, &fakeMessageNotifier{}
			uc := NewMessageUseCase(store, n, lim)

			_, err := uc.Post(context.Background(), "room", 7, "hi")

			var limited *model.RateLimitedError
			if !errors.As(err, &limited) {
				t.Fatalf("err = %v, want *model.RateLimitedError", err)
			}
			if limited.RetryAfter != tc.want {
				t.Errorf("RetryAfter = %v, want %v", limited.RetryAfter, tc.want)
			}
			if !slices.Equal(lim.keys, []string{burstKey, sustainKey}) {
				t.Errorf("asked keys = %v, want both rules", lim.keys)
			}
			if store.createCalls != 0 || n.calls != 0 {
				t.Errorf("store %d / notify %d called, want none", store.createCalls, n.calls)
			}
		})
	}
}

func TestPostLimiterErrorFailsOpen(t *testing.T) {
	store := &fakeMessageStore{created: model.Message{ID: 1}}
	n := &fakeMessageNotifier{}
	uc := NewMessageUseCase(store, n, &fakeMessageLimiter{err: errors.New("redis down")})

	msg, err := uc.Post(context.Background(), "room", 7, "hi")

	if err != nil || msg.ID != 1 {
		t.Fatalf("msg %+v err %v, want success", msg, err)
	}
	if store.createCalls != 1 {
		t.Errorf("store calls = %d, want 1", store.createCalls)
	}
}

func TestPostNotify(t *testing.T) {
	created := model.Message{ID: 42, Content: "hi"}
	for _, banned := range []bool{false, true} {
		store := &fakeMessageStore{created: created, banned: banned}
		n := &fakeMessageNotifier{}
		uc := NewMessageUseCase(store, n, &fakeMessageLimiter{})

		msg, err := uc.Post(context.Background(), "room-x", 7, "hi")

		if err != nil || msg.ID != 42 {
			t.Fatalf("banned=%v msg %+v err %v, want created msg", banned, msg, err)
		}
		if store.gotRoom != "room-x" || store.gotUserID != 7 || store.gotContent != "hi" {
			t.Errorf("store got %q/%d/%q", store.gotRoom, store.gotUserID, store.gotContent)
		}
		if banned {
			if n.calls != 0 {
				t.Errorf("banned author notified %d times, want 0", n.calls)
			}
			continue
		}
		if n.calls != 1 || n.gotRoom != "room-x" || n.gotMsg.ID != 42 {
			t.Errorf("notify calls %d room %q msg %+v, want once with room-x / msg 42", n.calls, n.gotRoom, n.gotMsg)
		}
	}
}

func TestPostStoreErrorPassesThroughWithoutNotify(t *testing.T) {
	for _, want := range []error{model.ErrRoomNotFound, model.ErrMessageUserNotFound, errors.New("db down")} {
		n := &fakeMessageNotifier{}
		uc := NewMessageUseCase(&fakeMessageStore{createErr: want}, n, &fakeMessageLimiter{})

		_, err := uc.Post(context.Background(), "room", 7, "hi")

		if err != want {
			t.Errorf("err = %v, want %v as-is", err, want)
		}
		if n.calls != 0 {
			t.Errorf("notified %d times on error, want 0", n.calls)
		}
	}
}

func TestHistoryReversesAndPassesLimit(t *testing.T) {
	store := &fakeMessageStore{
		room: model.Room{ID: 9},
		msgs: []model.Message{{ID: 3}, {ID: 2}, {ID: 1}}, // DB 是新到舊
	}
	uc := NewMessageUseCase(store, &fakeMessageNotifier{}, &fakeMessageLimiter{})

	got, err := uc.History(context.Background(), "room")

	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].ID != 1 || got[1].ID != 2 || got[2].ID != 3 {
		t.Errorf("order = %+v, want oldest first (1,2,3)", got)
	}
	if store.gotQueryRoom != 9 || store.gotLimit != HistoryLimit {
		t.Errorf("query room %d limit %d, want 9 / %d", store.gotQueryRoom, store.gotLimit, HistoryLimit)
	}
}

func TestHistoryErrorsAreWrappedWithStep(t *testing.T) {
	t.Run("get room", func(t *testing.T) {
		store := &fakeMessageStore{getRoomErr: model.ErrRoomNotFound}
		uc := NewMessageUseCase(store, &fakeMessageNotifier{}, &fakeMessageLimiter{})

		_, err := uc.History(context.Background(), "room")

		if !errors.Is(err, model.ErrRoomNotFound) {
			t.Errorf("err = %v, want errors.Is ErrRoomNotFound", err)
		}
		if err == nil || !strings.HasPrefix(err.Error(), "get room: ") {
			t.Errorf("err = %v, want prefix %q", err, "get room: ")
		}
		if store.queryCalls != 0 {
			t.Errorf("QueryMessage called %d times, want 0", store.queryCalls)
		}
	})
	t.Run("query messages", func(t *testing.T) {
		cause := errors.New("db down")
		store := &fakeMessageStore{queryErr: cause}
		uc := NewMessageUseCase(store, &fakeMessageNotifier{}, &fakeMessageLimiter{})

		_, err := uc.History(context.Background(), "room")

		if !errors.Is(err, cause) {
			t.Errorf("err = %v, want errors.Is cause", err)
		}
		if err == nil || !strings.HasPrefix(err.Error(), "query messages: ") {
			t.Errorf("err = %v, want prefix %q", err, "query messages: ")
		}
	})
}
