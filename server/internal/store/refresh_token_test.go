package store

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// 需要真的 Postgres：沒設 TEST_DATABASE_URL 就 skip（見 newTestDBStore）
// SQL 字串裡的錯（= NULL、多一個逗號、少傳 $1）compiler 抓不到，fake 也抓不到，只有這裡抓得到

const (
	testTTL   = 7 * 24 * time.Hour
	testGrace = 15 * time.Second
)

func TestRotateRefreshToken(t *testing.T) {
	t.Run("active token rotates: old revoked, new stored", func(t *testing.T) {
		s := newTestDBStore(t)
		userID := createTestUser(t, s, "jay")
		mustCreateToken(t, s, userID, "old", testTTL)

		got, err := s.RotateRefreshToken(t.Context(), "old", "new", testTTL, testGrace)

		if err != nil {
			t.Fatalf("Rotate(old) error = %v, want nil", err)
		}
		if got != userID {
			t.Errorf("Rotate(old) userID = %d, want %d", got, userID)
		}
		if !isRevoked(t, s, "old") {
			t.Errorf("old token not revoked after rotate")
		}
		if isRevoked(t, s, "new") {
			t.Errorf("new token is revoked, want active")
		}
	})

	// 回應掉了、client 拿同一個 old 重送：要拿到新的，而且不能動到其他 token
	t.Run("retry within grace issues another token without revoking others", func(t *testing.T) {
		s := newTestDBStore(t)
		userID := createTestUser(t, s, "jay")
		mustCreateToken(t, s, userID, "old", testTTL)
		mustCreateToken(t, s, userID, "other-device", testTTL)
		mustRotate(t, s, "old", "new1", testGrace)

		got, err := s.RotateRefreshToken(t.Context(), "old", "new2", testTTL, testGrace)

		if err != nil {
			t.Fatalf("retry Rotate(old) error = %v, want nil", err)
		}
		if got != userID {
			t.Errorf("retry Rotate(old) userID = %d, want %d", got, userID)
		}
		// new2 真的有存進去：之前 inGrace 直接 return 的 bug 就是這裡會抓到
		for _, h := range []string{"new1", "new2", "other-device"} {
			if isRevoked(t, s, h) {
				t.Errorf("%s is revoked, want active", h)
			}
		}
	})

	t.Run("duplicate use after grace revokes all tokens of that user only", func(t *testing.T) {
		s := newTestDBStore(t)
		userID := createTestUser(t, s, "jay")
		otherID := createTestUser(t, s, "alice")
		mustCreateToken(t, s, userID, "old", testTTL)
		mustCreateToken(t, s, userID, "other-device", testTTL)
		mustCreateToken(t, s, otherID, "alice-token", testTTL)
		mustRotate(t, s, "old", "new", testGrace)

		// grace = 0：不用真的等 15 秒，「作廢的時間」一定早於這次的 now()
		_, err := s.RotateRefreshToken(t.Context(), "old", "attacker-new", testTTL, 0)

		if !errors.Is(err, ErrRefreshTokenDuplicateUse) {
			t.Fatalf("Rotate(old) after grace error = %v, want ErrRefreshTokenDuplicateUse", err)
		}
		// handler 的 Warn log 要知道是哪個 user：userID 帶在 error 裡
		var dupUse *RefreshTokenDuplicateUseError
		if !errors.As(err, &dupUse) || dupUse.UserID != userID {
			t.Errorf("Rotate(old) after grace error = %#v, want *RefreshTokenDuplicateUseError{UserID: %d}", err, userID)
		}
		// 撤銷有 Commit：忘了 Commit 的話 defer Rollback 會取消它，這幾個會還是 active
		for _, h := range []string{"new", "other-device"} {
			if !isRevoked(t, s, h) {
				t.Errorf("%s still active after duplicate use, want revoked", h)
			}
		}
		if exists(t, s, "attacker-new") {
			t.Errorf("attacker-new was stored, want not stored")
		}
		// 連帶撤銷的寫 duplicate_use_detected；被重送的 old 早就作廢，維持 rotated
		for h, want := range map[string]string{"old": "rotated", "new": "duplicate_use_detected", "other-device": "duplicate_use_detected"} {
			if got := revokedReason(t, s, h); got != want {
				t.Errorf("%s revoked_reason = %q, want %q", h, got, want)
			}
		}
		// WHERE user_id 沒寫錯：別人的 token 不受影響
		if isRevoked(t, s, "alice-token") {
			t.Errorf("other user's token revoked, want untouched")
		}
	})

	// logout 也會設 revoked_at：grace 只給 rotated，logout 掉的 token 馬上再出現就是 duplicate use
	t.Run("logged-out token within grace is duplicate use, not grace", func(t *testing.T) {
		s := newTestDBStore(t)
		userID := createTestUser(t, s, "jay")
		mustCreateToken(t, s, userID, "phone", testTTL)
		mustCreateToken(t, s, userID, "laptop", testTTL)
		if err := s.RevokeRefreshToken(t.Context(), "phone"); err != nil {
			t.Fatalf("setup Revoke(phone): %v", err)
		}

		_, err := s.RotateRefreshToken(t.Context(), "phone", "attacker-new", testTTL, testGrace)

		if !errors.Is(err, ErrRefreshTokenDuplicateUse) {
			t.Fatalf("Rotate(logged-out) error = %v, want ErrRefreshTokenDuplicateUse", err)
		}
		if exists(t, s, "attacker-new") {
			t.Errorf("attacker-new was stored, want not stored")
		}
		if !isRevoked(t, s, "laptop") {
			t.Errorf("laptop still active after duplicate use, want revoked")
		}
	})

	t.Run("unknown token is invalid", func(t *testing.T) {
		s := newTestDBStore(t)

		_, err := s.RotateRefreshToken(t.Context(), "nobody", "new", testTTL, testGrace)

		if !errors.Is(err, ErrRefreshTokenInvalid) {
			t.Errorf("Rotate(nobody) error = %v, want ErrRefreshTokenInvalid", err)
		}
	})

	// 過期但沒作廢：revoked_at 是 NULL → 沒有 COALESCE 的話 Scan 會失敗、變成 500
	t.Run("expired token is invalid", func(t *testing.T) {
		s := newTestDBStore(t)
		userID := createTestUser(t, s, "jay")
		// 負的 ttl：expires_at = now() - 1 分鐘
		mustCreateToken(t, s, userID, "old", -time.Minute)

		_, err := s.RotateRefreshToken(t.Context(), "old", "new", testTTL, testGrace)

		if !errors.Is(err, ErrRefreshTokenInvalid) {
			t.Errorf("Rotate(expired) error = %v, want ErrRefreshTokenInvalid", err)
		}
		if exists(t, s, "new") {
			t.Errorf("new token stored for expired old token")
		}
	})

	// expired 和 inGrace 同時成立：照規格 expired 優先
	t.Run("expired token within grace is still invalid", func(t *testing.T) {
		s := newTestDBStore(t)
		userID := createTestUser(t, s, "jay")
		mustCreateToken(t, s, userID, "old", testTTL)
		if _, err := s.dbPool.Exec(t.Context(),
			`UPDATE refresh_tokens SET revoked_at = now(), revoked_reason = 'rotated', expires_at = now() - interval '1 second' WHERE token_hash = $1`,
			"old"); err != nil {
			t.Fatalf("setup: %v", err)
		}

		_, err := s.RotateRefreshToken(t.Context(), "old", "new", testTTL, testGrace)

		if !errors.Is(err, ErrRefreshTokenInvalid) {
			t.Errorf("Rotate(expired, in grace) error = %v, want ErrRefreshTokenInvalid", err)
		}
	})

	// 兩個 request 同時拿同一個 token：r1 的 conditional UPDATE 只有一個搶得到（另一個會等 row lock），
	// 搶輸的那個在 grace 內 → 也成功。兩個都不能是 500 或 duplicate use
	t.Run("concurrent rotate of same token both succeed", func(t *testing.T) {
		s := newTestDBStore(t)
		userID := createTestUser(t, s, "jay")
		mustCreateToken(t, s, userID, "old", testTTL)

		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, newHash := range []string{"new-a", "new-b"} {
			wg.Go(func() {
				_, errs[i] = s.RotateRefreshToken(t.Context(), "old", newHash, testTTL, testGrace)
			})
		}
		wg.Wait()

		for i, err := range errs {
			if err != nil {
				t.Errorf("concurrent Rotate #%d error = %v, want nil", i, err)
			}
		}
		for _, h := range []string{"new-a", "new-b"} {
			if isRevoked(t, s, h) {
				t.Errorf("%s is revoked, want active", h)
			}
		}
	})
}

func TestRevokeRefreshToken(t *testing.T) {
	t.Run("revokes only that token", func(t *testing.T) {
		s := newTestDBStore(t)
		userID := createTestUser(t, s, "jay")
		mustCreateToken(t, s, userID, "phone", testTTL)
		mustCreateToken(t, s, userID, "laptop", testTTL)

		if err := s.RevokeRefreshToken(t.Context(), "phone"); err != nil {
			t.Fatalf("Revoke(phone) error = %v, want nil", err)
		}

		if !isRevoked(t, s, "phone") {
			t.Errorf("phone still active after revoke")
		}
		if isRevoked(t, s, "laptop") {
			t.Errorf("laptop revoked, want untouched")
		}
	})

	// idempotent：已作廢、查不到都不是錯
	t.Run("revoking twice or unknown token returns nil", func(t *testing.T) {
		s := newTestDBStore(t)
		userID := createTestUser(t, s, "jay")
		mustCreateToken(t, s, userID, "phone", testTTL)

		for _, h := range []string{"phone", "phone", "nobody"} {
			if err := s.RevokeRefreshToken(t.Context(), h); err != nil {
				t.Errorf("Revoke(%s) error = %v, want nil", h, err)
			}
		}
	})
}

func createTestUser(t *testing.T, s *DBStore, username string) int64 {
	t.Helper()
	id, err := s.CreateUser(t.Context(), username, "fake-hash")
	if err != nil {
		t.Fatalf("setup CreateUser(%q): %v", username, err)
	}
	return id
}

// 測試直接用看得懂的字串當 hash：store 不在乎 hash 長什麼樣
func mustCreateToken(t *testing.T, s *DBStore, userID int64, hash string, ttl time.Duration) {
	t.Helper()
	if err := s.CreateRefreshToken(t.Context(), userID, hash, ttl); err != nil {
		t.Fatalf("setup CreateRefreshToken(%q): %v", hash, err)
	}
}

func mustRotate(t *testing.T, s *DBStore, oldHash, newHash string, grace time.Duration) {
	t.Helper()
	if _, err := s.RotateRefreshToken(t.Context(), oldHash, newHash, testTTL, grace); err != nil {
		t.Fatalf("setup Rotate(%q → %q): %v", oldHash, newHash, err)
	}
}

func isRevoked(t *testing.T, s *DBStore, hash string) bool {
	t.Helper()
	var revoked bool
	if err := s.dbPool.QueryRow(t.Context(),
		`SELECT revoked_at IS NOT NULL FROM refresh_tokens WHERE token_hash = $1`, hash,
	).Scan(&revoked); err != nil {
		t.Fatalf("isRevoked(%q): %v", hash, err)
	}
	return revoked
}

func exists(t *testing.T, s *DBStore, hash string) bool {
	t.Helper()
	var n int
	if err := s.dbPool.QueryRow(t.Context(),
		`SELECT count(*) FROM refresh_tokens WHERE token_hash = $1`, hash,
	).Scan(&n); err != nil {
		t.Fatalf("exists(%q): %v", hash, err)
	}
	return n > 0
}

func revokedReason(t *testing.T, s *DBStore, hash string) string {
	t.Helper()
	var reason *string // NULL → nil（還沒作廢）
	if err := s.dbPool.QueryRow(t.Context(),
		`SELECT revoked_reason FROM refresh_tokens WHERE token_hash = $1`, hash,
	).Scan(&reason); err != nil {
		t.Fatalf("revokedReason(%q): %v", hash, err)
	}
	if reason == nil {
		return ""
	}
	return *reason
}
