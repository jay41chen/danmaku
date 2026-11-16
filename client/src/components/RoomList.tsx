import { ApiError } from "../api/client";
import { RoomItem } from "./RoomItem";
import { useRoomList, type RoomPageFetcher } from "../hooks/useRoomList";

export function RoomList({
  title,
  fetcher,
}: {
  title: string;
  fetcher: RoomPageFetcher;
}) {
  const { rooms, hasMore, loading, error, loadMore } = useRoomList(fetcher);

  return (
    <div>
      <h3>{title}</h3>
      <ul>
        {rooms.map((r) => (
          <RoomItem key={r.public_id} room={r} />
        ))}
      </ul>
      {loading && <div>載入中…</div>}
      {error != null && (
        <div style={{ color: "crimson" }}>
          {error instanceof ApiError ? error.message : "網路錯誤，請稍後再試"}
        </div>
      )}
      {!loading && error == null && rooms.length === 0 && <div>還沒有房間</div>}
      {error != null && !loading ? (
        // 失敗後重試：第 1 頁失敗時 token 仍是 undefined，會重抓第 1 頁；後面頁失敗則重抓該頁
        <button onClick={() => void loadMore()}>重試</button>
      ) : (
        hasMore && (
          <button onClick={() => void loadMore()} disabled={loading}>
            載入更多
          </button>
        )
      )}
    </div>
  );
}
