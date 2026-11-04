import { ApiError } from "../api/client";
import { ROOM_PUBLIC_ID } from "../config";
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
          // 房間名稱是使用者輸入，和 MessageList 一樣做輸出層防線（<bdi> + overflow hidden + overflowWrap）
          <li
            key={r.public_id}
            style={{ overflow: "hidden", overflowWrap: "anywhere" }}
          >
            {r.public_id === ROOM_PUBLIC_ID ? (
              <strong>
                <bdi>{r.room_name}</bdi>（目前）
              </strong>
            ) : (
              // 相對 href 保留 /danmaku/ base path；點了等於換網址、整頁重新載入
              <a href={"?room=" + encodeURIComponent(r.public_id)}>
                <bdi>{r.room_name}</bdi>
              </a>
            )}
          </li>
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
