package main

import (
	"strings"
	"testing"
)

func TestCheckServerURL(t *testing.T) {
	good := map[string]string{
		"http://localhost:6388":    "http://localhost:6388",
		"http://127.0.0.1:6389":    "http://127.0.0.1:6389",
		"http://127.0.0.1:6389/":   "http://127.0.0.1:6389",
		"https://localhost":        "https://localhost",
		" http://localhost:8080  ": "http://localhost:8080",
	}
	for in, want := range good {
		got, err := checkServerURL(in)
		if err != nil || got != want {
			t.Errorf("checkServerURL(%q) = %q, %v; want %q", in, got, err, want)
		}
	}

	bad := []string{
		"http://example.com",
		"http://192.168.1.10:8080",
		"http://0.0.0.0:8080",
		"http://localhost.evil.com",     // 前綴相同但是別人的 host
		"http://evil.com/localhost",     // localhost 只在 path
		"http://localhost@evil.com",     // 帳密欺騙：真正的 host 是 evil.com
		"http://user:pw@localhost:8080", // 帶帳密一律拒絕
		"http://[::1]:8080",             // 只准 localhost / 127.0.0.1
		"ftp://localhost",
		"localhost:8080",
		"http://localhost:8080/rooms",
		"",
	}
	for _, in := range bad {
		if got, err := checkServerURL(in); err == nil {
			t.Errorf("checkServerURL(%q) = %q, want error", in, got)
		}
	}

	// 其中一個不合格就整批拒絕，不能只過濾掉壞的
	if _, err := checkServerURLs([]string{"http://localhost:1", "http://example.com"}); err == nil {
		t.Error("checkServerURLs accepted a list containing a non-local server")
	}
	if _, err := checkServerURLs(nil); err == nil {
		t.Error("checkServerURLs accepted an empty list")
	}
}

func TestCheckDatabaseURL(t *testing.T) {
	good := []string{
		"postgres://danmaku:danmaku@localhost:3192/danmaku_test?sslmode=disable",
		"postgres://u:p@127.0.0.1:5432/danmaku_test",
		"postgresql://u:p@localhost/danmaku_test",
	}
	for _, in := range good {
		if _, err := checkDatabaseURL(in); err != nil {
			t.Errorf("checkDatabaseURL(%q) = %v, want ok", in, err)
		}
	}
	pw, _ := checkDatabaseURL("postgres://u:s3cretpw@localhost/danmaku_test")
	if pw != "s3cretpw" {
		t.Errorf("returned password = %q, want the URL's password (for scrubbing)", pw)
	}

	bad := []string{
		"postgres://danmaku:danmaku@localhost:3192/danmaku", // 開發用 DB
		"postgres://u:p@localhost/danmaku_test2",
		"postgres://u:p@localhost/Danmaku_Test",
		"postgres://u:p@localhost/danmaku_testing",
		"postgres://u:p@localhost/",
		"postgres://u:p@db.example.com/danmaku_test",
		"postgres://u:p@10.0.0.5/danmaku_test",
		"postgres://u:p@localhost.evil.com/danmaku_test",
		// 備援 host 也要是本機
		"postgres://u:p@localhost,db.example.com/danmaku_test",
		"host=prod.example.com dbname=danmaku_test user=u password=p",
		"not a url at all %%",
		"",
	}
	for _, in := range bad {
		if _, err := checkDatabaseURL(in); err == nil {
			t.Errorf("checkDatabaseURL(%q) = nil, want error", in)
		}
	}
}

func TestCheckRedisURL(t *testing.T) {
	for _, in := range []string{"redis://localhost:2907/15", "redis://127.0.0.1:6379/15", "redis://:pw@localhost:2907/15"} {
		if _, err := checkRedisURL(in); err != nil {
			t.Errorf("checkRedisURL(%q) = %v, want ok", in, err)
		}
	}
	for _, in := range []string{
		"redis://localhost:2907",   // db 0
		"redis://localhost:2907/0", // 開發用
		"redis://localhost:2907/14",
		"redis://example.com:6379/15",
		"redis://10.0.0.1:6379/15",
		"redis://localhost.evil.com:6379/15",
		"http://localhost:2907/15",
		"",
	} {
		if _, err := checkRedisURL(in); err == nil {
			t.Errorf("checkRedisURL(%q) = nil, want error", in)
		}
	}
}

// 錯誤訊息不能帶出密碼；scrubber 會把已知 secret 遮掉
func TestErrorsDoNotLeakSecrets(t *testing.T) {
	_, err := checkDatabaseURL("postgres://u:topsecretpw@db.example.com/danmaku_test")
	if err == nil || strings.Contains(err.Error(), "topsecretpw") {
		t.Fatalf("database error = %v, want error without the password", err)
	}
	_, err = checkRedisURL("redis://:topsecretpw@example.com:6379/15")
	if err == nil || strings.Contains(err.Error(), "topsecretpw") {
		t.Fatalf("redis error = %v, want error without the password", err)
	}
	_, err = checkDatabaseURL("postgres://u:topsecretpw@[bad/danmaku_test")
	if err == nil || strings.Contains(err.Error(), "topsecretpw") {
		t.Fatalf("unparseable database URL error = %v, want error without the password", err)
	}

	s := &scrubber{}
	s.add("topsecretpw")
	s.add("abc") // 太短，不遮
	if got := s.scrub("dial failed: password topsecretpw rejected, abc"); got != "dial failed: password *** rejected, abc" {
		t.Errorf("scrub = %q", got)
	}
}
