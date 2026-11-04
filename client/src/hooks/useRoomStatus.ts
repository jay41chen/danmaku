import { useSyncExternalStore } from "react";
import * as room from "../realtime/room";

export function useRoomStatus() {
  return useSyncExternalStore(room.subscribe, room.getSnapshot);
}

// 在線人數和 join 狀態同屬 room store、共用 subscribe；回傳型別不同所以多一個 hook，不改動 useRoomStatus 的回傳值
export function useRoomPresence() {
  return useSyncExternalStore(room.subscribe, room.getPresence);
}
