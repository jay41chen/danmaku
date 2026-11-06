import { useCallback, useEffect, useRef, useState } from "react";

// 一頁的資料：items 是這頁的項目，next 是下一頁的 token（沒有下一頁就是 undefined 或空字串）
export interface PagedResult<T> {
  items: T[];
  next?: string;
}

// 分頁載入的共用邏輯（useRoomList、useRoleList 的底層）：enabled 變 true（預設一掛載就是）時載第 1 頁，
// loadMore 用上一頁給的 token 接著載並附加在後面（依 keyOf 去重）；reload 從第 1 頁重來，
// 重新載入期間舊資料先留著，成功後才整份換掉，避免畫面閃空。
// fetcher、keyOf 只取第一次 render 的值，預期是穩定的；要換請用 key 重新掛載元件
export function usePagedList<T>(
  fetcher: (pageToken?: string) => Promise<PagedResult<T>>,
  keyOf: (item: T) => string | number,
  enabled = true,
) {
  const [items, setItems] = useState<T[]>([]);
  const [hasMore, setHasMore] = useState(false);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<unknown>(null);

  const fetcherRef = useRef(fetcher);
  const keyOfRef = useRef(keyOf);
  const tokenRef = useRef<string | undefined>(undefined);
  const loadingRef = useRef(false); // 同步旗標：state 更新有延遲，擋不住連點
  // 世代計數器而非布林旗標：StrictMode 的重新掛載會沿用同一組 ref，
  // 布林會被第二次掛載重設回 false，讓第一次掛載還在飛的請求復活；計數器遞增後舊請求就永遠對不上。
  // reload 也靠它作廢進行中的舊請求
  const genRef = useRef(0);

  const load = useCallback(async (fromStart: boolean) => {
    if (loadingRef.current && !fromStart) return;
    const gen = ++genRef.current;
    loadingRef.current = true;
    setLoading(true);
    setError(null);
    try {
      const page = await fetcherRef.current(fromStart ? undefined : tokenRef.current);
      if (gen !== genRef.current) return;
      // 空字串不算還有下一頁
      tokenRef.current = page.next || undefined;
      setItems((prev) => {
        if (fromStart) return page.items;
        // 防禦性去重（翻頁期間有新資料插入時邊界可能重疊）
        const seen = new Set(prev.map(keyOfRef.current));
        return [...prev, ...page.items.filter((x) => !seen.has(keyOfRef.current(x)))];
      });
      setHasMore(!!page.next);
    } catch (e) {
      if (gen === genRef.current) setError(e);
    } finally {
      // 世代已變代表被清理過或被 reload 取代，loadingRef 屬於新一輪請求，不能動
      if (gen === genRef.current) {
        loadingRef.current = false;
        setLoading(false);
      }
    }
  }, []);

  useEffect(() => {
    if (!enabled) return;
    void load(true);
    return () => {
      // 收合 / 卸載：作廢進行中的請求並清空，下次重新載入（StrictMode 的 mount→unmount→mount 也走這裡）
      // 這條規則是提醒「DOM ref 在 cleanup 時可能已經換掉」；genRef 是計數器，這裡要的就是最新值
      // eslint-disable-next-line react-hooks/exhaustive-deps
      genRef.current++;
      loadingRef.current = false;
      tokenRef.current = undefined;
      setItems([]);
      setHasMore(false);
      setError(null);
      setLoading(false);
    };
  }, [enabled, load]);

  const loadMore = useCallback(() => load(false), [load]);
  const reload = useCallback(() => load(true), [load]);
  return { items, hasMore, loading, error, loadMore, reload };
}
