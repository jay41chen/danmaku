package pgstore

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jay41chen/danmaku/server/internal/model"
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

		if !errors.Is(err, model.ErrRefreshTokenDuplicateUse) {
			t.Fatalf("Rotate(old) after grace error = %v, want model.ErrRefreshTokenDuplicateUse", err)
		}
		// handler 的 Warn log 要知道是哪個 user：userID 帶在 error 裡
		var dupUse *model.RefreshTokenDuplicateUseError
		if !errors.As(err, &dupUse) || dupUse.UserID != userID {
			t.Errorf("Rotate(old) after grace error = %#v, want *model.RefreshTokenDuplicateUseError{UserID: %d}", err, userID)
		}
		if exists(t, s, "attacker-new") {
			t.Errorf("attacker-new was stored, want not stored")
		}
		// 撤銷有 Commit：忘了 Commit 的話 defer Rollback 會取消 valid_after，這兩顆還能換
		for _, h := range []string{"new", "other-device"} {
			_, err := s.RotateRefreshToken(t.Context(), h, h+"-next", testTTL, testGrace)
			if !errors.Is(err, model.ErrRefreshTokenInvalid) {
				t.Errorf("Rotate(%s) after revoke all error = %v, want model.ErrRefreshTokenInvalid", h, err)
			}
		}
		// 撤銷全部只寫 valid_after，不改 refresh_tokens（見 tokenState 上方說明）
		if got := revokedReason(t, s, "old"); got != "rotated" {
			t.Errorf("revoked_reason(old) = %q, want %q", got, "rotated")
		}
		for _, h := range []string{"new", "other-device"} {
			if got := revokedReason(t, s, h); got != "" {
				t.Errorf("revoked_reason(%s) = %q, want empty (untouched)", h, got)
			}
		}
		// valid_after 只寫這個 user 的列：別人的 token 不受影響
		if !hasCredentialState(t, s, userID) {
			t.Errorf("user_credential_state has no row for user, want one")
		}
		if hasCredentialState(t, s, otherID) {
			t.Errorf("user_credential_state has a row for other user, want none")
		}
		if _, err := s.RotateRefreshToken(t.Context(), "alice-token", "alice-new", testTTL, testGrace); err != nil {
			t.Errorf("other user's Rotate(alice-token) error = %v, want nil", err)
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

		if !errors.Is(err, model.ErrRefreshTokenDuplicateUse) {
			t.Fatalf("Rotate(logged-out) error = %v, want model.ErrRefreshTokenDuplicateUse", err)
		}
		if exists(t, s, "attacker-new") {
			t.Errorf("attacker-new was stored, want not stored")
		}
		if _, err := s.RotateRefreshToken(t.Context(), "laptop", "laptop-next", testTTL, testGrace); !errors.Is(err, model.ErrRefreshTokenInvalid) {
			t.Errorf("Rotate(laptop) after revoke all error = %v, want model.ErrRefreshTokenInvalid", err)
		}
	})

	// Judge 重現的洞：撤銷全部之後，grace 內的 token 不能再換出新的
	t.Run("in-grace token cannot refresh after revoke all", func(t *testing.T) {
		s := newTestDBStore(t)
		userID := createTestUser(t, s, "jay")
		mustCreateToken(t, s, userID, "t0", testTTL)
		mustRotate(t, s, "t0", "t1", testGrace)
		// t0 的作廢時間往前調 1 分鐘：超過 testGrace，不用 sleep
		backdateRevokedAt(t, s, "t0", time.Minute)
		mustRotate(t, s, "t1", "t2", testGrace) // t1 現在 revoked_at = now()，在 grace 內

		if _, err := s.RotateRefreshToken(t.Context(), "t0", "x", testTTL, testGrace); !errors.Is(err, model.ErrRefreshTokenDuplicateUse) {
			t.Fatalf("Rotate(t0) error = %v, want model.ErrRefreshTokenDuplicateUse", err)
		}

		_, err := s.RotateRefreshToken(t.Context(), "t1", "t3", testTTL, testGrace)
		if !errors.Is(err, model.ErrRefreshTokenInvalid) {
			t.Errorf("Rotate(t1) after revoke all error = %v, want model.ErrRefreshTokenInvalid", err)
		}
		if exists(t, s, "t3") {
			t.Errorf("t3 was stored, want not stored")
		}
	})

	t.Run("token issued before revoke does not trigger revoke again", func(t *testing.T) {
		s := newTestDBStore(t)
		userID := createTestUser(t, s, "jay")
		mustCreateToken(t, s, userID, "t0", testTTL)
		mustRotate(t, s, "t0", "t1", testGrace)
		backdateRevokedAt(t, s, "t0", time.Minute)
		if _, err := s.RotateRefreshToken(t.Context(), "t0", "x", testTTL, testGrace); !errors.Is(err, model.ErrRefreshTokenDuplicateUse) {
			t.Fatalf("setup Rotate(t0) error = %v, want model.ErrRefreshTokenDuplicateUse", err)
		}
		before := validAfter(t, s, userID)

		_, err := s.RotateRefreshToken(t.Context(), "t0", "y", testTTL, testGrace)

		if !errors.Is(err, model.ErrRefreshTokenInvalid) || errors.Is(err, model.ErrRefreshTokenDuplicateUse) {
			t.Errorf("Rotate(t0) again error = %v, want model.ErrRefreshTokenInvalid (not duplicate use)", err)
		}
		if after := validAfter(t, s, userID); !after.Equal(before) {
			t.Errorf("valid_after changed %v → %v, want unchanged", before, after)
		}
		// handler 的 Warn log 要知道是哪個 user
		var revoked *model.RefreshTokenRevokedError
		if !errors.As(err, &revoked) || revoked.UserID != userID {
			t.Errorf("Rotate(t0) again error = %#v, want *model.RefreshTokenRevokedError{UserID: %d}", err, userID)
		}
	})

	// 第二次撤銷要把 valid_after 往後推：ON CONFLICT 若是 DO NOTHING，重新登入後的 token 會永遠留在舊的 valid_after 之後
	t.Run("second revoke moves valid_after forward", func(t *testing.T) {
		s := newTestDBStore(t)
		userID := createTestUser(t, s, "jay")
		mustCreateToken(t, s, userID, "a0", testTTL)
		mustRotate(t, s, "a0", "a1", testGrace)
		backdateRevokedAt(t, s, "a0", time.Minute)
		if _, err := s.RotateRefreshToken(t.Context(), "a0", "x", testTTL, testGrace); !errors.Is(err, model.ErrRefreshTokenDuplicateUse) {
			t.Fatalf("setup first revoke error = %v, want model.ErrRefreshTokenDuplicateUse", err)
		}
		v1 := validAfter(t, s, userID)

		// 重新登入，之後同樣的外洩再發生一次
		mustCreateToken(t, s, userID, "b0", testTTL)
		mustRotate(t, s, "b0", "b1", testGrace)
		backdateRevokedAt(t, s, "b0", time.Minute)
		if _, err := s.RotateRefreshToken(t.Context(), "b0", "y", testTTL, testGrace); !errors.Is(err, model.ErrRefreshTokenDuplicateUse) {
			t.Fatalf("second revoke error = %v, want model.ErrRefreshTokenDuplicateUse", err)
		}

		if v2 := validAfter(t, s, userID); !v2.After(v1) {
			t.Errorf("valid_after = %v after second revoke, want later than %v", v2, v1)
		}
		_, err := s.RotateRefreshToken(t.Context(), "b1", "b2", testTTL, testGrace)
		if !errors.Is(err, model.ErrRefreshTokenInvalid) {
			t.Errorf("Rotate(b1) after second revoke error = %v, want model.ErrRefreshTokenInvalid", err)
		}
		if exists(t, s, "b2") {
			t.Errorf("b2 was stored, want not stored")
		}
	})

	t.Run("login after revoke works", func(t *testing.T) {
		s := newTestDBStore(t)
		userID := createTestUser(t, s, "jay")
		mustCreateToken(t, s, userID, "t0", testTTL)
		mustRotate(t, s, "t0", "t1", testGrace)
		backdateRevokedAt(t, s, "t0", time.Minute)
		if _, err := s.RotateRefreshToken(t.Context(), "t0", "x", testTTL, testGrace); !errors.Is(err, model.ErrRefreshTokenDuplicateUse) {
			t.Fatalf("setup Rotate(t0) error = %v, want model.ErrRefreshTokenDuplicateUse", err)
		}

		mustCreateToken(t, s, userID, "relogin", testTTL) // 模擬重新登入

		if _, err := s.RotateRefreshToken(t.Context(), "relogin", "relogin-next", testTTL, testGrace); err != nil {
			t.Errorf("Rotate(relogin) error = %v, want nil", err)
		}
	})

	// TOCTOU：Rotate 在等 user 鎖的時候，撤銷全部 commit 了；拿到鎖之後一定要重新檢查
	t.Run("checks happen after user lock (TOCTOU)", func(t *testing.T) {
		s := newTestDBStore(t)
		userID := createTestUser(t, s, "jay")
		mustCreateToken(t, s, userID, "active", testTTL)

		tx, err := s.dbPool.Begin(t.Context())
		if err != nil {
			t.Fatalf("setup Begin: %v", err)
		}
		defer tx.Rollback(t.Context()) // 測試中途失敗也要放鎖，不然後面的 Rotate goroutine 會一直卡著
		if _, err := tx.Exec(t.Context(), `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, userID); err != nil {
			t.Fatalf("setup lock user: %v", err)
		}

		done := make(chan error, 1)
		go func() {
			_, err := s.RotateRefreshToken(t.Context(), "active", "new", testTTL, testGrace)
			done <- err
		}()

		select {
		case err := <-done:
			t.Fatalf("Rotate returned %v while user lock was held, want blocked", err)
		case <-time.After(100 * time.Millisecond):
		}

		if _, err := tx.Exec(t.Context(),
			`INSERT INTO user_credential_state(user_id, valid_after) VALUES ($1, clock_timestamp())`, userID); err != nil {
			t.Fatalf("setup revoke all: %v", err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatalf("setup Commit: %v", err)
		}

		select {
		case err := <-done:
			if !errors.Is(err, model.ErrRefreshTokenInvalid) {
				t.Errorf("Rotate error = %v, want model.ErrRefreshTokenInvalid", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Rotate still blocked 2s after lock released")
		}
		if exists(t, s, "new") {
			t.Errorf("new token stored after revoke all, want not stored")
		}
	})

	// now() 是 transaction 開始的時間：用它的話 valid_after 會早於 transaction 中途建立的 token
	t.Run("revokeAllCredentials uses statement time, not transaction start", func(t *testing.T) {
		s := newTestDBStore(t)
		userID := createTestUser(t, s, "jay")

		tx, err := s.dbPool.Begin(t.Context())
		if err != nil {
			t.Fatalf("setup Begin: %v", err)
		}
		defer tx.Rollback(t.Context())
		var txNow time.Time
		if err := tx.QueryRow(t.Context(), `SELECT now()`).Scan(&txNow); err != nil { // 確保 transaction 時間已經定下來
			t.Fatalf("setup SELECT now(): %v", err)
		}
		time.Sleep(50 * time.Millisecond)
		mustCreateToken(t, s, userID, "later", testTTL) // 用 pool：created_at 晚於 txNow

		if err := revokeAllCredentials(t.Context(), tx, userID); err != nil {
			t.Fatalf("revokeAllCredentials: %v", err)
		}
		if err := tx.Commit(t.Context()); err != nil {
			t.Fatalf("setup Commit: %v", err)
		}

		_, err = s.RotateRefreshToken(t.Context(), "later", "new", testTTL, testGrace)
		if !errors.Is(err, model.ErrRefreshTokenInvalid) {
			t.Errorf("Rotate(later) error = %v, want model.ErrRefreshTokenInvalid", err)
		}
	})

	t.Run("unknown token is invalid", func(t *testing.T) {
		s := newTestDBStore(t)

		_, err := s.RotateRefreshToken(t.Context(), "nobody", "new", testTTL, testGrace)

		if !errors.Is(err, model.ErrRefreshTokenInvalid) {
			t.Errorf("Rotate(nobody) error = %v, want model.ErrRefreshTokenInvalid", err)
		}
	})

	// 過期但沒作廢：revoked_at 是 NULL → 沒有 COALESCE 的話 Scan 會失敗、變成 500
	t.Run("expired token is invalid", func(t *testing.T) {
		s := newTestDBStore(t)
		userID := createTestUser(t, s, "jay")
		// 負的 ttl：expires_at = now() - 1 分鐘
		mustCreateToken(t, s, userID, "old", -time.Minute)

		_, err := s.RotateRefreshToken(t.Context(), "old", "new", testTTL, testGrace)

		if !errors.Is(err, model.ErrRefreshTokenInvalid) {
			t.Errorf("Rotate(expired) error = %v, want model.ErrRefreshTokenInvalid", err)
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

		if !errors.Is(err, model.ErrRefreshTokenInvalid) {
			t.Errorf("Rotate(expired, in grace) error = %v, want model.ErrRefreshTokenInvalid", err)
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

// 把 token 的 revoked_at 往前調：模擬「已經作廢超過 grace」，不用 sleep
func backdateRevokedAt(t *testing.T, s *DBStore, hash string, d time.Duration) {
	t.Helper()
	if _, err := s.dbPool.Exec(t.Context(),
		`UPDATE refresh_tokens SET revoked_at = revoked_at - $2::interval WHERE token_hash = $1`, hash, d,
	); err != nil {
		t.Fatalf("backdateRevokedAt(%q): %v", hash, err)
	}
}

func hasCredentialState(t *testing.T, s *DBStore, userID int64) bool {
	t.Helper()
	var n int
	if err := s.dbPool.QueryRow(t.Context(),
		`SELECT count(*) FROM user_credential_state WHERE user_id = $1`, userID,
	).Scan(&n); err != nil {
		t.Fatalf("hasCredentialState(%d): %v", userID, err)
	}
	return n > 0
}

func validAfter(t *testing.T, s *DBStore, userID int64) time.Time {
	t.Helper()
	var v time.Time
	if err := s.dbPool.QueryRow(t.Context(),
		`SELECT valid_after FROM user_credential_state WHERE user_id = $1`, userID,
	).Scan(&v); err != nil {
		t.Fatalf("validAfter(%d): %v", userID, err)
	}
	return v
}
