import { useEffect, useRef, useState } from "react";
import { ApiError } from "../api/client";
import { deleteRole } from "../api/roles";
import { useMyRole, useRosterVersion } from "../hooks/useMyRole";
import { useRoleList } from "../hooks/useRoleList";
import { runAction } from "../realtime/role";

// 房主與房管才看得到的「管理名單」：展開時才載入，列出被 ban 的人與房管
export function RoleRoster() {
  const myRole = useMyRole();
  const [open, setOpen] = useState(false);
  if (myRole !== "owner" && myRole !== "moderator") return null;
  // 角色變成一般觀眾時整個元件消失，open 也跟著重置
  return (
    <div>
      <button onClick={() => setOpen(!open)}>
        管理名單 {open ? "▲" : "▼"}
      </button>
      {open && <RosterBody isOwner={myRole === "owner"} />}
    </div>
  );
}

function RosterBody({ isOwner }: { isOwner: boolean }) {
  const { roles, hasMore, loading, error, loadMore, reload } = useRoleList(true);
  const version = useRosterVersion();
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);

  // 名單有異動（含彈幕旁按鈕造成的）就重新載入；第一次載入由 hook 負責，所以跳過初始值
  const seenVersion = useRef(version);
  useEffect(() => {
    if (seenVersion.current === version) return;
    seenVersion.current = version;
    void reload();
  }, [version, reload]);

  const remove = async (userId: number) => {
    setBusy(true);
    setActionError(null);
    // 成功時 runAction 會 bump 版本，上面的 effect 負責重新載入
    const err = await runAction(() => deleteRole(userId));
    setBusy(false);
    setActionError(err);
  };

  return (
    <div>
      {actionError !== null && <div style={{ color: "crimson" }}>{actionError}</div>}
      <ul>
        {roles.map((r) => (
          // 使用者名稱是使用者輸入，和 MessageList 一樣做輸出層防線
          <li
            key={r.user_id}
            style={{ overflow: "hidden", overflowWrap: "anywhere" }}
          >
            <bdi>{r.username}</bdi>：{r.role === "banned" ? "已 Ban" : "房管"}（指派者：
            <bdi>{r.assigned_by_username ?? "未知"}</bdi>）
            {(r.role === "banned" || isOwner) && (
              <>
                {" "}
                <button disabled={busy} onClick={() => void remove(r.user_id)}>
                  {r.role === "banned" ? "解除" : "撤銷房管"}
                </button>
              </>
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
      {!loading && error == null && roles.length === 0 && <div>名單是空的</div>}
      {error != null && !loading ? (
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
