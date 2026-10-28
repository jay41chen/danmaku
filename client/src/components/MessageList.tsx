import { useMessages } from "../hooks/useMessages";

export function MessageList() {
  const msgs = useMessages();
  return (
    <ul>
      {msgs.map((m) => (
        // 全部當純文字渲染，不使用 dangerouslySetInnerHTML
        // user_id 為 NULL 有兩種：W4 前的匿名舊彈幕、帳號被刪除（ON DELETE SET NULL）。目前沒有刪帳號功能，先統一顯示匿名，見 ROADMAP W4b
        <li key={m.id}>
          {m.username ?? "匿名"}: {m.content}
        </li>
      ))}
    </ul>
  );
}
