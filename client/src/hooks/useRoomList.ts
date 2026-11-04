import { useCallback, useEffect, useRef, useState } from "react";
import type { RoomListItem, RoomListPage } from "../api/rooms";

export type RoomPageFetcher = (pageToken?: string) => Promise<RoomListPage>;

// 分頁載入房間列表：掛載時載第 1 頁，loadMore 用上一頁給的 token 接著載並附加在後面。
// fetcher 預期是穩定的（模組層級函式）；換 fetcher 請用 key 重新掛載元件
export function useRoomList(fetcher: RoomPageFetcher) {
  const [rooms, setRooms] = useState<RoomListItem[]>([]);
  const [hasMore, setHasMore] = useState(false);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<unknown>(null);

  const fetcherRef = useRef(fetcher);
  const tokenRef = useRef<string | undefined>(undefined);
  const loadingRef = useRef(false); // 同步旗標：state 更新有延遲，擋不住連點
  // 世代計數器而非布林旗標：StrictMode 的重新掛載會沿用同一組 ref，
  // 布林會被第二次掛載重設回 false，讓第一次掛載還在飛的請求復活；計數器遞增後舊請求就永遠對不上
  const genRef = useRef(0);

  const load = useCallback(async () => {
    if (loadingRef.current) return;
    loadingRef.current = true;
    const gen = genRef.current;
    setLoading(true);
    setError(null);
    try {
      const page = await fetcherRef.current(tokenRef.current);
      if (gen !== genRef.current) return;
      tokenRef.current = page.next_page_token;
      // 防禦性：依 public_id 去重（翻頁期間有新房間插入時邊界可能重疊）
      setRooms((prev) => {
        const seen = new Set(prev.map((r) => r.public_id));
        return [...prev, ...page.rooms.filter((r) => !seen.has(r.public_id))];
      });
      // 與送請求時判斷 token 的方式一致：空字串不算還有下一頁
      setHasMore(!!page.next_page_token);
    } catch (e) {
      if (gen === genRef.current) setError(e);
    } finally {
      // 世代已變代表被清理過，loadingRef 屬於新一輪請求，不能動
      if (gen === genRef.current) {
        loadingRef.current = false;
        setLoading(false);
      }
    }
  }, []);

  useEffect(() => {
    void load();
    return () => {
      // 卸載後忽略舊請求結果；StrictMode 的 mount→unmount→mount 也會作廢舊世代並重置狀態
      // 這條規則是提醒「DOM ref 在 cleanup 時可能已經換掉」；genRef 是計數器，這裡要的就是最新值
      // eslint-disable-next-line react-hooks/exhaustive-deps
      genRef.current++;
      loadingRef.current = false;
      tokenRef.current = undefined;
      setRooms([]);
      setHasMore(false);
    };
  }, [load]);

  return { rooms, hasMore, loading, error, loadMore: load };
}
