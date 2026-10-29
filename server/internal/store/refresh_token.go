package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

var (
	// 查不到、或已過期：當作沒這個 token
	ErrRefreshTokenInvalid = errors.New("refresh token invalid")
	// 已作廢超過 grace period 又出現：視為外洩，這個 user 的 token 已全部撤銷
	// 業界叫 refresh token reuse detection（RFC 9700、Auth0）；這裡的 reuse 是「重複使用」，不是「復用」
	// RotateRefreshToken 實際回的是 *RefreshTokenDuplicateUseError；只想知道「是不是 duplicate use」用 errors.Is 比這個就好
	ErrRefreshTokenDuplicateUse = errors.New("refresh token used twice")
)

// 帶著 userID 的 duplicate use error（同 pgconn.PgError 的模式：error 本身帶資料，用 errors.As 取出）
// 不用「err 之外再回 userID」：Go 的 caller 預設 err != nil 時其他回傳值是零值，不會去看
type RefreshTokenDuplicateUseError struct {
	UserID int64 // 被撤銷全部 token 的 user
}

func (e *RefreshTokenDuplicateUseError) Error() string {
	return fmt.Sprintf("refresh token used twice, user %d", e.UserID)
}

// errors.Is 會呼叫這個：讓 errors.Is(err, ErrRefreshTokenDuplicateUse) 繼續成立
// 用 == 比是因為 sentinel error 比的是「同一個變數」（指標），不是訊息內容：
// 兩個 errors.New("refresh token used twice") 用 == 比是 false
func (e *RefreshTokenDuplicateUseError) Is(target error) bool {
	return target == ErrRefreshTokenDuplicateUse
}

// 時間一律用 DB 的 now()，不用 Go 的 time.Now()：
// 兩台 server 的時鐘可能差幾秒，DB 只有一個，「過期了沒」「作廢多久了」才有同一個基準
//
// db 只要求有 Exec：login 時傳 pool、rotate 時傳 tx
// （*pgxpool.Pool 和 pgx.Tx 都有 Exec，structural typing，不用宣告實作）
// 不強制要 pgx.Tx，因為單獨一句 INSERT 不在 transaction 裡跑也是對的；
// 在 rotate 裡傳 tx 才會跟作廢舊 token 一起生效
func createRefreshToken(ctx context.Context, db interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}, userID int64, tokenHash string, ttl time.Duration) error {
	_, err := db.Exec(ctx,
		`
			INSERT INTO refresh_tokens(user_id,token_hash,created_at ,expires_at )
			VALUES ($1, $2,now(), now() + $3)
		`, userID, tokenHash, ttl,
	)
	return err
}

// login 成功時呼叫：不在 transaction 裡，用 pool
func (this *DBStore) CreateRefreshToken(ctx context.Context, userID int64, tokenHash string, ttl time.Duration) error {
	return createRefreshToken(ctx, this.dbPool, userID, tokenHash, ttl)
}

// refresh token 在 /refresh 當下的狀態（classifyRefreshToken 的結果）
// iota：const 區塊裡從 0 開始自動 +1（= Kotlin enum 的 ordinal）
type tokenState int

const (
	tokenActive       tokenState = iota // 還沒作廢、還沒過期：r1 的 UPDATE 搶到了（已經作廢掉它）
	tokenInGrace                        // 作廢 ≤ grace：回應掉了、client 重送
	tokenDuplicateUse                   // 作廢 > grace：視為外洩
	tokenInvalid                        // 查不到 / 已過期
)

// 只負責「判斷」oldHash 現在是哪個狀態，不決定接下來要做什麼
// 唯一的寫入是 r1 的 conditional UPDATE：「搶到」本身就是判斷的一部分（兩個 request 同時來只有一個搶得到）
// 回傳的 userID 只有 tokenActive / tokenInGrace / tokenDuplicateUse 才有意義
// 參數是 pgx.Tx 不是 pool：r1 的 UPDATE 已經把舊 token 作廢，一定要跟後面「存新的」或「撤銷全部」在同一個 transaction 裡，讓 compiler 擋掉拿 pool 來呼叫
func classifyRefreshToken(ctx context.Context, tx pgx.Tx, oldHash string, grace time.Duration) (tokenState, int64, error) {
	var userID int64
	// r1：只有「還沒作廢、還沒過期」的才會被更新，RETURNING user_id
	err := tx.QueryRow(
		ctx,
		`
			UPDATE refresh_tokens
			SET revoked_at = now(), revoked_reason = 'rotated'
			WHERE token_hash = $1
				AND revoked_at IS NULL -- 還沒作廢
				AND expires_at > now() -- 還沒過期
			RETURNING user_id
		`, oldHash).Scan(&userID)
	if err == nil {
		// 更新成功，直接結束
		return tokenActive, userID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		// 如果是ErrNoRows => 往下繼續找
		// 不然就直接return error
		return tokenInvalid, 0, err // DB 壞了之類的：狀態不重要，caller 看到 err 就會直接 return
	}

	// r2：搶不到，查清楚是為什麼
	var expired, inGrace bool
	// SELECT 除了拿欄位，也能回傳算出來的結果（像 Kotlin 的 map）：比較式對每一列算一次 → true / false，AS 幫它取名
	// in_grace：「作廢的時間點比 grace 秒前還晚」→ 是 now() - $2，不是 +（+ 是未來，永遠不會成立）
	//   $2::interval 一定要寫：timestamptz - ? 有兩種可能（- interval → 時間、- timestamptz → interval），
	//   Postgres 猜成後者 → 拿時間跟 interval 比 → operator does not exist（createRefreshToken 的 now() + $3 只有一種可能，所以不用）
	//   只有 rotated 才算 grace：logout 掉的 token 15 秒內再出現，不能讓它換到新的
	// COALESCE(x, false)：x 是 NULL 就改用 false（= Kotlin 的 x ?: false）
	//   沒作廢過的 token revoked_at 是 NULL，NULL 跟任何東西比較結果都是 NULL（不是 false）
	//   Go 的 bool 裝不下 NULL → Scan 會回 error → 「過期但沒作廢」的 token 會變成 500 而不是 401
	// Scan 依「位置」對應 SELECT 的欄位順序，不是依名字
	err = tx.QueryRow(
		ctx,
		`
			SELECT user_id, expires_at <= now() AS expired, COALESCE(revoked_reason = 'rotated' AND revoked_at > now() - $2::interval, false) AS in_grace
			FROM refresh_tokens
			WHERE token_hash = $1
		`, oldHash, grace).Scan(&userID, &expired, &inGrace)
	// 順序就是優先權：先看 err（Scan 失敗時 expired / inGrace 只是零值，不能拿來判斷），
	// 再看 expired（過期又剛好在 grace 內 → 照規格算 invalid，不發新的）
	if errors.Is(err, pgx.ErrNoRows) {
		// 查不到是正常的狀態，不是 DB 錯誤：error 回 nil，交給 Rotate 的 switch 轉成 ErrRefreshTokenInvalid
		return tokenInvalid, 0, nil
	} else if err != nil {
		// 沒有這個分支的話：DB 斷線 → 一路掉到 else → 當成 duplicate use 把 user 全部登出
		return tokenInvalid, 0, err
	} else if expired {
		return tokenInvalid, 0, nil
	} else if inGrace {
		return tokenInGrace, userID, nil
	} else {
		// 找得到、沒過期、作廢超過 grace → 視為外洩
		return tokenDuplicateUse, userID, nil
	}
}

// /refresh：作廢 oldHash、存入 newHash，回傳 user id
//
//	有效             → 作廢、存新的                       → (userID, nil)
//	已作廢 ≤ grace   → 不動其他 token、存新的（回應掉了重送）→ (userID, nil)
//	已作廢 > grace   → 撤銷這個 user 全部 token            → *RefreshTokenDuplicateUseError（errors.Is ErrRefreshTokenDuplicateUse）
//	查不到 / 已過期   →                                      ErrRefreshTokenInvalid
func (this *DBStore) RotateRefreshToken(ctx context.Context, oldHash, newHash string, ttl, grace time.Duration) (int64, error) {
	tx, err := this.dbPool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	// 中途任何一個 return 都會 rollback；Commit 成功之後再 Rollback 不會做任何事
	// （回傳的 ErrTxClosed 直接忽略），所以不用每個 return 前都手動 rollback
	defer tx.Rollback(ctx)

	state, userID, err := classifyRefreshToken(ctx, tx, oldHash, grace)
	if err != nil {
		return 0, err
	}

	switch state {
	case tokenActive, tokenInGrace:
		// 存新的：跟 classifyRefreshToken 裡 r1 的 UPDATE 同一個 transaction
		if err := createRefreshToken(ctx, tx, userID, newHash, ttl); err != nil {
			return 0, err
		}
		// 作廢舊的 + 存新的一起生效
		if err := tx.Commit(ctx); err != nil {
			return 0, err
		}
		return userID, nil
	case tokenDuplicateUse:
		// 撤銷這個 user 全部還有效的 token
		_, err = tx.Exec(
			ctx,
			`
				UPDATE refresh_tokens
				SET revoked_at = now(), revoked_reason = 'duplicate_use_detected'
				WHERE user_id = $1
					AND revoked_at IS NULL
			`, userID)
		if err != nil {
			return 0, err
		}
		// 回傳的是 error，但撤銷一定要生效：不 Commit 的話 defer 的 Rollback 會把它取消
		if err := tx.Commit(ctx); err != nil {
			return 0, err
		}
		return 0, &RefreshTokenDuplicateUseError{UserID: userID}
	default:
		return 0, ErrRefreshTokenInvalid
	}
}

// /logout：只作廢這一個；查不到或已作廢都不算錯（logout 本來就該 idempotent）
// 而且這是user intent behavior，所以要特別記下revoked_reason
func (this *DBStore) RevokeRefreshToken(ctx context.Context, tokenHash string) error {
	_, err := this.dbPool.Exec(ctx,
		`
			UPDATE refresh_tokens
			SET revoked_at = now(), revoked_reason = 'logout'
			WHERE token_hash = $1
				AND revoked_at IS NULL
		`, tokenHash)
	if err != nil {
		return fmt.Errorf("revoke refresh token fail, %w", err)
	}
	return nil
}
