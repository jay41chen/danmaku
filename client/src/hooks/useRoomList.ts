import type { RoomListItem, RoomListPage } from "../api/rooms";
import { usePagedList } from "./usePagedList";

export type RoomPageFetcher = (pageToken?: string) => Promise<RoomListPage>;

const roomKey = (r: RoomListItem) => r.public_id;

// 分頁載入房間列表：掛載時載第 1 頁，loadMore 用上一頁給的 token 接著載並附加在後面（細節見 usePagedList）。
// fetcher 預期是穩定的（模組層級函式）；換 fetcher 請用 key 重新掛載元件
export function useRoomList(fetcher: RoomPageFetcher) {
  const { items, ...rest } = usePagedList(
    async (pageToken) => {
      const page = await fetcher(pageToken);
      return { items: page.rooms, next: page.next_page_token };
    },
    roomKey,
  );
  return { rooms: items, ...rest };
}
