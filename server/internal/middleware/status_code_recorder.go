package middleware

import (
	"bufio"
	"errors"
	"net"
	"net/http"
)

// StatusCodeRecorder 包住 ResponseWriter，記下 client「實際收到」的 status code，
// 給 Logging / Metrics 使用。
//
// 原則：只觀察、不改變行為。拿掉 recorder 時 net/http 的行為要完全一樣。
//
// HTTP 規則：status 一定在 body 之前送出，送出後就不能改。常見的呼叫順序：
//
//	200 成功           只有 Write（第一次 Write 時 net/http 自動補上 200）
//	非 200（http.Error）WriteHeader(code) → Write
//	204 No Content     只有 WriteHeader(204)，沒有 body
//	什麼都沒寫         handler return 時 net/http 自動送出 200
//	WebSocket          Hijack → 記成 101
//	Write → WriteHeader 違反規則（panic 或 bug），client 收到 200，只記第一次
//
// 注意：embed 是「轉發」不是「繼承」（類似 Kotlin 的 `: Writer by w`）。
// 內層 ResponseWriter 呼叫自己的 method 時不會經過 recorder，
// 所以凡是會影響 status 的 method（WriteHeader、Write、Hijack）都要 override。
type StatusCodeRecorder struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool // header 是否已送出；送出後 status 就不會再變
}

// constructor
func NewStatusCodeRecorder(w http.ResponseWriter) *StatusCodeRecorder {
	return &StatusCodeRecorder{ResponseWriter: w, statusCode: 200}
}

func (r *StatusCodeRecorder) StatusCode() int {
	return r.statusCode
}

// 還沒 WriteHeader 就直接 Write 時，內層會「自己」先送出 200，不會經過 recorder 的 WriteHeader。
// 所以要在這裡標記 header 已送出（statusCode 預設就是 200，不用改），
// 否則之後的 WriteHeader(500) 會被誤記成 500。
//
//	w.Write(body)       → 內層隱含送出 200 → 這裡標記 wroteHeader
//	w.WriteHeader(500)  → 已送出，不記錄 → StatusCode() 仍是 200
func (r *StatusCodeRecorder) Write(b []byte) (int, error) {
	if !r.wroteHeader {
		r.wroteHeader = true
	}
	return r.ResponseWriter.Write(b)
}

// HTTP status 只有第一次 WriteHeader 生效，所以只記第一次。
// 但每次都要轉發給內層
func (r *StatusCodeRecorder) WriteHeader(statusCode int) {
	if !r.wroteHeader {
		r.statusCode = statusCode
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(statusCode)
}

// WebSocket upgrade 需要 Hijack 取得底層 TCP 連線
// 所以需要override這個method，否則upgrade的request會failed
func (r *StatusCodeRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("underlying ResponseWriter does not support hijacking")
	}
	conn, rw, err := hijacker.Hijack()
	if err == nil {
		r.statusCode = http.StatusSwitchingProtocols
	}
	return conn, rw, err
}

func (r *StatusCodeRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
