package handler

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jay41chen/danmaku/server/internal/auth"
	"github.com/jay41chen/danmaku/server/internal/model"
	"github.com/jay41chen/danmaku/server/internal/store"
)

// handler test 共用的假 DB：只實作用得到的方法，其他的呼叫到會 nil panic
type fakeDBStore struct {
	store.DBStoreInterface // 沒填 → nil；其他 4 個方法「有」，但被呼叫會 nil panic
	msgs                   []model.Message
	err                    error

	// CreateMessage 用
	createdMsg       model.Message
	createMsgErr     error
	gotMsgUserID     int64 // spy：CreateMessage 收到的 user id
	gotMsgContent    string
	gotMsgRoomPublic string // spy：CreateMessage 收到的 room public id
	gotQueryRoomID   int64  // spy：QueryMessage 收到的 room id
	queryMsgCalled   bool   // spy：QueryMessage 有沒有被呼叫（不能用 gotQueryRoomID == 0 判斷：零值 room 也會傳 0）
	createMsgCalled  bool

	// CreateUser 用
	createID    int64
	createErr   error
	gotUsername string // spy：handler 傳進來的參數
	gotHash     string

	// GetUser 用
	user          model.User
	getErr        error
	getUserCalled bool // spy：GetUser 有沒有被呼叫

	// GetUserByID 用
	userByID    model.User
	getByIDErr  error
	gotByIDCall int64 // spy：GetUserByID 收到的 id

	// refresh token 用
	createTokenErr error
	gotTokenUserID int64 // spy：CreateRefreshToken 收到的 user id
	gotTokenHash   string
	rotateUserID   int64
	rotateErr      error
	gotOldHash     string // spy：RotateRefreshToken 收到的舊 hash
	revokeErr      error
	gotRevokeHash  string // spy：RevokeRefreshToken 收到的 hash

	// CreateRoom 用
	createdRoom     model.Room
	createRoomErr   error
	gotRoomOwnerID  int64 // spy：CreateRoom 收到的 owner id
	gotRoomName     string
	createRoomCalls int

	// GetRoomByPublicID 用
	room          model.Room
	getRoomErr    error
	gotRoomPublic string // spy：GetRoomByPublicID 收到的 public id
}

func (f *fakeDBStore) QueryMessage(ctx context.Context, roomID int64, limit int) ([]model.Message, error) {
	f.queryMsgCalled = true
	f.gotQueryRoomID = roomID
	return f.msgs, f.err
}

func (f *fakeDBStore) CreateMessage(ctx context.Context, roomPublicID string, userID int64, content string) (model.Message, error) {
	f.createMsgCalled = true
	f.gotMsgRoomPublic, f.gotMsgUserID, f.gotMsgContent = roomPublicID, userID, content
	return f.createdMsg, f.createMsgErr
}

func (f *fakeDBStore) CreateUser(ctx context.Context, username, passwordHash string) (int64, error) {
	f.gotUsername, f.gotHash = username, passwordHash
	return f.createID, f.createErr
}

func (f *fakeDBStore) GetUser(ctx context.Context, username string) (model.User, error) {
	f.getUserCalled = true
	return f.user, f.getErr
}

func (f *fakeDBStore) GetUserByID(ctx context.Context, id int64) (model.User, error) {
	f.gotByIDCall = id
	return f.userByID, f.getByIDErr
}

func (f *fakeDBStore) CreateRefreshToken(ctx context.Context, userID int64, tokenHash string, ttl time.Duration) error {
	f.gotTokenUserID, f.gotTokenHash = userID, tokenHash
	return f.createTokenErr
}

func (f *fakeDBStore) RotateRefreshToken(ctx context.Context, oldHash, newHash string, ttl, grace time.Duration) (int64, error) {
	f.gotOldHash = oldHash
	return f.rotateUserID, f.rotateErr
}

func (f *fakeDBStore) RevokeRefreshToken(ctx context.Context, tokenHash string) error {
	f.gotRevokeHash = tokenHash
	return f.revokeErr
}

func (f *fakeDBStore) CreateRoom(ctx context.Context, ownerID int64, name string) (model.Room, error) {
	f.createRoomCalls++
	f.gotRoomOwnerID, f.gotRoomName = ownerID, name
	return f.createdRoom, f.createRoomErr
}

func (f *fakeDBStore) GetRoomByPublicID(ctx context.Context, publicID string) (model.Room, error) {
	f.gotRoomPublic = publicID
	return f.room, f.getRoomErr
}

// handler test 用的假 limiter：照 test 設好的劇本回答，不真的計數
var _ store.RateLimiterInterface = (*fakeRateLimiter)(nil) // compile 時檢查 fake 有實作 interface

type fakeRateLimiter struct {
	blockKey   string                   // 收到這個 key 就回 allowed=false
	retryAfter time.Duration            // 被擋時回傳的等待時間
	blocks     map[string]time.Duration // 多個 key 同時被擋時用：key → 該 key 的等待時間
	err        error                    // 不是 nil 就直接回傳這個錯（模擬 Redis 掛了）

	allowKeys  []string // spy：Allow 收到過的 key
	resetKeys  []string // spy：Reset 收到過的 key
	refundKeys []string // spy：Refund 收到過的 key
}

func (f *fakeRateLimiter) Allow(ctx context.Context, key string, limit int, window time.Duration) (bool, time.Duration, error) {
	f.allowKeys = append(f.allowKeys, key) // spy：記下被問過的 key
	if f.err != nil {
		return false, 0, f.err // 模擬 Redis 掛了；有 err 時呼叫端不看前兩個值
	}
	if key == f.blockKey {
		return false, f.retryAfter, nil
	}
	if d, ok := f.blocks[key]; ok {
		return false, d, nil
	}
	return true, 0, nil
}

func (f *fakeRateLimiter) Reset(ctx context.Context, key string) error {
	f.resetKeys = append(f.resetKeys, key) // spy：只記錄，不真的清什麼
	return nil
}

func (f *fakeRateLimiter) Refund(ctx context.Context, key string) error {
	f.refundKeys = append(f.refundKeys, key) // spy：只記錄，不真的退
	return nil
}

// handler test 共用的 token issuer：簽 / 驗都是純計算、沒有 I/O，直接用真的，不用 fake
var testTokens = func() *auth.TokenIssuer {
	issuer, err := auth.NewTokenIssuer(strings.Repeat("k", 32), time.Hour)
	if err != nil {
		panic(err)
	}
	return issuer
}()

// 一定簽失敗的 issuer：真的 HS256 不會失敗，只能用 fake 走到這條路
type failingTokenIssuer struct{}

func (failingTokenIssuer) IssueAccessToken(userID int64) (string, time.Duration, error) {
	return "", 0, errors.New("sign failed")
}

// handler test 用的假 Redis：記下 Publish / JoinRoom / LeaveRoom 的呼叫，Subscribe 只存 handler 然後立刻返回
// WS 的 handler 在 server 端 goroutine 呼叫 JoinRoom / LeaveRoom，所以這些 spy 都用 mutex 保護
// 同步執行的 test 可以直接讀 published / publishedRooms
var _ store.RedisStoreInterface = (*fakeRedis)(nil)

type fakeRedis struct {
	publishErr error
	published  []string // spy：Publish 收到過的訊息
	// spy：Publish 收到過的 room id（跟 published 同 index）
	publishedRooms []string

	mu      sync.Mutex
	handler func(roomID, msg string) // Subscribe 收到的 handler，test 用 deliver 推訊息進去
	joinErr error                    // 不是 nil 時 JoinRoom 回傳這個錯
	joined  []string                 // spy：JoinRoom 收到過的 room id
	left    []string                 // spy：LeaveRoom 收到過的 room id

	// 不是 nil 時 JoinRoom 會先對 joinEntered 送一個訊號，再卡到 joinBlock 被 close 才回傳
	// 用來在 test 裡把 JoinRoom 停在「Redis I/O 進行中」，製造 race 情境；兩個都是 nil 時不影響既有 test
	joinEntered chan struct{}
	joinBlock   chan struct{}
}

func (f *fakeRedis) Publish(ctx context.Context, roomID, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.published = append(f.published, msg)
	f.publishedRooms = append(f.publishedRooms, roomID)
	return f.publishErr
}

func (f *fakeRedis) Subscribe(ctx context.Context, handler func(roomID, msg string)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handler = handler
	return nil
}

func (f *fakeRedis) JoinRoom(ctx context.Context, roomID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.joined = append(f.joined, roomID)
	err := f.joinErr
	entered, block := f.joinEntered, f.joinBlock
	f.mu.Unlock()
	if entered != nil {
		entered <- struct{}{}
	}
	if block != nil {
		<-block
	}
	f.mu.Lock() // 讓上面的 defer Unlock 成對
	return err
}

func (f *fakeRedis) LeaveRoom(ctx context.Context, roomID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.left = append(f.left, roomID)
}

func (f *fakeRedis) CheckHealth(ctx context.Context) error { return nil }

// 模擬 Redis 推來一則訊息：呼叫 Subscribe 存下的 handler（要先呼叫 StartBroadcast）
func (f *fakeRedis) deliver(roomID, msg string) {
	f.mu.Lock()
	h := f.handler
	f.mu.Unlock()
	h(roomID, msg)
}

func (f *fakeRedis) joinedRooms() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.joined)
}

func (f *fakeRedis) leftRooms() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.left)
}
