package textrule

import (
	"errors"
	"strings"
	"testing"

	"github.com/jay41chen/danmaku/server/internal/model"
)

// 每個驗證 sentinel 都要帶預期的 code：code 是 API 合約，改名會被這裡抓到
func TestValidationSentinelCodes(t *testing.T) {
	cases := []struct {
		err  *model.CodedError
		want string
	}{
		{errContentEmpty, "content_empty"},
		{errContentTooLong, "content_too_long"},
		{errContentInvalid, "content_invalid"},
		{errRoomNameInvalid, "room_name_invalid"},
		{errRoomNameLength, "room_name_length"},
		{errRoomNameSurroundingSpace, "room_name_surrounding_space"},
		{errRoomNameConsecutiveSpaces, "room_name_consecutive_spaces"},
		{errRoomNameTooManyMarks, "room_name_too_many_marks"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if tc.err.Code != tc.want {
				t.Errorf("code = %q, want %q", tc.err.Code, tc.want)
			}
			if tc.err.Message == "" {
				t.Error("message is empty")
			}
		})
	}

	// 從 validator 實際回傳的值驗：確認 validate 函式回的是這些 sentinel，不是別的
	validators := []struct {
		name string
		got  error
		want string
	}{
		{"empty content", MessageContent("   "), "content_empty"},
		{"long content", MessageContent(strings.Repeat("a", maxMessageRunes+1)), "content_too_long"},
		{"invalid content", MessageContent("a\x00b"), "content_invalid"},
		{"invalid room name", RoomName("a\x00b"), "room_name_invalid"},
		{"room name length", RoomName(""), "room_name_length"},
		{"surrounding space", RoomName(" a"), "room_name_surrounding_space"},
		{"consecutive spaces", RoomName("a  b"), "room_name_consecutive_spaces"},
		{"too many marks", RoomName("á́́"), "room_name_too_many_marks"},
	}
	for _, tc := range validators {
		t.Run("validator "+tc.name, func(t *testing.T) {
			var coded *model.CodedError
			if !errors.As(tc.got, &coded) || coded.Code != tc.want {
				t.Errorf("err = %v, want CodedError with code %q", tc.got, tc.want)
			}
		})
	}
}
