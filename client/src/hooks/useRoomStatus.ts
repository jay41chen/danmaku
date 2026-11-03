import { useSyncExternalStore } from "react";
import * as room from "../realtime/room";

export function useRoomStatus() {
  return useSyncExternalStore(room.subscribe, room.getSnapshot);
}
