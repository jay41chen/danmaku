package handler

import "encoding/json"

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

// type = "error" 的 payload：code 給程式判斷，message 給人看
type wsError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
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
