package handler

import (
	"errors"
	"strings"
	"testing"
)

// 契約：validator 只能回傳自己的 sentinel（或 nil），不能把 precis 等內部 error 往外丟
// 這個 helper 檢查 got 一定是 sentinels 其中之一，並且就是 want
func assertSentinel(t *testing.T, got, want error, sentinels []error) {
	t.Helper()
	if want == nil {
		if got != nil {
			t.Fatalf("got error %v, want nil", got)
		}
		return
	}
	if got == nil {
		t.Fatalf("got nil, want %v", want)
	}
	known := false
	for _, s := range sentinels {
		if errors.Is(got, s) {
			known = true
			break
		}
	}
	if !known {
		t.Fatalf("got %v, which is not one of the validator's sentinels (internal error leaked?)", got)
	}
	if !errors.Is(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// 直接呼叫 validateMessageContent（不經 HTTP），確認回的是正確的 sentinel，不只是「有錯」
func TestValidateMessageContent(t *testing.T) {
	sentinels := []error{errContentEmpty, errContentTooLong, errContentInvalid}

	tests := []struct {
		name  string
		input string
		want  error
	}{
		{"empty string", "", errContentEmpty},
		{"ascii whitespace only", "  \t", errContentEmpty},
		{"ideographic space only", "\u3000\u3000", errContentEmpty},

		{"101 runes too long", strings.Repeat("哈", 101), errContentTooLong},
		{"100 runes ok", strings.Repeat("哈", 100), nil},
		// 長度檢查先於 Freeform：超長又含不合法字元時回 TooLong
		{"too long and invalid reports too long", strings.Repeat("哈", 100) + "\u200b", errContentTooLong},

		{"zero width space", "\u200b", errContentInvalid},
		{"newline", "a\nb", errContentInvalid},
		{"right-to-left override", "abc\u202eevil", errContentInvalid},
		{"zwj emoji sequence", "\U0001F468\u200d\U0001F469\u200d\U0001F467", errContentInvalid},

		{"fullwidth punctuation", "好喔！", nil},
		{"bopomofo", "ㄏㄏ", nil},
		{"emoji", "😂", nil},
		{"surrounding spaces allowed", " 前後空白 ", nil},
		{"consecutive spaces allowed", "a  b", nil},
		{"zalgo allowed", "Z\u0301\u0301\u0301\u0301", nil},

		// VS16 (U+FE0F) 會在檢查前被 profile 移除（手機鍵盤送心形 emoji 是 U+2764 U+FE0F）；
		// 只剩 selector 的字串移除後變空字串，會被拒絕
		{"heart with VS16", "\u2764\ufe0f", nil},
		{"heart with VS15", "\u2764\ufe0e", nil},
		{"keycap emoji", "1\ufe0f\u20e3", nil},
		{"VS16 emoji with text", "\u26a0\ufe0f 小心", nil},
		{"variation selector only", "\ufe0f", errContentInvalid},
		{"two variation selectors only", "\ufe0f\ufe0f", errContentInvalid},
		{"zero width after VS16 emoji", "\u2764\ufe0f\u200b", errContentInvalid},
		// 刻意的不對稱：選擇符在檢查前被移除，所以彈幕會過（看不見的夾帶，屬於之後內容審核處理，見 ROADMAP C1 Known）；
		// 房名則因為 U+FE0F 是 Mn，被 mark 上限擋下
		{"letter with 3 variation selectors", "a\ufe0f\ufe0f\ufe0f", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertSentinel(t, validateMessageContent(tt.input), tt.want, sentinels)
		})
	}
}

// 直接呼叫 validateRoomName（不經 HTTP），確認回的是正確的 sentinel
func TestValidateRoomName(t *testing.T) {
	sentinels := []error{
		errRoomNameLength, errRoomNameInvalid, errRoomNameSurroundingSpace,
		errRoomNameConsecutiveSpaces, errRoomNameTooManyMarks,
	}

	tests := []struct {
		name  string
		input string
		want  error
	}{
		{"empty string", "", errRoomNameLength},
		{"31 runes too long", strings.Repeat("哈", 31), errRoomNameLength},
		{"30 runes ok", strings.Repeat("哈", 30), nil},

		{"zero width space", "\u200b房間", errRoomNameInvalid},
		{"newline", "房\n間", errRoomNameInvalid},

		{"leading space", " 房間", errRoomNameSurroundingSpace},
		{"trailing space", "房間 ", errRoomNameSurroundingSpace},
		{"trailing ideographic space", "房間\u3000", errRoomNameSurroundingSpace},
		{"spaces only", "   ", errRoomNameSurroundingSpace},

		{"two ascii spaces", "房  間", errRoomNameConsecutiveSpaces},
		{"ascii space then ideographic space", "房 \u3000間", errRoomNameConsecutiveSpaces},

		{"3 marks at start", "\u0301\u0301\u0301房", errRoomNameTooManyMarks},
		{"3 marks in middle", "a\u0301\u0301\u0301b", errRoomNameTooManyMarks},

		{"single space inside", "房 間", nil},
		{"single ideographic space inside", "房\u3000間", nil},
		// 計數遇到不是 mark 的字元就歸零，所以前後兩組各 2 個都不超標
		{"vietnamese 2 marks", "e\u0302\u0301", nil},
		{"two letters each with 2 marks", "e\u0301\u0301a\u0301\u0301", nil},
		{"fullwidth punctuation", "好喔！", nil},
		// Mc（例如 U+0903 天城文 visarga）會佔寬度、不會撐高，不算進 mark 上限
		{"3 spacing marks are not counted", "x\u0903\u0903\u0903", nil},

		{"heart with VS16", "\u2764\ufe0f", nil},
		{"heart with VS15", "\u2764\ufe0e", nil},
		// U+FE0F 是 Mn、U+20E3 是 Me → 連續剛好 2 個 mark，在上限內
		{"keycap emoji", "1\ufe0f\u20e3", nil},
		{"two keycap emoji", "1\ufe0f\u20e31\ufe0f\u20e3", nil},
		// U+FE0F 是 Mn：連 3 個超過 mark 上限（彈幕那邊同樣的輸入會過，見 TestValidateMessageContent）
		{"letter with 3 variation selectors", "a\ufe0f\ufe0f\ufe0f", errRoomNameTooManyMarks},
		{"variation selector only", "\ufe0f", errRoomNameInvalid},
		{"3 enclosing marks", "a\u20dd\u20dd\u20dd", errRoomNameTooManyMarks},
		{"mixed Mn and Me marks", "a\u0301\u20dd\u0301", errRoomNameTooManyMarks},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertSentinel(t, validateRoomName(tt.input), tt.want, sentinels)
		})
	}
}
