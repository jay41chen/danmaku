// WS 連線管理（純 TS）。level-triggered：只要 WS 連著，它的身分就要和 session 目前狀態一致。
import { WS_URL, ROOM_MESSAGES_PATH, ROOM_PUBLIC_ID } from "../config";
import { apiFetch } from "../api/client";
import * as session from "../auth/session";
import { equalJitter } from "../lib/backoff";
import * as messages from "./messages";

const RECONNECT_BASE_MS = 1000;
const RECONNECT_CAP_MS = 32_000;
const AUTH_RETRY_BASE_MS = 1000;
const AUTH_RETRY_CAP_MS = 60_000;
const JOIN_RETRY_BASE_MS = 1000;
const JOIN_RETRY_CAP_MS = 30_000;
// 要跟 server wsPingPeriod（server/internal/handler/ws.go）一致，範圍檢查在 server/internal/handler/ws_test.go
const HEARTBEAT_INTERVAL_MS = 25_000;
const PONG_TIMEOUT_MS = 20_000;

let started = false;
let ws: WebSocket | null = null;
let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
let reconnectAttempt = 0;
// 這條 socket 上最後送出的 auth token；null 代表這條連線還是訪客
let sentToken: string | null = null;
let authRetryTimer: ReturnType<typeof setTimeout> | null = null;
let authRetryAttempt = 0;
let joinRetryTimer: ReturnType<typeof setTimeout> | null = null;
let joinRetryAttempt = 0;
let unsubscribeSession: (() => void) | null = null;
let heartbeatTimer: ReturnType<typeof setInterval> | null = null;
let pongTimer: ReturnType<typeof setTimeout> | null = null;

function stopHeartbeat() {
  if (heartbeatTimer !== null) {
    clearInterval(heartbeatTimer);
    heartbeatTimer = null;
  }
  if (pongTimer !== null) {
    clearTimeout(pongTimer);
    pongTimer = null;
  }
}

// 每 25 秒送 ping 並等 pong；逾時視為連線掉了（不是自己關的），走原本的退避重連
function startHeartbeat(sock: WebSocket) {
  stopHeartbeat();
  heartbeatTimer = setInterval(() => {
    if (sock !== ws || sock.readyState !== WebSocket.OPEN) return;
    sock.send(JSON.stringify({ type: "ping" }));
    if (pongTimer !== null) return; // 上一個 ping 還在等
    pongTimer = setTimeout(() => {
      pongTimer = null;
      if (sock !== ws) return;
      console.warn("ws: pong timeout, reconnecting");
      // 先把 ws 設 null，舊 socket 的 onclose 才不會重複排重連；重連由這裡直接排
      ws = null;
      stopHeartbeat();
      clearAuthRetry();
      clearJoinRetry();
      sock.close();
      scheduleReconnect();
    }, PONG_TIMEOUT_MS);
  }, HEARTBEAT_INTERVAL_MS);
}

function clearAuthRetry() {
  if (authRetryTimer !== null) {
    clearTimeout(authRetryTimer);
    authRetryTimer = null;
  }
}

function clearJoinRetry() {
  if (joinRetryTimer !== null) {
    clearTimeout(joinRetryTimer);
    joinRetryTimer = null;
  }
}

// server 要求先 join 才會收到房間廣播；每條 socket（含重連、recycle）都在 onopen 送一次
function sendJoin(sock: WebSocket) {
  sock.send(JSON.stringify({ type: "join", payload: { room: ROOM_PUBLIC_ID } }));
}

// server 暫時無法 join（ws_join_unavailable）→ 退避重送，直到 join_ok 或連線換掉
function scheduleJoinRetry(sock: WebSocket) {
  clearJoinRetry();
  const wait = equalJitter(joinRetryAttempt++, JOIN_RETRY_BASE_MS, JOIN_RETRY_CAP_MS);
  joinRetryTimer = setTimeout(() => {
    joinRetryTimer = null;
    if (sock === ws && sock.readyState === WebSocket.OPEN) sendJoin(sock);
  }, wait);
}

function sendAuth(sock: WebSocket, token: string) {
  sock.send(JSON.stringify({ type: "auth", payload: { token } }));
  sentToken = token;
}

// 讓 WS 身分追上 session：已登入且 token 跟上次送的不同才送
function syncAuth() {
  if (!ws || ws.readyState !== WebSocket.OPEN) return; // 還沒 OPEN 的由 onopen 讀最新狀態
  if (session.getSnapshot().status !== "authenticated") return;
  const token = session.getAccessToken();
  if (token && token !== sentToken) sendAuth(ws, token);
}

async function fetchMessages() {
  try {
    const res = await apiFetch(ROOM_MESSAGES_PATH);
    const data: unknown = await res.json();
    if (Array.isArray(data)) messages.merge(data.filter(messages.isMessage));
  } catch (e) {
    console.warn(`fetch ${ROOM_MESSAGES_PATH} failed`, e);
  }
}

// 自己關掉並換一條新的（登出時：server 沒有 logout frame，降回訪客只能斷線重連）。
// 舊 socket 的 handler 靠 `sock !== ws` 判斷成「自己關的」，不會觸發退避重連。
function recycle() {
  const old = ws;
  ws = null;
  stopHeartbeat();
  old?.close();
  reconnectAttempt = 0;
  connect();
}

function scheduleReconnect() {
  if (!started || reconnectTimer !== null) return;
  const wait = equalJitter(reconnectAttempt++, RECONNECT_BASE_MS, RECONNECT_CAP_MS);
  reconnectTimer = setTimeout(() => {
    reconnectTimer = null;
    connect();
  }, wait);
}

async function onAuthRejected(sock: WebSocket, rejected: string) {
  await session.reportRejected(rejected);
  if (sock !== ws) return;
  if (session.getSnapshot().status !== "authenticated") return; // anonymous 由 subscription 處理
  const current = session.getAccessToken();
  if (current !== rejected) {
    syncAuth(); // token 換了；通常 subscription 已送，這裡只是保險（已送過不會重複）
    return;
  }
  // 規則 3：新 token 仍被拒（例如 server DB 慢）→ 同一個 token 退避重送，直到 auth_ok 或狀態改變
  clearAuthRetry();
  const wait = equalJitter(authRetryAttempt++, AUTH_RETRY_BASE_MS, AUTH_RETRY_CAP_MS);
  authRetryTimer = setTimeout(() => {
    authRetryTimer = null;
    if (
      sock === ws &&
      sock.readyState === WebSocket.OPEN &&
      session.getSnapshot().status === "authenticated" &&
      session.getAccessToken() === rejected
    ) {
      sendAuth(sock, rejected);
    }
  }, wait);
}

function handleFrame(sock: WebSocket, raw: unknown) {
  let frame: { type?: unknown; payload?: unknown };
  try {
    frame = JSON.parse(String(raw));
  } catch {
    console.warn("ws: invalid JSON frame ignored");
    return;
  }
  const payload = frame.payload as Record<string, unknown> | undefined;
  switch (frame.type) {
    case "message":
      if (messages.isMessage(frame.payload)) messages.merge([frame.payload]);
      break;
    case "pong":
      // 收到就代表連線還活著；不用渲染
      if (pongTimer !== null) {
        clearTimeout(pongTimer);
        pongTimer = null;
      }
      break;
    case "auth_ok":
      if (sentToken) session.reportAccepted(sentToken);
      authRetryAttempt = 0;
      clearAuthRetry();
      break;
    case "join_ok":
      joinRetryAttempt = 0;
      clearJoinRetry();
      // WS open ≠ 已經在收廣播：open 到 join_ok（server SUBSCRIBE 完成）之間的訊息可能漏掉，
      // 所以 join_ok 後再 fetch 一次補洞；和 onopen 那次重疊的部分由 messages.merge 去重
      void fetchMessages();
      break;
    case "error": {
      const code = payload?.code;
      if (code === "ws_auth_failed" || code === "ws_auth_expired") {
        if (sentToken) void onAuthRejected(sock, sentToken);
      } else if (code === "ws_join_unavailable") {
        scheduleJoinRetry(sock);
      } else if (code === "ws_room_not_found") {
        console.warn("ws: room not found, not retrying");
      } else if (code === "ws_already_in_room" || code === "ws_not_in_room") {
        console.warn("ws: join state mismatch (client bug)", code);
      } else if (code === "ws_write_forbidden") {
        // 舊版 backend 不認得 join frame 時會回這個，無害
        console.warn("ws: write forbidden");
      } else {
        console.warn("ws: server error", code);
      }
      break;
    }
  }
}

function connect() {
  if (!started || ws) return;
  const sock = new WebSocket(WS_URL);
  ws = sock;
  sentToken = null;
  authRetryAttempt = 0;
  clearAuthRetry();
  joinRetryAttempt = 0;
  clearJoinRetry();
  stopHeartbeat();

  sock.onopen = () => {
    if (sock !== ws) return;
    reconnectAttempt = 0;
    startHeartbeat(sock);
    sendJoin(sock);
    syncAuth();
    // 先 WS 連上、再 GET：寧可重疊（merge 去重）也不要留空隙漏訊息。
    // 這次仍然需要：部署時前端先上線，舊 backend 永遠不會送 join_ok；
    // join_ok 後的第二次 fetch 負責補 open→subscribe 的空隙，重疊由 merge 去重
    void fetchMessages();
  };
  sock.onmessage = (e) => {
    if (sock === ws) handleFrame(sock, e.data);
  };
  sock.onerror = () => {
    /* 之後一定會有 onclose，在那邊處理 */
  };
  sock.onclose = (e) => {
    // code / wasClean 只拿來記 log，不用來判斷是不是自己關的
    console.info("ws closed", e.code, e.wasClean);
    if (sock !== ws) return; // 自己關的（stop / 登出 recycle）
    ws = null;
    clearAuthRetry();
    clearJoinRetry();
    stopHeartbeat();
    scheduleReconnect();
  };
}

function onSessionChange() {
  clearAuthRetry();
  const snap = session.getSnapshot();
  if (snap.status === "anonymous") {
    // 這條連線若曾以會員身分認證過，必須斷線重連才能降回訪客
    if (ws && sentToken !== null) recycle();
  } else if (snap.status === "authenticated") {
    syncAuth();
  }
}

export function start() {
  if (started) return;
  started = true;
  reconnectAttempt = 0;
  unsubscribeSession = session.subscribe(onSessionChange);
  connect();
}

export function stop() {
  started = false;
  unsubscribeSession?.();
  unsubscribeSession = null;
  if (reconnectTimer !== null) {
    clearTimeout(reconnectTimer);
    reconnectTimer = null;
  }
  clearAuthRetry();
  clearJoinRetry();
  stopHeartbeat();
  const old = ws;
  ws = null; // 先設 null，onclose 才會認得是自己關的
  old?.close();
}
