package pgstore

import (
	"context"
	"errors"

	"github.com/jay41chen/danmaku/server/internal/model"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// 存訊息並回傳完整的 Message（含 id、username、created_at），handler 直接拿去廣播 / 回給 client
// 房間用 public_id 指定：「房間存在且未刪除」和 INSERT 合成同一句 statement（INSERT ... SELECT FROM rooms）
// 第二個回傳值 = 作者此刻在這間房是否被 ban（同一句 statement 查，不多一次來回）：
// 被 ban 的訊息照常存，只是 handler 不廣播（shadow ban）
func (this *DBStore) CreateMessage(ctx context.Context, roomPublicID string, userID int64, content string) (model.Message, bool, error) {
	var msg model.Message
	var banned bool
	// 一個 statement 同時 INSERT 和 JOIN users 取 username，不用兩次來回
	err := this.dbPool.QueryRow(ctx,
		`
			WITH m AS (
				INSERT INTO messages(room_id, user_id, content)
				SELECT id, $2, $3 FROM rooms WHERE public_id = $1 AND deleted_at IS NULL
				RETURNING id, room_id, user_id, content, created_at
			)
			SELECT m.id, m.user_id, u.username, m.content, m.created_at,
				EXISTS (
					SELECT 1 FROM room_roles r
					WHERE r.room_id = m.room_id AND r.user_id = m.user_id AND r.role = 'banned'
				)
			FROM m
			JOIN users u ON m.user_id = u.id
		`, roomPublicID, userID, content,
	).Scan(&msg.ID, &msg.UserID, &msg.Username, &msg.Content, &msg.CreatedAt, &banned)
	if err != nil {
		// 同 insertRoom 的手法：房間不存在或已 soft delete → SELECT 回 0 列 → 插入 0 列 → RETURNING 也是空 → ErrNoRows
		// 檢查跟插入在同一句裡，「查房間」和「插入」之間沒有空隙，而且省一次來回（熱門房間約 50 msgs/s）
		// user 不存在時 FK 會先失敗（23503），到不了 JOIN，所以這裡的 ErrNoRows 只可能是房間不存在
		// 房間跟 user 都不存在時回 model.ErrRoomNotFound：插入 0 列，根本不會觸發 FK 檢查（handler 會回 404 而不是 401）
		if errors.Is(err, pgx.ErrNoRows) {
			return model.Message{}, false, model.ErrRoomNotFound
		}
		var pgErr *pgconn.PgError
		// 23503 = foreign_key_violation：user_id 指向的 user 不存在（已被刪除）
		// messages 現在有兩個 FK（user_id、room_id），但這裡的 23503 只可能是 user 那個：
		// room_id 來自 SELECT（那一刻列一定存在）、房間只做 soft delete、ON DELETE RESTRICT 也擋住「刪有訊息的房間」
		// 之後若加了 hard delete，要用 pgErr.ConstraintName 區分
		if errors.As(err, &pgErr) && pgErr.Code == "23503" {
			return model.Message{}, false, model.ErrMessageUserNotFound
		}
		return model.Message{}, false, err
	}
	// 已接受的 race：Read Committed 下，若 owner 的 soft delete 在這句 statement 的 snapshot 之後才 commit，
	// 訊息仍會寫進剛被刪除的房間。可以接受，因為已刪除的房間不會再被讀取；
	// 要避免就得每則訊息都加鎖或提高 isolation level，不划算
	return msg, banned, nil
}

// roomID 是房間的內部 id：handler 先用 GetRoomByPublicID 換到 id（同時負責回 404），這裡不再檢查房間狀態
// 被 ban 的作者的訊息不回傳（NOT EXISTS 在讀取時過濾，訊息本身不標記；解除 ban 後全部回來）
// 一定要 NOT EXISTS：NOT IN / <> ALL 遇到 NULL（user_id 為 NULL 的舊訊息、已刪除的發送者）會變成 unknown，整批訊息消失
// 過濾在 LIMIT 之前，所以仍會回傳最多 limit 則「看得到的」訊息
// 索引 (room_id, id) 讓 WHERE room_id ... ORDER BY id DESC LIMIT 直接走索引
func (this *DBStore) QueryMessage(ctx context.Context, roomID int64, limit int) ([]model.Message, error) {
	// LEFT JOIN：舊資料和發送者已被刪除的訊息 user_id 是 NULL，INNER JOIN 會把這些列整個丟掉
	// 所以 Message.Username / UserID 是指標：NULL 掃不進 string / int64，只能掃進 *string / *int64
	rows, err := this.dbPool.Query(ctx,
		`
			SELECT m.id, m.user_id, u.username, m.content, m.created_at
			FROM messages m
			LEFT JOIN users u ON u.id = m.user_id
			WHERE m.room_id = $1
				AND NOT EXISTS (
					SELECT 1 FROM room_roles r
					WHERE r.room_id = m.room_id AND r.user_id = m.user_id AND r.role = 'banned'
				)
			ORDER BY m.id DESC
			LIMIT $2
		`, roomID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	msgs := []model.Message{}
	for rows.Next() {
		var msg model.Message
		if err := rows.Scan(&msg.ID, &msg.UserID, &msg.Username, &msg.Content, &msg.CreatedAt); err != nil {
			return nil, err
		}
		msgs = append(msgs, msg)
	}

	// rows.Next() 讀完跟讀到一半出錯（例如連線斷掉）都回 false，迴圈分不出來
	// pgx 把中途的錯誤存在 rows 裡，只能在迴圈結束後用 rows.Err() 問；放在迴圈前一定是 nil
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return msgs, nil
}
