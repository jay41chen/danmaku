// API / WebSocket 位址集中在這裡，避免各處各自寫一份 fallback
export const API_URL: string =
  import.meta.env.VITE_API_URL || "http://localhost:8080";
export const WS_URL: string =
  import.meta.env.VITE_WS_URL || "ws://localhost:8080/ws";

// 暫時用 `?room=<public_id>` 指定房間（沒給或空字串 → 舊的預設房間 legacy00000）。
// 不用 /{username}/{public_id}：GitHub Pages 是靜態託管、沒有 SPA fallback，深層路徑會 404。
// 見 ROADMAP W4b。載入時讀一次即可（換房間 = 換網址 = 重新載入）。
export const ROOM_PUBLIC_ID: string =
  new URLSearchParams(window.location.search).get("room") || "legacy00000";
// 值來自使用者可控的網址，所以要 encodeURIComponent；兩個呼叫點共用，不各自組字串
export const ROOM_MESSAGES_PATH = `/rooms/${encodeURIComponent(ROOM_PUBLIC_ID)}/messages`;
