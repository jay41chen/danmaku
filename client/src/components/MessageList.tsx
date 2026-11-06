import { useState } from "react";
import { putRole } from "../api/roles";
import { useMessages } from "../hooks/useMessages";
import { useMyRole } from "../hooks/useMyRole";
import { useSession } from "../hooks/useSession";
import * as messages from "../realtime/messages";
import { runAction } from "../realtime/role";

export function MessageList() {
  const msgs = useMessages();
  const myRole = useMyRole();
  const myId = useSession().user?.id ?? null;
  const [busy, setBusy] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);

  const canBan = myRole === "owner" || myRole === "moderator";
  const canAssign = myRole === "owner";

  // 對某使用者執行角色操作；結果文字由 runAction 統一處理（403 會重抓角色）。
  // Ban 成功後 WS 的 user_hidden 不保證送達，所以本地也先移除他的彈幕
  const act = async (fn: () => Promise<void>, hideUserId?: number) => {
    setBusy(true);
    setActionError(null);
    const err = await runAction(fn);
    setBusy(false);
    setActionError(err);
    if (err === null && hideUserId !== undefined) messages.removeByUser(hideUserId);
  };

  return (
    <>
      {actionError !== null && <div style={{ color: "crimson" }}>{actionError}</div>}
      <ul>
        {msgs.map((m) => {
          // 沒有 user_id（匿名舊彈幕）或是自己的彈幕不顯示按鈕；按鈕只是 UX，真正的檢查在 server
          const uid = m.user_id;
          const showButtons = uid !== null && uid !== myId && canBan;
          return (
            // 全部當純文字渲染，不使用 dangerouslySetInnerHTML
            // user_id 為 NULL 有兩種：W4 前的匿名舊彈幕、帳號被刪除（ON DELETE SET NULL）。目前沒有刪帳號功能，先統一顯示匿名，見 ROADMAP W4b
            // 輸出層防線：<bdi> 隔離 RLO / RTL，不讓一個欄位影響外面的排列；overflow hidden 讓 Zalgo 疊字畫不出這一則，overflowWrap 讓無空白長字串換行而不是被裁掉
            <li key={m.id} style={{ overflow: "hidden", overflowWrap: "anywhere" }}>
              <bdi>{m.username ?? "匿名"}</bdi>: <bdi>{m.content}</bdi>
              {showButtons && (
                <>
                  {" "}
                  <button
                    disabled={busy}
                    onClick={() => void act(() => putRole(uid, "banned"), uid)}
                  >
                    Ban
                  </button>
                  {canAssign && (
                    <>
                      {" "}
                      <button
                        disabled={busy}
                        onClick={() => void act(() => putRole(uid, "moderator"))}
                      >
                        設為房管
                      </button>
                    </>
                  )}
                </>
              )}
            </li>
          );
        })}
      </ul>
    </>
  );
}
