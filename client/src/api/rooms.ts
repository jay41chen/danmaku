// 房間列表 API：GET /rooms（公開）、GET /me/rooms（需登入）
import { apiFetch } from "./client";

export interface RoomListItem {
  room_name: string;
  public_id: string;
}

export interface RoomListPage {
  rooms: RoomListItem[];
  next_page_token?: string; // 沒有下一頁時 server 不會送這個欄位
}

const PAGE_LIMIT = 20;

// 執行期驗證 server 回來的 JSON，不直接信任型別斷言
function isRoomListItem(x: unknown): x is RoomListItem {
  if (typeof x !== "object" || x === null) return false;
  const item = x as Record<string, unknown>;
  return typeof item.room_name === "string" && typeof item.public_id === "string";
}

export function isRoomListPage(x: unknown): x is RoomListPage {
  if (typeof x !== "object" || x === null) return false;
  const p = x as Record<string, unknown>;
  if (!Array.isArray(p.rooms)) return false;
  if (!p.rooms.every(isRoomListItem)) return false;
  return p.next_page_token === undefined || typeof p.next_page_token === "string";
}

async function fetchPage(
  path: string,
  auth: boolean,
  pageToken?: string,
): Promise<RoomListPage> {
  let qs = `?limit=${PAGE_LIMIT}`;
  if (pageToken) qs += `&page_token=${encodeURIComponent(pageToken)}`;
  const res = await apiFetch(path + qs, {}, { auth });
  const data: unknown = await res.json();
  if (!isRoomListPage(data)) throw new Error("unexpected response");
  return data;
}

export function fetchRooms(pageToken?: string): Promise<RoomListPage> {
  return fetchPage("/rooms", false, pageToken);
}

export function fetchMyRooms(pageToken?: string): Promise<RoomListPage> {
  return fetchPage("/me/rooms", true, pageToken);
}

export interface PopularRoom extends RoomListItem {
  viewer_count: number;
}

const POPULAR_LIMIT = 10;

function isPopularRoom(r: unknown): r is PopularRoom {
  return (
    isRoomListItem(r) &&
    typeof (r as unknown as Record<string, unknown>).viewer_count === "number"
  );
}

// GET /rooms?sort=viewers：server 已依觀看人數由多到少排好，沒有分頁
export async function fetchPopularRooms(signal?: AbortSignal): Promise<PopularRoom[]> {
  const res = await apiFetch(`/rooms?sort=viewers&limit=${POPULAR_LIMIT}`, { signal });
  const data: unknown = await res.json();
  if (
    typeof data !== "object" ||
    data === null ||
    !Array.isArray((data as Record<string, unknown>).rooms) ||
    !((data as { rooms: unknown[] }).rooms).every(isPopularRoom)
  ) {
    throw new Error("unexpected response");
  }
  return (data as { rooms: PopularRoom[] }).rooms;
}
