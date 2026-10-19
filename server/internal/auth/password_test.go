package auth

import (
	"testing"
)

func TestCheckPassword(t *testing.T) {
	// Given：共用的 hash，所有 case 都拿它來比
	hash, err := HashPassword("s3kr1t")
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}

	cases := []struct {
		name     string
		hash     string
		password string
		wantOK   bool
		wantErr  bool
	}{
		{"correct password", hash, "s3kr1t", true, false},
		{"mismatch password", hash, "s3Kr1t", false, false},
		{"error password", "not-a-hash", "??", false, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// When
			ok, err := CheckPassword(tc.hash, tc.password)

			// Then
			if (err != nil) != tc.wantErr {
				t.Fatalf("CheckPassword() error = %v, wantErr %v", err, tc.wantErr)
			}
			if ok != tc.wantOK {
				t.Errorf("CheckPassword() ok = %v, want %v", ok, tc.wantOK)
			}
		})
	}
}

func TestHashPasswordUsesRandomSalt(t *testing.T) {
	pw := "aaabbb"
	hash1, err1 := HashPassword(pw)
	hash2, err2 := HashPassword(pw)

	if err1 != nil || err2 != nil {
		t.Fatalf("hash should contains value %s, err1: %s, err2: %s", pw, err1, err2)
	}

	if hash1 == hash2 {
		t.Fatalf("salt should not same %s, %s", hash1, hash2)
	}
}
