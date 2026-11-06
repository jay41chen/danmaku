// 目前登入者在本房的角色（純 TS store）。只是 UX 用：按鈕要不要顯示；真正的檢查在 server。
// 任何錯誤都當一般觀眾（被 ban 的人 server 也回 viewer，所以這裡不會有任何特別的狀態）。
import { ApiError } from "../api/client";
import { fetchMyRole, type MyRole } from "../api/roles";

interface RoleState {
  role: MyRole;
  rosterVersion: number; // 名單異動時 +1，開著的管理名單看到變動就重新載入
}

let state: RoleState = { role: "viewer", rosterVersion: 0 };
const listeners = new Set<() => void>();
// 世代計數器：重設 / 重抓後，舊的請求結果不能蓋掉新的
let gen = 0;

export function subscribe(l: () => void): () => void {
  listeners.add(l);
  return () => listeners.delete(l);
}
export function getSnapshot(): RoleState {
  return state;
}

function setState(next: RoleState) {
  if (next.role === state.role && next.rosterVersion === state.rosterVersion) return;
  state = next; // immutable：每次換新物件給 useSyncExternalStore
  listeners.forEach((l) => l());
}

// 登出 / 換帳號 / 離開房間：立刻降回一般觀眾並作廢進行中的請求
export function reset() {
  gen++;
  setState({ ...state, role: "viewer" });
}

// 重抓角色；失敗（含 401、網路錯誤）一律當一般觀眾
export async function refresh() {
  const my = ++gen;
  let role: MyRole = "viewer";
  try {
    role = await fetchMyRole();
  } catch {
    /* 按鈕只是 UX，抓不到就不顯示 */
  }
  if (my === gen) setState({ ...state, role });
}

export function bumpRoster() {
  setState({ ...state, rosterVersion: state.rosterVersion + 1 });
}

// 執行一個會改角色的操作並統一收尾：回傳錯誤文字（成功為 null）。
// 成功 → 通知名單重新載入；403 → 角色可能已被降級，重抓 /me 讓過期的按鈕消失
export async function runAction(fn: () => Promise<void>): Promise<string | null> {
  try {
    await fn();
    bumpRoster();
    return null;
  } catch (e) {
    if (e instanceof ApiError) {
      if (e.status === 403) void refresh();
      return e.message;
    }
    return "網路錯誤，請稍後再試";
  }
}
