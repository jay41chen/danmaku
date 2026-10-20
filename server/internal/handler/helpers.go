package handler

import (
	"encoding/json"
	"net/http"
	"slices"
)

// handler package 共用的 HTTP helper（各個 Handler 都會用）

func setContentType(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
}

func checkMethodValidation(r *http.Request, allowMethods []string) bool {
	return slices.Contains(allowMethods, r.Method)
}

// 限制 body 大小（1 MB）再 decode JSON 到 dst；dst 要傳 pointer
// 包在一起是為了不會忘記限制：直接寫 json.NewDecoder(r.Body) 的 handler 就是漏了
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	return json.NewDecoder(r.Body).Decode(dst)
}
