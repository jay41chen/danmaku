import { useSyncExternalStore } from "react";
import * as messages from "../realtime/messages";

export function useMessages() {
  return useSyncExternalStore(messages.subscribe, messages.getSnapshot);
}
