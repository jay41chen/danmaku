package handler

import (
	"encoding/base64"
	"testing"
	"time"

	"github.com/jay41chen/danmaku/server/internal/store"
)

func rawToken(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

func TestPageTokenRoundTrip(t *testing.T) {
	zone := time.FixedZone("UTC+8", 8*3600)
	created := time.Date(2030, 1, 2, 3, 4, 5, 123456000, zone)

	for _, publicID := range []string{"abcdefghijk", store.LegacyRoomPublicID} {
		t.Run(publicID, func(t *testing.T) {
			token, err := encodePageToken(store.RoomCursor{CreatedAt: created, PublicID: publicID})
			if err != nil {
				t.Fatalf("encodePageToken error = %v, want nil", err)
			}

			got, err := decodePageToken(token)

			if err != nil {
				t.Fatalf("decodePageToken error = %v, want nil", err)
			}
			if !got.CreatedAt.Equal(created) {
				t.Errorf("CreatedAt = %v, want %v", got.CreatedAt, created)
			}
			if got.PublicID != publicID {
				t.Errorf("PublicID = %q, want %q", got.PublicID, publicID)
			}
		})
	}
}

func TestDecodePageToken(t *testing.T) {
	const (
		validTime = `"2030-01-02T03:04:05.123456Z"`
		validID   = `"abcdefghijk"`
		validJSON = `{"v":1,"created_at":` + validTime + `,"public_id":` + validID + `}`
	)

	t.Run("valid token", func(t *testing.T) {
		got, err := decodePageToken(rawToken(validJSON))

		if err != nil {
			t.Fatalf("decodePageToken error = %v, want nil", err)
		}
		if want := time.Date(2030, 1, 2, 3, 4, 5, 123456000, time.UTC); !got.CreatedAt.Equal(want) || got.PublicID != "abcdefghijk" {
			t.Errorf("cursor = %+v, want {%v abcdefghijk}", got, want)
		}
	})

	bad := []struct {
		name  string
		token string
	}{
		{"empty", ""},
		{"not base64", "!!!"},
		{"padded base64", rawToken(validJSON) + "="},
		{"base64 of non-json", rawToken("hello world")},
		{"json null", rawToken(`null`)},
		{"json array", rawToken(`[]`)},
		{"unknown field", rawToken(`{"v":1,"created_at":` + validTime + `,"public_id":` + validID + `,"extra":1}`)},
		{"version 2", rawToken(`{"v":2,"created_at":` + validTime + `,"public_id":` + validID + `}`)},
		{"version missing", rawToken(`{"created_at":` + validTime + `,"public_id":` + validID + `}`)},
		{"version as string", rawToken(`{"v":"1","created_at":` + validTime + `,"public_id":` + validID + `}`)},
		{"created_at missing", rawToken(`{"v":1,"public_id":` + validID + `}`)},
		{"created_at zero", rawToken(`{"v":1,"created_at":"0001-01-01T00:00:00Z","public_id":` + validID + `}`)},
		{"created_at not a time", rawToken(`{"v":1,"created_at":"yesterday","public_id":` + validID + `}`)},
		{"public_id missing", rawToken(`{"v":1,"created_at":` + validTime + `}`)},
		{"public_id wrong length", rawToken(`{"v":1,"created_at":` + validTime + `,"public_id":"abc"}`)},
		{"public_id has plus", rawToken(`{"v":1,"created_at":` + validTime + `,"public_id":"abcdefghij+"}`)},
		{"trailing empty object", rawToken(validJSON + `{}`)},
		{"trailing garbage", rawToken(validJSON + ` x`)},
		{"trailing second valid object", rawToken(validJSON + validJSON)},
		// Decoder.More() 看到 } 或 ] 會回 false：只用 More() 檢查的話這個會漏
		{"trailing closing brace", rawToken(validJSON + `}`)},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			got, err := decodePageToken(tt.token)

			if err != errInvalidPageToken {
				t.Errorf("error = %v, want errInvalidPageToken", err)
			}
			if got != (store.RoomCursor{}) {
				t.Errorf("cursor = %+v, want zero value", got)
			}
		})
	}
}
