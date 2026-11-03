import { useMessages } from "../hooks/useMessages";

export function MessageList() {
  const msgs = useMessages();
  return (
    <ul>
      {msgs.map((m) => (
        // 全部當純文字渲染，不使用 dangerouslySetInnerHTML
        // user_id 為 NULL 有兩種：W4 前的匿名舊彈幕、帳號被刪除（ON DELETE SET NULL）。目前沒有刪帳號功能，先統一顯示匿名，見 ROADMAP W4b
        // 輸出層防線：<bdi> 隔離 RLO / RTL，不讓一個欄位影響外面的排列；overflow hidden 讓 Zalgo 疊字畫不出這一則，overflowWrap 讓無空白長字串換行而不是被裁掉
        <li key={m.id} style={{ overflow: "hidden", overflowWrap: "anywhere" }}>
          <bdi>{m.username ?? "匿名"}</bdi>: <bdi>{m.content}</bdi>
        </li>
      ))}
    </ul>
  );
}
