package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// 時間一律用 DB 的時間（now() / clock_timestamp()，差別見 revokeAllCredentials），不用 Go 的 time.Now()：
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

// refresh token 的狀態（設計工具：DB 沒有 state 欄位，狀態是從欄位 + 現在時間推出來的）
//
//	狀態                       怎麼判斷                                                  拿來 /refresh
//	expired                    expires_at <= now()                                       無效
//	撤銷全部之前發的             created_at <= user_credential_state.valid_after            無效，而且不再觸發撤銷
//	active                     revoked_at IS NULL                                        → rotated，發新的
//	rotated·grace內            revoked_reason = 'rotated' AND revoked_at > now() - grace  發新的（回應掉了重送、兩個分頁同時 refresh）
//	rotated·過grace            revoked_reason = 'rotated' AND revoked_at <= now() - grace 視為外洩 → 撤銷全部
//	logout                     revoked_reason = 'logout'                                  視為外洩 → 撤銷全部
//	duplicate_use_detected     舊資料（改用 valid_after 之前寫的），之後不會再寫入            視為外洩 → 撤銷全部
//	前兩個優先於後面全部
//
// 「撤銷之前發的」不再觸發撤銷：攻擊者手上通常留著一堆舊 token，如果每次都觸發，他可以一直把正牌使用者重新登入拿到的 token 撤掉
//
// 撤銷全部 = 只寫 valid_after，不改 refresh_tokens 的任何一列：
//
//	以前是 UPDATE「還能用的那些列」，要自己列舉哪些狀態算還能用 → 漏了 rotated·grace內：
//	  t0  攻擊者偷到 A，自己換成 B'，之後每 14 秒 rotate 一次並留著上一顆（手上永遠有一顆在 grace 內）
//	  t1  正牌使用者送 A → 偵測到外洩 → 撤銷全部，只撤到攻擊者最新那顆
//	  t2  攻擊者送上一顆 → 還在 grace 內 → 換到新的 7 天 token，之後不會再被偵測到
//	改成時間點：這之前發的一律無效，不管它在什麼狀態，就不會漏
//	代價：被撤銷的 token 那一列 revoked_at 還是 NULL，光看 refresh_tokens 會以為它有效；
//	要算「這個 user 還有哪些有效 token」一定要 JOIN user_credential_state
//
// 判斷順序（RotateRefreshToken，同一個 transaction）：
//
//	(a) 查 token 屬於哪個 user     不用鎖：refresh_tokens.user_id 寫入後永遠不變
//	(b) lockUser                   同一個 user 的 refresh / 撤銷全部在這裡排隊
//	(c0) 被 valid_after 撤銷了嗎 → (c) classifyRefreshToken → (d) 發新的或撤銷全部
//	(c0)(c) 一定要在鎖之後（TOCTOU：time-of-check to time-of-use）：
//	  t0  tx R 檢查 token：撤銷還沒 commit，看不到 valid_after → 有效
//	  t1  R 等鎖（撤銷全部的 tx 拿著）
//	  t2  撤銷 commit、放鎖
//	  t3  R 拿到鎖，照 t0 的判斷發新 token → 撤銷之後還換得到
//	  鎖只保證同時只有一個人在改，不會讓鎖之前讀到的資料變新；鎖之前只能讀不會變的資料（user_id）
//
// Known issue 1：登入（CreateRefreshToken）不明確拿鎖，但 INSERT refresh_tokens 的 FK 檢查會對 users 那一列
// 拿 FOR KEY SHARE，跟 lockUser 的 FOR UPDATE 衝突 → 撤銷的 tx 拿著鎖時開始的登入會排隊，等撤銷 commit 才寫入；
// 觸發條件是「撤銷的 tx 拿到 user 鎖之後、寫入 valid_after 之前開始的登入」：
// created_at 是 now()（排隊之前），早於 valid_after（撤銷 tx 執行 INSERT 當下的 clock_timestamp()）
// → 剛發的 token 立刻失效，要再登入一次。
// 在 valid_after 寫入之後、撤銷 commit 之前開始的登入也會排隊，但 created_at 晚於 valid_after，token 有效。
// 很少見、代價小，接受
//
// Known issue 2（極罕見，效果同樣是誤殺不是繞過）：rotate 發出的新 token，created_at 也是 now()（tx 開始、拿鎖之前）：
//
//	R BEGIN → 撤銷 D commit → 別處登入發出 L → R 讀到 L、拿鎖、c0 判斷沒撤銷
//	→ 發出 created_at < valid_after 的新 token → 發出就失效
//
// 下面的 tokenState 是 classifyRefreshToken「這次判斷出來的結果」，不是上面的狀態：
// active → tokenActive、rotated·grace內 → tokenInGrace、rotated·過grace / logout / duplicate_use_detected → tokenDuplicateUse、
// 查不到 / expired → tokenInvalid（撤銷全部之前發的，在 classify 之前的 c0 就擋掉了）
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

// 撤銷這個 user 全部的 refresh token：只寫 valid_after，不改 refresh_tokens 的任何一列
// （為什麼不 UPDATE 那些列、狀態表與 TOCTOU 見 tokenState 上方的說明）
//
// 參數是 pgx.Tx：呼叫端要先 lockUser，而且寫入要跟呼叫端的 transaction 一起 Commit
// 之後 Ban 也可以重用
//
// 一定要用 clock_timestamp()，不能用 now()：Postgres 的 now() 是 transaction「開始」的時間，
// clock_timestamp() 是執行這一行的當下
//
//	t0  tx D 開始（now() = T1），查 user_id，等鎖
//	t1  tx R（攻擊者的 refresh）比 D 早拿到鎖，發新 token，created_at = T2（T2 > T1），commit
//	t2  D 拿到鎖 → duplicate use → 如果 valid_after = now() = T1 → R 的新 token（T2 > T1）漏網
//
// 用 clock_timestamp()：寫入時已經拿著鎖，之前 commit 的 token created_at 一定比它早
func revokeAllCredentials(ctx context.Context, tx pgx.Tx, userID int64) error {
	_, err := tx.Exec(ctx,
		`
			INSERT INTO user_credential_state(user_id, valid_after)
			VALUES ($1, clock_timestamp())
			ON CONFLICT (user_id) DO UPDATE SET valid_after = EXCLUDED.valid_after
		`, userID)
	return err
}

// c0：這顆 token 是不是在「撤銷全部」之前發的（created_at <= valid_after）
// 一定要在 lockUser 之後呼叫（TOCTOU，見 tokenState 上方的說明）
// LEFT JOIN：沒有 user_credential_state 那一列 = 從來沒被撤銷過，cs.valid_after 是 NULL，
// NULL 的比較結果是 NULL，COALESCE 轉成 false
// 查不到 token 回 pgx.ErrNoRows（呼叫端在 (a) 已經確認過存在）
func revokedByCredentialState(ctx context.Context, tx pgx.Tx, oldHash string) (bool, error) {
	var revoked bool
	err := tx.QueryRow(ctx,
		`
			SELECT COALESCE(rt.created_at <= cs.valid_after, false)
			FROM refresh_tokens rt
			LEFT JOIN user_credential_state cs ON cs.user_id = rt.user_id
			WHERE rt.token_hash = $1
		`, oldHash).Scan(&revoked)
	return revoked, err
}

// /refresh：作廢 oldHash、存入 newHash，回傳 user id
// 狀態與判斷順序的說明見 tokenState 上方
//
//	有效             → 作廢、存新的                       → (userID, nil)
//	已作廢 ≤ grace   → 不動其他 token、存新的（回應掉了重送）→ (userID, nil)
//	已作廢 > grace   → 寫入 valid_after（撤銷全部）         → *RefreshTokenDuplicateUseError（errors.Is ErrRefreshTokenDuplicateUse）
//	被 valid_after 撤銷 → 不撤銷、不寫任何東西                → *RefreshTokenRevokedError（errors.Is ErrRefreshTokenInvalid）
//	查不到 / 已過期   →                                      ErrRefreshTokenInvalid
func (this *DBStore) RotateRefreshToken(ctx context.Context, oldHash, newHash string, ttl, grace time.Duration) (int64, error) {
	tx, err := this.dbPool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	// 中途任何一個 return 都會 rollback；Commit 成功之後再 Rollback 不會做任何事
	// （回傳的 ErrTxClosed 直接忽略），所以不用每個 return 前都手動 rollback
	defer tx.Rollback(ctx)

	// (a) 查 token 屬於哪個 user：不用鎖，user_id 寫入後永遠不變
	var userID int64
	err = tx.QueryRow(ctx,
		`SELECT user_id FROM refresh_tokens WHERE token_hash = $1`, oldHash).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, ErrRefreshTokenInvalid
	} else if err != nil {
		return 0, err
	}

	// (b) 同一個 user 的 refresh / 撤銷全部在這裡排隊
	if _, err := lockUser(ctx, tx, userID); errors.Is(err, ErrUserNotFound) {
		return 0, ErrRefreshTokenInvalid
	} else if err != nil {
		return 0, err
	}

	// (c0) 拿到鎖「之後」才查：鎖之前看到的 valid_after 可能已經過時
	revoked, err := revokedByCredentialState(ctx, tx, oldHash)
	if err != nil {
		return 0, err
	}
	if revoked {
		// 撤銷之前發的 token 不再觸發撤銷（見 tokenState 上方），但回帶 userID 的 error 讓 handler 記 log
		return 0, &RefreshTokenRevokedError{UserID: userID}
	}

	// (c) 判斷狀態；這裡的 userID 與 (a) 的相同，沿用 classify 的回傳
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
		// 撤銷這個 user 全部的 token：已經拿著 (b) 的鎖
		if err := revokeAllCredentials(ctx, tx, userID); err != nil {
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
