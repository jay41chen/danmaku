// 彈幕列表 store（純 TS）：依 id 去重、依 id 排序、最多保留最新 500 則
export interface Message {
  id: number;
  user_id: number | null;
  username: string | null;
  content: string;
  created_at: string;
}

const MAX_MESSAGES = 500;

let items: readonly Message[] = [];
const listeners = new Set<() => void>();

export function subscribe(l: () => void): () => void {
  listeners.add(l);
  return () => listeners.delete(l);
}
export function getSnapshot(): readonly Message[] {
  return items;
}

export function isMessage(v: unknown): v is Message {
  if (typeof v !== "object" || v === null) return false;
  const m = v as Record<string, unknown>;
  return typeof m.id === "number" && typeof m.content === "string";
}

// WS 廣播與 GET /messages 會重疊，所以一律走 merge 用 id 去重
export function merge(incoming: readonly Message[]) {
  if (incoming.length === 0) return;
  const byId = new Map<number, Message>();
  for (const m of items) byId.set(m.id, m);
  for (const m of incoming) byId.set(m.id, m);
  items = [...byId.values()].sort((a, b) => a.id - b.id).slice(-MAX_MESSAGES);
  listeners.forEach((l) => l());
}
