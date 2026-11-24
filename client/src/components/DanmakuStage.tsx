import { useEffect, useRef } from "react";
import * as danmaku from "../realtime/danmaku";
import type { LaneItem } from "../realtime/danmaku";

type Active = { el: HTMLElement; anim: Animation; userId: number | null };

// 彈幕舞台：飛行中的項目不放 React state，全由 effect 內以 DOM + Web Animations 命令式管理
export function DanmakuStage() {
  const stageRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    const stage = stageRef.current;
    if (stage === null) return;
    const lanes: (LaneItem | null)[] = Array.from({ length: danmaku.LANE_COUNT }, () => null);
    const active = new Set<Active>();

    const sink: danmaku.Sink = {
      launch(m, self) {
        const W = stage.clientWidth;
        const el = document.createElement("bdi");
        el.textContent = m.content; // 絕不用 innerHTML
        const s = el.style;
        s.position = "absolute";
        s.left = "0";
        s.top = "0";
        s.whiteSpace = "nowrap";
        s.lineHeight = `${danmaku.LANE_HEIGHT}px`;
        s.color = "var(--text-h)";
        s.textShadow = "0 0 2px var(--bg, #000), 0 0 4px var(--bg, #000)";
        s.pointerEvents = "none";
        s.visibility = "hidden";
        s.transform = `translateX(${W}px)`;
        // 輸出層防護：Zalgo 之類的疊字不能畫出自己的框
        s.maxHeight = `${danmaku.LANE_HEIGHT}px`;
        s.overflow = "hidden";
        if (self) {
          s.outline = "1px solid var(--accent)";
          s.outlineOffset = "-1px";
          s.padding = "0 6px";
          s.borderRadius = "6px";
        }
        stage.appendChild(el);
        const w = el.offsetWidth;
        const t = performance.now();
        const lane = danmaku.pickLane(lanes, w, t, W, danmaku.DURATION_MS, self);
        if (lane === null) {
          el.remove();
          return;
        }
        lanes[lane] = { start: t, width: w };
        s.top = `${lane * danmaku.LANE_HEIGHT}px`;
        s.visibility = "visible";
        const anim = el.animate(
          [{ transform: `translateX(${W}px)` }, { transform: `translateX(${-w}px)` }],
          { duration: danmaku.DURATION_MS, easing: "linear" },
        );
        const item: Active = { el, anim, userId: m.user_id };
        active.add(item);
        anim.onfinish = () => {
          el.remove();
          active.delete(item);
        };
      },
      // 軌道狀態可能仍參照已移除的項目；這樣只會偏保守（多留一點空位），可接受
      removeByUser(uid) {
        for (const item of [...active]) {
          if (item.userId !== uid) continue;
          item.anim.cancel();
          item.el.remove();
          active.delete(item);
        }
      },
    };

    danmaku.setSink(sink);
    return () => {
      danmaku.setSink(null);
      for (const item of active) {
        item.anim.cancel();
        item.el.remove();
      }
      active.clear();
    };
  }, []);

  // 飛行文字和下方列表重複，不讓螢幕閱讀器念兩次
  return (
    <div
      ref={stageRef}
      aria-hidden="true"
      style={{
        position: "relative",
        overflow: "hidden",
        width: "100%",
        height: danmaku.LANE_COUNT * danmaku.LANE_HEIGHT,
        border: "1px solid var(--border)",
      }}
    />
  );
}
