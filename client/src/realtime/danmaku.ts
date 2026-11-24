// 彈幕層的核心邏輯（純 TS）：決定哪些訊息會飛、飛在哪一條軌道。
// 會飛的：即時 WS 訊息，以及自己 POST 的回應；不會飛的：歷史撈取 / resync 補洞撈到的訊息（它們只進列表）。
// 自己的訊息會經由 POST 和 WS 各到一次，所以用 id 去重，只飛一次。
// 軌道全滿時直接丟掉是刻意的設計，與 ADR 0018 同樣是送達盡力而為的取向。
import * as session from "../auth/session";
import type { Message } from "./messages";

export const DURATION_MS = 8000;
export const LANE_HEIGHT = 32;
export const LANE_COUNT = 10;
export const ENTRY_GAP_PX = 16;
export const LAUNCHED_MAX = 500;

// 某條軌道上「最後發射」的那一則：起飛時間與寬度
export type LaneItem = { start: number; width: number };

// 判斷新項目（寬 w2、在 t2 起飛）能不能放進 prev 所在的軌道。
// 固定時長模型：每則從右緣外飛到左緣外都花 T，所以速度 v(w) = (W + w) / T，越長的字飛越快。
// 進場條件：前一則的尾巴已完全進場並留出間隔（v1 * 經過時間 >= 前一則寬度 + ENTRY_GAP_PX），起點才不會重疊。
// 出場條件：新項目的頭碰到左緣的時間不早於前一則的尾巴離開左緣的時間（t2 + W / v2 >= prev.start + T）。
// 兩者都是線性運動，起點與前一則離場這兩個時間點都不重疊，中間就不可能重疊（同 DanmakuFlameMaster 的 willHitInDuration）。
// 只需檢查每條軌道的最後一則：更早的項目都在最後一則前面、也更早離場；新項目進場時不撞最後一則、離場前追不上最後一則，就不會撞到更前面的。前提是軌道內每一則都經 canPlace 放入；自己的訊息被 force 硬塞時不保證（可接受，force 本來就允許重疊）。
export function canPlace(prev: LaneItem | null, w2: number, t2: number, W: number, T: number): boolean {
  if (prev === null) return true;
  if (t2 >= prev.start + T) return true;
  const v1 = (W + prev.width) / T;
  const v2 = (W + w2) / T;
  const entry = v1 * (t2 - prev.start) >= prev.width + ENTRY_GAP_PX;
  const exit = t2 + W / v2 >= prev.start + T;
  return entry && exit;
}

// 挑軌道：回傳編號最小且放得下的軌道；都放不下時，force（自己的訊息）改塞進最早發射的那條，否則回傳 null（丟掉）
export function pickLane(
  lanes: readonly (LaneItem | null)[],
  w2: number,
  t2: number,
  W: number,
  T: number,
  force: boolean,
): number | null {
  for (let i = 0; i < lanes.length; i++) {
    if (canPlace(lanes[i], w2, t2, W, T)) return i;
  }
  if (!force) return null;
  let best = 0;
  let bestStart = Infinity;
  for (let i = 0; i < lanes.length; i++) {
    const s = lanes[i]?.start ?? -Infinity;
    if (s < bestStart) {
      bestStart = s;
      best = i;
    }
  }
  return lanes.length === 0 ? null : best;
}

export type Sink = {
  launch(m: Message, self: boolean): void;
  removeByUser(userId: number): void;
};

let sink: Sink | null = null;
const launched = new Set<number>();
const launchedQueue: number[] = [];

// 舞台元件掛載時註冊、卸載時傳 null 取消
export function setSink(s: Sink | null) {
  sink = s;
}

// 發射一則彈幕：沒有舞台、分頁在背景、或這個 id 已經飛過就忽略
export function launch(m: Message) {
  if (sink === null || document.hidden || launched.has(m.id)) return;
  launched.add(m.id);
  launchedQueue.push(m.id);
  if (launchedQueue.length > LAUNCHED_MAX) {
    const old = launchedQueue.shift();
    if (old !== undefined) launched.delete(old);
  }
  const self = m.user_id !== null && m.user_id === session.getSnapshot().user?.id;
  sink.launch(m, self);
}

// 移除某使用者正在飛的彈幕（被 ban 時）
export function removeByUser(userId: number) {
  sink?.removeByUser(userId);
}
