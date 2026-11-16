import type { ReactNode } from "react";
import { ROOM_PUBLIC_ID } from "../config";
import type { RoomListItem } from "../api/rooms";

// 房間列表的一列（RoomList、PopularRooms 共用）；extra 是接在名稱後面的附註（例如觀看人數）
export function RoomItem({
  room,
  extra,
}: {
  room: RoomListItem;
  extra?: ReactNode;
}) {
  return (
    // 房間名稱是使用者輸入，和 MessageList 一樣做輸出層防線（<bdi> + overflow hidden + overflowWrap）
    <li style={{ overflow: "hidden", overflowWrap: "anywhere" }}>
      {room.public_id === ROOM_PUBLIC_ID ? (
        <strong>
          <bdi>{room.room_name}</bdi>（目前）
        </strong>
      ) : (
        // 相對 href 保留 /danmaku/ base path；點了等於換網址、整頁重新載入
        <a href={"?room=" + encodeURIComponent(room.public_id)}>
          <bdi>{room.room_name}</bdi>
        </a>
      )}
      {extra}
    </li>
  );
}
