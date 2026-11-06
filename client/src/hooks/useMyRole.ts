import { useEffect, useSyncExternalStore } from "react";
import * as role from "../realtime/role";
import { useRoomStatus } from "./useRoomStatus";
import { useSession } from "./useSession";

export function useMyRole() {
  return useSyncExternalStore(role.subscribe, role.getSnapshot).role;
}

export function useRosterVersion() {
  return useSyncExternalStore(role.subscribe, role.getSnapshot).rosterVersion;
}

// 在 App 呼叫一次：登入 / 登出 / 換帳號時重抓角色；匿名或房間不存在不發請求（視為一般觀眾）
export function useRoleSync() {
  const userId = useSession().user?.id ?? null;
  const roomMissing = useRoomStatus() === "not_found";
  useEffect(() => {
    role.reset();
    if (userId !== null && !roomMissing) void role.refresh();
  }, [userId, roomMissing]);
}
