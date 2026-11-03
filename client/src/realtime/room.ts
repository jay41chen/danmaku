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
