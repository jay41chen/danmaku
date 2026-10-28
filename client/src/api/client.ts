// fetch wrapper：帶 Authorization、處理 401（換 token 重送一次）。
// /refresh /login /logout /register 不走這裡（在 session 內直接 fetch），
// 否則 /refresh 的 401 會再觸發 refresh。
import { API_URL } from "../config";
import * as session from "../auth/session";

export class ApiError extends Error {
  status: number;
  retryAfter: number | null; // 秒；跨 origin 時 server 沒 expose header，會是 null
  constructor(status: number, message: string, retryAfter: number | null) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.retryAfter = retryAfter;
  }
}

async function toApiError(res: Response): Promise<ApiError> {
  let text = "";
  try {
    text = (await res.text()).trim();
  } catch {
    /* ignore */
  }
  const ra = Number.parseInt(res.headers.get("Retry-After") ?? "", 10);
  return new ApiError(
    res.status,
    text || `HTTP ${res.status}`,
    Number.isFinite(ra) ? ra : null,
  );
}

export async function apiFetch(
  path: string,
  init: RequestInit = {},
  opts: { auth: boolean } = { auth: false },
): Promise<Response> {
  let retried = false;
  for (;;) {
    const token = opts.auth ? session.getAccessToken() : null;
    if (opts.auth && !token) throw new ApiError(401, "not logged in", null);
    const headers = new Headers(init.headers);
    if (token) headers.set("Authorization", `Bearer ${token}`);
    const res = await fetch(`${API_URL}${path}`, { ...init, headers });

    if (res.ok) {
      if (token) session.reportAccepted(token);
      return res;
    }
    if (res.status === 401 && token) {
      if (!retried) {
        await session.reportRejected(token);
        const next = session.getAccessToken();
        // 只要不是 anonymous 就重送：status 可能是 refreshing（另一個 refresh 剛好在跑），
        // 但目前的 access token 在到期前仍有效，不能因此登出
        if (
          session.getSnapshot().status !== "anonymous" &&
          next &&
          next !== token
        ) {
          // 換到新 token 了，重送一次。server 401 前不會寫 DB，重送 POST 是安全的
          retried = true;
          continue;
        }
      }
      // 重送後仍 401，或新 token 仍被拒：清 session，跳回登入
      void session.logout();
      throw new ApiError(401, "unauthorized", null);
    }
    throw await toApiError(res);
  }
}
