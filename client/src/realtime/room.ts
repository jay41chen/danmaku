// 目前房間的 join 狀態（純 TS store）：pending = 還沒收到 join 回應、joined = join_ok、not_found = server 回 ws_room_not_found
export type RoomStatus = "pending" | "joined" | "not_found";

let status: RoomStatus = "pending";
const listeners = new Set<() => void>();

export function subscribe(l: () => void): () => void {
  listeners.add(l);
  return () => listeners.delete(l);
}
export function getSnapshot(): RoomStatus {
  return status;
}

export function set(next: RoomStatus) {
  if (next === status) return;
  status = next;
  listeners.forEach((l) => l());
}

// 房間在線人數（server 約每 5 秒推一次 presence）；null = 還沒收到過 / 已斷線，UI 不顯示
let presence: number | null = null;

export function getPresence(): number | null {
  return presence;
}

export function setPresence(next: number | null) {
  if (next === presence) return;
  presence = next;
  listeners.forEach((l) => l());
}

export function isPresenceCount(v: unknown): v is number {
  return typeof v === "number" && Number.isInteger(v) && v >= 0;
}
