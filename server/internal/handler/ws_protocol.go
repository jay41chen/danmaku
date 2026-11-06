package handler

import (
	"encoding/json"
	"strings"
)

// WS 上跑的訊息都包成 envelope：{"type": ..., "payload": ...}
// client 靠 type 分辨收到的是廣播的訊息還是錯誤，之後要加新種類（presence 等）也不用改格式
const (
	wsTypeMessage  = "message"
	wsTypeError    = "error"
	wsTypeAuth     = "auth"     // client → server：連線內登入 / 續期
	wsTypeAuthOK   = "auth_ok"  // server → client：登入 / 續期成功
	wsTypePing     = "ping"     // client → server：應用層心跳
	wsTypePong     = "pong"     // server → client：回應心跳
	wsTypeJoin     = "join"     // client → server：進入房間
	wsTypeJoinOK   = "join_ok"  // server → client：進入房間成功
	wsTypeLeave    = "leave"    // client → server：離開房間
	wsTypeLeaveOK  = "leave_ok" // server → client：離開房間成功
	wsTypePresence = "presence" // server → client：房間人數（每 5 秒推一次）
	wsTypeResync   = "resync"   // server → client：server 的 Redis 訂閱剛重建過，這段期間可能漏訊息，請重新撈歷史

	// 同時是 Redis room channel 上的 payload 種類：訊息之外的第二種。Redis 上的 JSON 開頭固定是 {"type":"user_hidden"（見 userHiddenPrefix）
	wsTypeUserHidden = "user_hidden" // server → client：這個 user 在這間房被 ban，請把畫面上他的彈幕移除（不會發給他本人的連線）

	wsCodeWriteForbidden  = "ws_write_forbidden"
	wsMsgWriteForbidden   = "請透過 POST /rooms/{public_id}/messages 發送"
	wsCodeAuthFailed      = "ws_auth_failed"
	wsMsgAuthFailed       = "登入驗證失敗，目前為訪客"
	wsCodeAuthExpired     = "ws_auth_expired"
	wsMsgAuthExpired      = "登入已過期，目前為訪客"
	wsCodeRoomNotFound    = "ws_room_not_found"
	wsMsgRoomNotFound     = "房間不存在"
	wsCodeJoinUnavailable = "ws_join_unavailable"
	wsMsgJoinUnavailable  = "暫時無法進入房間，請稍後再試"
	wsCodeAlreadyInRoom   = "ws_already_in_room"
	wsMsgAlreadyInRoom    = "已在房間內，請先離開"
	wsCodeNotInRoom       = "ws_not_in_room"
	wsMsgNotInRoom        = "目前不在任何房間"
)

type wsEnvelope struct {
	Type    string `json:"type"`
	Payload any    `json:"payload"`
}

// type = "auth" 的 payload（client → server）
type wsAuthPayload struct {
	Token string `json:"token"`
}

// type = "join"（client → server）以及 "join_ok" / "leave_ok"（server → client）的 payload：room 是房間的 public_id
type wsRoomPayload struct {
	Room string `json:"room"`
}

// type = "auth_ok" 的 payload：user 沿用 REST 的 userResponse，不用 loginResponse（WS 上沒有 token 要回）
// expires_in = 剩餘整數秒，跟 REST login 的 expires_in 同單位
type wsAuthOKPayload struct {
	User      userResponse `json:"user"`
	ExpiresIn int          `json:"expires_in"`
}

// type = "presence" 的 payload（server → client）：room 是房間的 public_id，count 是跨 server 去重後的在線人數
type wsPresencePayload struct {
	Room  string `json:"room"`
	Count int    `json:"count"`
}

// presence envelope 的 frame（給 presence worker 用，沒有 request ctx 所以不是 WebsocketHandler 的方法）
// 編碼失敗回傳 nil（Hub.Broadcast 收到 nil 會送出空 frame，所以呼叫端要自己檢查）；實務上這個固定結構不會失敗
func encodePresence(room string, count int) []byte {
	msg, err := json.Marshal(wsEnvelope{
		Type:    wsTypePresence,
		Payload: wsPresencePayload{Room: room, Count: count},
	})
	if err != nil {
		return nil
	}
	return msg
}

// resync envelope 的 frame（payload 沿用 wsRoomPayload）：編碼失敗回傳 nil，呼叫端不可把 nil 交給 Hub.Broadcast（理由同 encodePresence）
func encodeResync(room string) []byte {
	msg, err := json.Marshal(wsEnvelope{
		Type:    wsTypeResync,
		Payload: wsRoomPayload{Room: room},
	})
	if err != nil {
		return nil
	}
	return msg
}

// type = "user_hidden" 的 payload
type wsUserHiddenPayload struct {
	UserID int64 `json:"user_id"`
}

// user_hidden 的 JSON 開頭：Redis consumer 靠它分辨「不是訊息」的 payload（訊息的開頭是 {"type":"message"）
// 只比對前綴、不 unmarshal 每一則訊息：consumer 整台只有一個 goroutine，訊息量大時不該每則都 parse 一次
// 前綴由 json.Marshal(wsEnvelope{...}) 決定（欄位順序 type、payload），ws_golden_test 鎖住 frame 的完整內容
const userHiddenPrefix = `{"type":"user_hidden"`

// 組出 user_hidden 的 envelope：production（room_roles.go 經 publishEnvelope 發到 Redis）和 golden test 都用這一個，
// 這樣 golden test 鎖住的就是實際發出去的內容；序列化由 publishEnvelope 統一做（timeout / 失敗計數只有一份）
func userHiddenEnvelope(userID int64) wsEnvelope {
	return wsEnvelope{
		Type:    wsTypeUserHidden,
		Payload: wsUserHiddenPayload{UserID: userID},
	}
}

// 從 Redis 收到的 payload 判斷是不是 user_hidden；是的話回傳被隱藏的 user id
// 解析失敗（格式不對、user_id <= 0）回 ok = false，呼叫端當作一般訊息的話會被廣播給所有人 ——
// 所以前綴對得上但解析失敗的 payload，呼叫端要丟掉，不能 fall through（見 ws.go StartBroadcast）
func parseUserHidden(msg string) (userID int64, isUserHidden bool, valid bool) {
	if !strings.HasPrefix(msg, userHiddenPrefix) {
		return 0, false, false
	}
	var env struct {
		Payload wsUserHiddenPayload `json:"payload"`
	}
	if err := json.Unmarshal([]byte(msg), &env); err != nil || env.Payload.UserID <= 0 {
		return 0, true, false
	}
	return env.Payload.UserID, true, true
}
