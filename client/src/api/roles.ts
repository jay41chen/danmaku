// 房間角色 API：GET /rooms/{id}/me、GET/PUT/DELETE /rooms/{id}/roles
import { apiFetch } from "./client";
import { ROOM_PUBLIC_ID } from "../config";

export type MyRole = "owner" | "moderator" | "viewer";
export type AssignableRole = "moderator" | "banned";

export interface RoleEntry {
  user_id: number;
  username: string;
  role: AssignableRole;
  assigned_by: number | null; // 指派者帳號被刪或舊資料時為 null
  assigned_by_username: string | null;
  created_at: string;
}

export interface RoleListPage {
  roles: RoleEntry[];
  next_page_token?: string; // 沒有下一頁時 server 不會送這個欄位
}

const PAGE_LIMIT = 20;
const BASE = `/rooms/${encodeURIComponent(ROOM_PUBLIC_ID)}`;

function isRoleEntry(x: unknown): x is RoleEntry {
  if (typeof x !== "object" || x === null) return false;
  const r = x as Record<string, unknown>;
  return (
    typeof r.user_id === "number" &&
    typeof r.username === "string" &&
    (r.role === "moderator" || r.role === "banned") &&
    (r.assigned_by == null || typeof r.assigned_by === "number") &&
    (r.assigned_by_username == null || typeof r.assigned_by_username === "string")
  );
}

// 執行期驗證 server 回來的 JSON，不直接信任型別斷言
export function isRoleListPage(x: unknown): x is RoleListPage {
  if (typeof x !== "object" || x === null) return false;
  const p = x as Record<string, unknown>;
  if (!Array.isArray(p.roles) || !p.roles.every(isRoleEntry)) return false;
  return p.next_page_token === undefined || typeof p.next_page_token === "string";
}

// 目前登入者在本房的角色（需登入）。形狀不符一律當一般觀眾
export async function fetchMyRole(): Promise<MyRole> {
  const res = await apiFetch(`${BASE}/me`, {}, { auth: true });
  const data: unknown = await res.json();
  const role = (data as { role?: unknown } | null)?.role;
  return role === "owner" || role === "moderator" ? role : "viewer";
}

export async function fetchRoles(pageToken?: string): Promise<RoleListPage> {
  let qs = `?limit=${PAGE_LIMIT}`;
  if (pageToken) qs += `&page_token=${encodeURIComponent(pageToken)}`;
  const res = await apiFetch(`${BASE}/roles${qs}`, {}, { auth: true });
  const data: unknown = await res.json();
  if (!isRoleListPage(data)) throw new Error("unexpected response");
  return data;
}

// 指派房管或 ban（冪等，204）
export async function putRole(userId: number, role: AssignableRole): Promise<void> {
  await apiFetch(
    `${BASE}/roles/${userId}`,
    {
      method: "PUT",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ role }),
    },
    { auth: true },
  );
}

// 解除 ban 或撤銷房管（冪等，204）
export async function deleteRole(userId: number): Promise<void> {
  await apiFetch(`${BASE}/roles/${userId}`, { method: "DELETE" }, { auth: true });
}
