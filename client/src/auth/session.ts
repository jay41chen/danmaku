// Session state machine：純 TS，不依賴 React。
// 外部只送事件（bootstrap / login / logout / reportRejected ...），狀態由這裡自己決定。
// 注意：這裡所有 log / error 都不可以帶 access token / refresh token。
import { API_URL } from "../config";

export type SessionStatus = "anonymous" | "authenticated" | "refreshing";
export interface User {
  id: number;
  username: string;
}
export interface SessionSnapshot {
  status: SessionStatus;
  user: User | null;
}

const REFRESH_KEY = "danmaku.refreshToken";
const REFRESH_LEEWAY_SEC = 300; // 到期前 5 分鐘主動 refresh
const REFRESH_DEADLINE_MS = 10_000; // 總重試期限，見 withDeadline

// ---- localStorage（隱私模式可能 throw，失敗一律當作沒有）----
function readRefreshToken(): string | null {
  try {
    return localStorage.getItem(REFRESH_KEY);
  } catch {
    return null;
  }
}
function writeRefreshToken(token: string) {
  try {
    localStorage.setItem(REFRESH_KEY, token);
  } catch {
    /* 寫不進去就沒有 refresh token：access token 過期後會被登出（App 會提示使用者） */
  }
}
// 偵測 localStorage 是否可寫（被封鎖時 throw）；模組載入時算一次，App 用來提示使用者
function detectStorage(): boolean {
  try {
    const k = "danmaku.storageTest";
    localStorage.setItem(k, "1");
    localStorage.removeItem(k);
    return true;
  } catch {
    return false;
  }
}
export const isStorageAvailable: boolean = detectStorage();

function removeRefreshToken() {
  try {
    localStorage.removeItem(REFRESH_KEY);
  } catch {
    /* ignore */
  }
}

// ---- state ----
let accessToken: string | null = null;
// 已被 REST 2xx / WS auth_ok 確認可用過的 token；不等於 accessToken 代表「剛拿到、未確認」
let acceptedToken: string | null = null;
let snapshot: SessionSnapshot = {
  // 有 refresh token 就從 refreshing 開始，避免啟動瞬間閃一下登入表單
  status: readRefreshToken() ? "refreshing" : "anonymous",
  user: null,
};
const listeners = new Set<() => void>();
let timer: ReturnType<typeof setTimeout> | null = null;
// inflight 記住所屬 epoch：清除 session（epoch +1）後，舊的 inflight 不再被共用
let inflight: { epoch: number; promise: Promise<boolean> } | null = null;
let bootstrapPromise: Promise<void> | null = null;
// 每次清除 session 就 +1，讓進行中的 refresh / me 發現「期間已經登出」而丟棄結果，
// 否則它們會把剛清掉的 token 又寫回來
let epoch = 0;

export function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}
export function getSnapshot(): SessionSnapshot {
  return snapshot;
}
export function getAccessToken(): string | null {
  return accessToken;
}

function setSnapshot(status: SessionStatus, user: User | null) {
  if (snapshot.status === status && snapshot.user === user) return;
  snapshot = { status, user }; // immutable：每次換新物件給 useSyncExternalStore
  listeners.forEach((l) => l());
}

// 只回傳「屬於目前 epoch」的進行中 refresh
function currentInflight(): Promise<boolean> | null {
  return inflight && inflight.epoch === epoch ? inflight.promise : null;
}

function clearTimer() {
  if (timer !== null) {
    clearTimeout(timer);
    timer = null;
  }
}

function clearAll() {
  epoch++;
  accessToken = null;
  acceptedToken = null;
  clearTimer();
  removeRefreshToken();
  setSnapshot("anonymous", null);
}

interface TokenPair {
  access_token: string;
  refresh_token: string;
  expires_in: number;
}

// 拿到新 token：存起來並重設唯一的 timer
function applyTokens(t: TokenPair) {
  accessToken = t.access_token;
  acceptedToken = null;
  writeRefreshToken(t.refresh_token);
  clearTimer();
  const delaySec = Math.max(0, t.expires_in - REFRESH_LEEWAY_SEC);
  timer = setTimeout(() => {
    timer = null;
    void refresh();
  }, delaySec * 1000);
}

function sleep(ms: number) {
  return new Promise<void>((r) => setTimeout(r, ms));
}

// 在總 deadline 內重試 fn。fn 回傳 undefined 或 throw 代表「可重試的失敗」，
// 回傳其他值代表定案。每次 fetch 的 AbortController 不會超過剩餘 deadline。
async function withDeadline<T>(
  fn: (signal: AbortSignal) => Promise<T | undefined>,
): Promise<T | undefined> {
  const deadline = Date.now() + REFRESH_DEADLINE_MS;
  for (let attempt = 0; ; attempt++) {
    const remaining = deadline - Date.now();
    if (remaining <= 0) return undefined;
    const ac = new AbortController();
    const t = setTimeout(() => ac.abort(), remaining);
    try {
      const r = await fn(ac.signal);
      if (r !== undefined) return r;
    } catch {
      /* 網路錯誤 / abort / JSON 壞掉：當作可重試 */
    } finally {
      clearTimeout(t);
    }
    const wait = Math.min(1000 * 2 ** attempt, deadline - Date.now());
    if (wait <= 0) return undefined;
    await sleep(wait);
  }
}

async function tryRefresh(
  rt: string,
  signal: AbortSignal,
): Promise<TokenPair | "rejected" | undefined> {
  const res = await fetch(`${API_URL}/refresh`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ refresh_token: rt }),
    signal,
  });
  if (res.status === 401) return "rejected"; // token 無效 / 已撤銷：重試沒有意義
  if (!res.ok) return undefined; // 5xx / 429 / 其他：重試
  return (await res.json()) as TokenPair;
}

// single-flight：同時只有一個 /refresh，其他呼叫共用同一個 Promise。
// 成功回 true。成功時若還沒有 user（bootstrap），維持 refreshing 等 /me。
function refresh(): Promise<boolean> {
  const existing = currentInflight();
  if (existing) return existing;
  const rt = readRefreshToken();
  if (!rt) {
    clearAll();
    return Promise.resolve(false);
  }
  const myEpoch = epoch;
  setSnapshot("refreshing", snapshot.user); // 保留 user，畫面不閃成未登入
  const entry: { epoch: number; promise: Promise<boolean> } = {
    epoch: myEpoch,
    promise: undefined as unknown as Promise<boolean>,
  };
  entry.promise = (async () => {
    let result: TokenPair | "rejected" | undefined;
    try {
      result = await withDeadline((s) => tryRefresh(rt, s));
    } catch (e) {
      // 理論上不會發生（withDeadline 自己會吞掉錯誤）；萬一發生，當成 refresh 失敗：
      // 走下面 undefined → clearAll 的路，不要讓 promise reject（呼叫端多是 void，會變 unhandled rejection、狀態卡在 refreshing）
      // 只印 name：錯誤物件的 message / stack / cause 內容不受控，可能夾帶 token
      console.error("refresh: unexpected error", e instanceof Error ? e.name : typeof e);
      result = undefined;
    } finally {
      // 放 finally：就算 withDeadline 之後改成會 reject，也不會讓 inflight 卡住
      if (inflight === entry) inflight = null; // 比對身分，避免清掉新 epoch 的 inflight
    }
    if (myEpoch !== epoch) return false; // 期間已登出
    if (result === undefined || result === "rejected") {
      // 超過 deadline 也清掉：server 可能已 rotate 但回應掉了，
      // 拖過 server 的 grace 再拿舊 token 會被判定 duplicate use
      clearAll();
      return false;
    }
    applyTokens(result);
    if (snapshot.user) setSnapshot("authenticated", snapshot.user);
    return true;
  })();
  inflight = entry;
  return entry.promise;
}

async function tryMe(
  token: string,
  signal: AbortSignal,
): Promise<User | "unauthorized" | undefined> {
  const res = await fetch(`${API_URL}/me`, {
    headers: { Authorization: `Bearer ${token}` },
    signal,
  });
  if (res.status === 401) return "unauthorized";
  if (!res.ok) return undefined;
  return (await res.json()) as User;
}

export function bootstrap(): Promise<void> {
  // StrictMode 會 effect 兩次；共用同一個 promise 避免打兩次 /refresh（會 rotation 兩次）
  if (bootstrapPromise) return bootstrapPromise;
  bootstrapPromise = (async () => {
    if (!readRefreshToken()) {
      clearAll();
      return;
    }
    const myEpoch = epoch;
    if (!(await refresh())) return;
    const token = accessToken;
    if (!token) return;
    const me = await withDeadline((s) => tryMe(token, s));
    if (myEpoch !== epoch) return;
    if (me === "unauthorized") {
      clearAll();
    } else if (me === undefined) {
      // /me 一直失敗（5xx / 網路）：不清 localStorage 的 refresh token（問題不在 token），
      // 只丟掉 memory 的 access token，這次顯示未登入，之後重新整理會再試
      epoch++;
      accessToken = null;
      acceptedToken = null;
      clearTimer();
      setSnapshot("anonymous", null);
    } else {
      reportAccepted(token);
      setSnapshot("authenticated", me);
    }
  })();
  return bootstrapPromise;
}

async function readError(res: Response): Promise<string> {
  try {
    return (await res.text()).trim();
  } catch {
    return "";
  }
}

export async function login(username: string, password: string): Promise<void> {
  let res: Response;
  try {
    res = await fetch(`${API_URL}/login`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ username, password }),
    });
  } catch {
    throw new Error("網路錯誤，請稍後再試");
  }
  if (res.status === 400) throw new Error("帳號或密碼錯誤");
  if (res.status === 429) throw new Error("嘗試次數過多，請稍後再試");
  if (!res.ok) throw new Error(`登入失敗（${res.status}）`);
  const data = (await res.json()) as TokenPair & { user: User };
  epoch++; // 讓任何進行中的舊 refresh 結果作廢
  applyTokens(data);
  setSnapshot("authenticated", data.user);
}

export async function register(
  username: string,
  password: string,
): Promise<void> {
  let res: Response;
  try {
    res = await fetch(`${API_URL}/register`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ username, password }),
    });
  } catch {
    throw new Error("網路錯誤，請稍後再試");
  }
  if (!res.ok) {
    // server 錯誤 body 是純文字，直接顯示
    throw new Error((await readError(res)) || `註冊失敗（${res.status}）`);
  }
  await login(username, password); // /register 不回 token
}

export async function logout(): Promise<void> {
  // 先等進行中的 refresh 結束：否則 /refresh 與 /logout 會帶同一顆舊 token，
  // server 若先處理 /logout（撤銷原因 'logout'，不適用 rotated 的 grace），
  // 後到的 /refresh 會被判定 duplicate use，連帶登出該使用者所有裝置。
  // refresh 自帶 REFRESH_DEADLINE_MS（約 10 秒）期限，所以這裡的等待有上限。
  const pending = currentInflight();
  if (pending) {
    try {
      await pending;
    } catch {
      /* refresh 不會 reject；保險起見，無論如何都要繼續登出 */
    }
  }
  const rt = readRefreshToken(); // refresh 成功時這已是 rotate 後的新 token
  // refresh 失敗（逾時 / 被拒）時這裡是 null → 故意不送 /logout：
  // 舊 token 的 /refresh 封包可能還在路上，再送 /logout(舊 token) 會讓它被判成 duplicate use、所有裝置登出
  // 代價是舊 token 在 server 留到過期（本機已刪，外洩面不變）
  clearAll(); // 先清本地，不等 server
  if (!rt) return;
  try {
    await fetch(`${API_URL}/logout`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ refresh_token: rt }),
    });
  } catch {
    /* best-effort */
  }
}

export function reportAccepted(token: string) {
  if (token === accessToken) acceptedToken = token;
}

export async function reportRejected(token: string): Promise<void> {
  // 1. 已經有人換過 token：呼叫端改用目前的
  if (token !== accessToken) return;
  // 2. 正在 refresh：等它
  const pending = currentInflight();
  if (pending) {
    await pending;
    return;
  }
  // 3. 剛拿到、還沒被確認過的 token 被拒：問題不在 token，再換也沒用，
  //    且會造成 retry storm，所以不 refresh
  if (acceptedToken !== token) return;
  // 4. 用過、曾經可用的 token 現在被拒：換新的
  await refresh();
}
