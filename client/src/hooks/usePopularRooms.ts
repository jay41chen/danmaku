import { useEffect, useState } from "react";
import { fetchPopularRooms, type PopularRoom } from "../api/rooms";

const REFRESH_MS = 30_000;

// 熱門房間：掛載時載一次，之後每 30 秒更新；分頁不可見時暫停並中止飛行中的請求，回到可見立即更新並重新計時。
// 逾時由 apiFetch 負責（10 秒，只計到收到 response headers）；headers 之後 body 卡住的情況，由下一個 tick 透過 abortInFlight 中止。
// 回傳 null 代表「還沒有成功載入過」（載入中或首次就失敗），元件應隱藏整區；
// 成功過之後，更新失敗就保留上一份結果，下一個 tick 再試。
export function usePopularRooms(): PopularRoom[] | null {
  const [rooms, setRooms] = useState<PopularRoom[] | null>(null);

  useEffect(() => {
    // 這些狀態都放在 effect 內：StrictMode 重新掛載時會拿到全新的一份，舊請求不會復活
    let cancelled = false;
    let controller: AbortController | undefined; // 目前飛行中請求的 controller；沒有請求時為 undefined
    let timer: ReturnType<typeof setInterval> | undefined;

    // 中止飛行中的請求（分頁隱藏、回到可見要重送、unmount）
    const abortInFlight = () => {
      controller?.abort();
      controller = undefined;
    };

    const tick = async () => {
      abortInFlight(); // 先中止舊的，同一時間最多一個請求；不用 inFlight 旗標，所以不會卡死
      const mine = new AbortController();
      controller = mine;
      // controller 只負責手動中止（分頁隱藏、重送、unmount）；逾時由 apiFetch 處理，不在這裡計時
      try {
        const list = await fetchPopularRooms(mine.signal);
        // 被中止或被新請求取代的回應不能蓋掉 state
        if (!cancelled && !mine.signal.aborted) setRooms(list);
      } catch {
        /* 首次失敗維持 null（隱藏）；之後失敗（含中止、逾時）保留舊值 */
      } finally {
        if (controller === mine) controller = undefined;
      }
    };

    const start = () => {
      void tick();
      timer = setInterval(() => void tick(), REFRESH_MS);
    };
    const stop = () => {
      clearInterval(timer);
      timer = undefined;
      abortInFlight();
    };

    const onVisibility = () => {
      stop();
      if (document.visibilityState === "visible") start();
    };

    if (document.visibilityState === "visible") start();
    document.addEventListener("visibilitychange", onVisibility);
    return () => {
      cancelled = true;
      stop();
      document.removeEventListener("visibilitychange", onVisibility);
    };
  }, []);

  return rooms;
}
