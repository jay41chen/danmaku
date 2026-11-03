package handler

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/runes"
	"golang.org/x/text/secure/precis"
	"golang.org/x/text/transform"
)

// 使用者輸入的文字驗證：彈幕內容（validateMessageContent）、房名（validateRoomName）
//
// 原則：只檢查、不改寫。不合規範就回 400，合規範就原樣存（不 trim、不正規化）
// 偷偷幫使用者修，存進去的會跟他送的不一樣，而且「修」這個動作本身可能被拿來繞過檢查
// 版面問題（Zalgo 撐高、RLO 反轉顯示）由前端輸出層處理，這裡只擋「不是合法文字」的輸入

const (
	// 彈幕長度上限（字元數，不是 bytes：中文一個字 3 bytes）
	maxMessageRunes = 100

	// 房名長度上限（字元數，不是 bytes）
	maxRoomNameRunes = 30
	// 房名最多允許連續幾個 combining mark（Mn + Me）：越南文一個字母最多疊 2 個（例如 ế = e + U+0302 + U+0301），再多就當成 Zalgo
	// keycap emoji 1️⃣ = 1 + U+FE0F（Mn）+ U+20E3（Me）剛好 2 個，在上限內
	maxRoomNameCombiningMarks = 2
)

// 彈幕內容、房名共用的 PRECIS Freeform profile（RFC 8264）：只拿它的 error 判斷有沒有不允許的字元（控制字元、零寬空白、RLO 等），
// 輸出（正規化後的字串）不用，存的是使用者送來的原樣
// 放 package 層級：Profile 建好後不會再變，所有 request 共用一個
var freeform = precis.NewFreeform(
	// 在 Freeform 之上多兩條規則（PRECIS 自己的擴充點，不在 validator 裡加特例）：
	//   - 檢查前先移除 VS15 / VS16（U+FE0E / U+FE0F）：手機鍵盤送的 ❤️ 是 U+2764 + U+FE0F，Freeform 預設會拒絕
	//   - DisallowEmpty：只有選擇符的字串，移除後變空 → 拒絕（不然 "\uFE0F" 這種看不見的內容會過）
	//
	// AdditionalMapping 收的是「產生 Transformer 的函式」而不是 Transformer 本身：
	// Transformer 有狀態（為串流設計，要記住處理到哪），這個 profile 又被所有 request 同時使用，
	// 所以每次 String() 都呼叫一次拿新的，用完就丟
	precis.AdditionalMapping(func() transform.Transformer {
		return runes.Remove(runes.Predicate(isVariationSelector))
	}),
	precis.DisallowEmpty,
)

// validateMessageContent / validateRoomName 回傳的錯誤，文字會直接回給 client（handler 用 err.Error()）
// 所以兩個 validate 只能回傳這裡的 sentinel，不能把 precis 等內部 error 往外丟
var (
	errContentEmpty   = errors.New("content must not be empty")
	errContentTooLong = errors.New("content too long")
	errContentInvalid = errors.New("content contains invalid characters")

	errRoomNameLength            = fmt.Errorf("room_name must be 1 to %d characters", maxRoomNameRunes)
	errRoomNameInvalid           = errors.New("room_name contains invalid characters")
	errRoomNameSurroundingSpace  = errors.New("room_name must not start or end with whitespace")
	errRoomNameConsecutiveSpaces = errors.New("room_name must not contain consecutive whitespace")
	errRoomNameTooManyMarks      = errors.New("room_name contains too many combining marks")
)

// 彈幕內容。頭尾空白、連續空白、Zalgo 都允許（彈幕文化常用；版面交給前端）
// 檢查順序：全空白 → 長度 → Freeform（便宜的先做：body 上限 1MB，超長的不用跑完 precis）
func validateMessageContent(s string) error {
	// 只用 trim 判斷「是不是整串空白」，不改 s
	if strings.TrimSpace(s) == "" {
		return errContentEmpty
	}
	if utf8.RuneCountInString(s) > maxMessageRunes {
		return errContentTooLong
	}
	if _, err := freeform.String(s); err != nil {
		return errContentInvalid
	}
	return nil
}

// 房名。比彈幕嚴：多擋頭尾空白、連續空白、過多 combining mark（房名會出現在房間列表，給所有人看）
// 檢查順序：長度 → Freeform → 頭尾空白 → 連續空白 → combining mark（便宜的先做）
// 空字串被長度擋；全空白被頭尾空白擋，所以不用另外檢查
func validateRoomName(s string) error {
	if n := utf8.RuneCountInString(s); n < 1 || n > maxRoomNameRunes {
		return errRoomNameLength
	}
	if _, err := freeform.String(s); err != nil {
		return errRoomNameInvalid
	}
	// 頭尾沒有空白的字串，trim 前後會一模一樣
	if strings.TrimSpace(s) != s {
		return errRoomNameSurroundingSpace
	}
	if hasConsecutiveSpaces(s) {
		return errRoomNameConsecutiveSpaces
	}
	if hasTooManyCombiningMarks(s, maxRoomNameCombiningMarks) {
		return errRoomNameTooManyMarks
	}
	return nil
}

// 有沒有相鄰兩個 rune 都是空白（unicode.IsSpace：半形空白、全形空白 U+3000 都算）
func hasConsecutiveSpaces(s string) bool {
	prevIsSpace := false
	for _, r := range s {
		if prevIsSpace && unicode.IsSpace(r) {
			return true
		}
		prevIsSpace = unicode.IsSpace(r)
	}
	return false
}

// 有沒有超過 max 個連續的 combining mark：Mn（疊在字的上下，Zalgo 主要用這種）+ Me（把字框起來，例如 U+20DD，疊多層效果跟 Zalgo 一樣）
// 不算 Mc：會佔寬度、往旁邊長不會撐高行，而且是印地文等文字的正常組成
// 中間只要隔一個不是 mark 的 rune 就重新計算；出現在字串開頭的 mark 也要算
func hasTooManyCombiningMarks(s string, max int) bool {
	combiningMarkCount := 0
	for _, r := range s {
		if unicode.Is(unicode.Mn, r) || unicode.Is(unicode.Me, r) {
			combiningMarkCount++
		} else {
			// 遇到不是 mark 的字元就歸零
			combiningMarkCount = 0
		}
		if combiningMarkCount > max {
			return true
		}
	}
	return false
}

// 是不是 emoji 用的變體選擇符（variation selector）：接在字元後面、本身看不見，只決定前一個字用哪種樣式顯示
// 其他會用到的例子：⚠️（U+26A0 + U+FE0F）、☺️（U+263A + U+FE0F）、keycap 1️⃣（1 + U+FE0F + U+20E3）
// 只是顯示提示、不是內容，所以檢查前移除；其他變體選擇符（U+FE00–U+FE0D 漢字異體 / 數學符號、U+E0100–U+E01EF）不處理，維持被 PRECIS 拒絕
func isVariationSelector(r rune) bool {
	//   - VS15（U+FE0E）文字樣式：U+2764 + U+FE0E → ❤︎（黑白，跟一般文字一樣）
	//   - VS16（U+FE0F）emoji 樣式：U+2764 + U+FE0F → ❤️（彩色；手機鍵盤送出的愛心就是這個）
	if r == '\uFE0E' ||
		r == '\uFE0F' {
		return true
	}
	return false
}
