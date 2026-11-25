// fetch wrapper：帶 Authorization、處理 401（換 token 重送一次）、每次嘗試有逾時（API_TIMEOUT_MS）。
// /refresh /login /logout /register 不走這裡（在 session 內直接 fetch），
// 否則 /refresh 的 401 會再觸發 refresh。
import { API_URL } from "../config";
import * as session from "../auth/session";
import { genericErrorText, readError } from "./errors";

// 每次 fetch 嘗試的逾時（401 換 token 後的第二次嘗試重新計時）。
// 和 session 的 REFRESH_DEADLINE_MS 同為 10 秒；server 最慢的路徑
// （Redis 1 秒預算、DB + 等連線池約 2 秒）離這個值還有很大餘裕，不會誤殺慢但健康的請求
const API_TIMEOUT_MS = 10_000;

export class ApiError extends Error {
  status: number;
  code: string | null; // server 的錯誤 code；body 不是新格式（舊後端 / nginx / mux）時為 null
  retryAfter: number | null; // 秒；跨 origin 時 server 沒 expose header，會是 null
  // message 是要顯示給使用者的文字（已翻譯，不含原始 body）
  constructor(
    status: number,
    message: string,
    retryAfter: number | null,
    code: string | null = null,
  ) {
    super(message);
    this.name = "ApiError";
    this.status = status;
    this.code = code;
    this.retryAfter = retryAfter;
  }
}

// 把失敗的 Response 轉成 ApiError，解析與翻譯交給 errors.ts
async function toApiError(res: Response): Promise<ApiError> {
  const e = await readError(res);
  return new ApiError(e.status, e.text, e.retryAfter, e.code);
}

// 逾時（沒收到 response headers）。刻意不繼承 ApiError：
// realtime/connection.ts 的 isRetryableFetchError 會把 status < 500 的 ApiError 當成不可重試，
// 而非 ApiError 會被當成可重試的網路錯誤，這正是逾時該有的行為
export class ApiTimeoutError extends Error {
  constructor() {
    super("連線逾時");
    this.name = "ApiTimeoutError";
  }
}

// 單次 fetch 加上逾時，並串接呼叫端的 signal。
// 不用 AbortSignal.any（要 Safari 17.4+，舊 iOS 會讓所有 API 壞掉），手動做（同 session.ts withDeadline 的 setTimeout + AbortController 寫法）。
// 逾時只計到 fetch resolve（收到 response headers）為止；之後的 body 讀取不再被計時器中止。
// 但呼叫端的 signal 會一直有效：呼叫端 abort 時，連已回傳的 body 讀取也會被取消
// （ac.signal 是 fetch 用的 signal，abort 它會取消 body stream）。
// 逾時丟 ApiTimeoutError；呼叫端 abort 時丟出呼叫端的 signal.reason
// （呼叫端呼叫 abort() 沒帶 reason 時就是瀏覽器的 AbortError；usePopularRooms 即是如此，且忽略所有錯誤）；其他網路錯誤原樣 rethrow
async function fetchWithTimeout(
  url: string,
  init: RequestInit,
): Promise<Response> {
  const ac = new AbortController();
  let timedOut = false;
  const t = setTimeout(() => {
    timedOut = true;
    ac.abort();
  }, API_TIMEOUT_MS);
  const callerSignal = init.signal;
  const onCallerAbort = () => ac.abort(callerSignal?.reason);
  if (callerSignal) {
    if (callerSignal.aborted) ac.abort(callerSignal.reason);
    else callerSignal.addEventListener("abort", onCallerAbort);
  }
  try {
    // 成功時不移除 onCallerAbort：讓呼叫端之後仍能取消 body 讀取。代價是 listener 會留到呼叫端的 signal 被 abort 或回收，所以傳入的 signal 應該每個請求各開一個（新的 AbortController），不要在多次 apiFetch 之間共用長壽的 signal（計時器則在 finally 清掉，只管到 headers）
    return await fetch(url, { ...init, signal: ac.signal });
  } catch (e) {
    callerSignal?.removeEventListener("abort", onCallerAbort);
    if (timedOut && !callerSignal?.aborted) throw new ApiTimeoutError();
    throw e;
  } finally {
    clearTimeout(t);
  }
}

// init.signal 要每個請求各開一個，不要在多次呼叫之間共用（見 fetchWithTimeout）
export async function apiFetch(
  path: string,
  init: RequestInit = {},
  opts: { auth: boolean } = { auth: false },
): Promise<Response> {
  let retried = false;
  for (;;) {
    const token = opts.auth ? session.getAccessToken() : null;
    if (opts.auth && !token) throw new ApiError(401, genericErrorText(401), null);
    const headers = new Headers(init.headers);
    if (token) headers.set("Authorization", `Bearer ${token}`);
    const res = await fetchWithTimeout(`${API_URL}${path}`, { ...init, headers });

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
          void res.body?.cancel(); // 第一次的 401 回應不會再讀，先釋放連線再重送
          continue;
        }
      }
      // 重送後仍 401，或新 token 仍被拒：清 session，跳回登入
      void session.logout();
      throw new ApiError(401, genericErrorText(401), null);
    }
    throw await toApiError(res);
  }
}
