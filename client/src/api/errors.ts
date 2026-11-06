// HTTP 錯誤回應的共用解析與文字：server 回 {"error":{"code","message"}}。
// code 是翻譯 key，message 只在 client 不認得 code 時當後備。
// 不依賴 client.ts / session.ts（兩邊都會 import 這裡），避免循環依賴。

// server 的錯誤 code → 顯示給使用者的繁體中文
const CODE_TEXT: Record<string, string> = {
  invalid_json: "請求格式錯誤",
  internal_error: "伺服器發生錯誤，請稍後再試",
  rate_limited: "請求太頻繁，請稍後再試",
  unauthorized: "請先登入",
  gone: "此資源已不存在",
  username_invalid: "帳號格式不正確",
  password_length: "密碼長度不符規定",
  password_too_common: "此密碼太常見，請換一個",
  username_taken: "此帳號已被使用",
  invalid_credentials: "帳號或密碼錯誤",
  invalid_refresh_token: "登入已失效，請重新登入",
  room_not_found: "房間不存在",
  room_limit_reached: "已達房間數量上限",
  room_name_invalid: "房間名稱不符規定",
  room_name_length: "房間名稱長度不符規定",
  room_name_surrounding_space: "房間名稱開頭或結尾不可有空白",
  room_name_consecutive_spaces: "房間名稱不可有連續空白",
  room_name_too_many_marks: "房間名稱的組合符號過多",
  content_empty: "訊息不可為空",
  content_too_long: "訊息太長",
  content_invalid: "訊息內容不符規定",
  invalid_limit: "分頁數量不正確",
  invalid_cursor: "分頁位置無效，請重新整理",
  room_forbidden: "你沒有權限執行此操作",
  room_target_forbidden: "無法對此使用者執行此操作",
  room_role_conflict: "此使用者目前的身分不允許這個操作",
  user_not_found: "找不到此使用者",
  invalid_role: "身分不正確",
};

// 只依 status 產生的通用文字；body 不可信（可能是 HTML 或舊版純文字），永遠不顯示
export function genericErrorText(status: number): string {
  if (status === 401) return "請先登入";
  if (status === 404) return "找不到資源";
  if (status === 429) return "請求太頻繁，請稍後再試";
  if (status >= 500) return `伺服器發生錯誤（${status}）`;
  return `請求失敗（${status}）`;
}

export interface ParsedError {
  code: string | null;
  message: string | null;
}

// 防禦式解析錯誤 body：不是 JSON 或形狀不符一律回 code/message 皆 null
export function parseErrorBody(text: string): ParsedError {
  const none: ParsedError = { code: null, message: null };
  let data: unknown;
  try {
    data = JSON.parse(text);
  } catch {
    return none;
  }
  if (typeof data !== "object" || data === null) return none;
  const err = (data as Record<string, unknown>).error;
  if (typeof err !== "object" || err === null) return none;
  const e = err as Record<string, unknown>;
  if (typeof e.code !== "string" || e.code === "") return none;
  return {
    code: e.code,
    message: typeof e.message === "string" && e.message !== "" ? e.message : null,
  };
}

// 決定顯示文字：已知 code → 中文；未知 code 且有 message → message；其餘 → 依 status 的通用文字
export function errorText(
  status: number,
  code: string | null,
  message: string | null,
): string {
  if (code !== null && Object.hasOwn(CODE_TEXT, code)) return CODE_TEXT[code];
  if (code !== null && message !== null) return message;
  return genericErrorText(status);
}

export interface ReadError {
  status: number;
  code: string | null;
  text: string; // 要顯示給使用者的文字
  retryAfter: number | null;
}

// 讀取失敗的 Response 並解析成 ReadError，供 apiFetch 與 session 共用
export async function readError(res: Response): Promise<ReadError> {
  let body = "";
  try {
    body = await res.text();
  } catch {
    /* ignore */
  }
  const { code, message } = parseErrorBody(body);
  const ra = Number.parseInt(res.headers.get("Retry-After") ?? "", 10);
  return {
    status: res.status,
    code,
    text: errorText(res.status, code, message),
    retryAfter: Number.isFinite(ra) ? ra : null,
  };
}
