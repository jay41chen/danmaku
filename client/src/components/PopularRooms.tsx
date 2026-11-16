import { usePopularRooms } from "../hooks/usePopularRooms";
import { RoomItem } from "./RoomItem";

export function PopularRooms() {
  const rooms = usePopularRooms();
  // 還沒成功載入過（載入中或首次失敗）就整區不顯示
  if (rooms === null) return null;

  return (
    <div>
      <h3>熱門</h3>
      {rooms.length === 0 ? (
        <div>目前沒有人在看</div>
      ) : (
        <ul>
          {rooms.map((r) => (
            <RoomItem
              key={r.public_id}
              room={r}
              extra={<span>（{r.viewer_count} 人）</span>}
            />
          ))}
        </ul>
      )}
    </div>
  );
}
