import { useSyncExternalStore } from "react";
import * as session from "../auth/session";

export function useSession() {
  return useSyncExternalStore(session.subscribe, session.getSnapshot);
}
