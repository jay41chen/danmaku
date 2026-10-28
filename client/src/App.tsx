import { useEffect, useState } from "react";
import * as session from "./auth/session";
import { AuthForm } from "./components/AuthForm";
import { Composer } from "./components/Composer";
import { MessageList } from "./components/MessageList";
import { useSession } from "./hooks/useSession";
import * as connection from "./realtime/connection";

function App() {
  const { status, user } = useSession();
  // 登出可能要等進行中的 refresh 結束（最多約 10 秒），期間按鈕停用並顯示「登出中…」
  const [loggingOut, setLoggingOut] = useState(false);

  const handleLogout = async () => {
    setLoggingOut(true);
    try {
      await session.logout();
    } finally {
      setLoggingOut(false);
    }
  };

  useEffect(() => {
    // bootstrap 內部是 idempotent 的（StrictMode 下不會打兩次 /refresh）
    void session.bootstrap();
    connection.start();
    return () => connection.stop();
  }, []);

  return (
    <div style={{ padding: 20 }}>
      {!session.isStorageAvailable && (
        <div style={{ color: "crimson" }}>
          你的瀏覽器封鎖了網站資料，登入狀態最多維持 1 小時，重新整理後也需要重新登入。請允許本站儲存資料。
        </div>
      )}
      <h2>Danmaku Chat</h2>
      {user ? (
        <div>
          {user.username} 已登入{" "}
          <button onClick={() => void handleLogout()} disabled={loggingOut}>
            {loggingOut ? "登出中…" : "登出"}
          </button>
        </div>
      ) : status === "refreshing" ? (
        <div>載入中…</div>
      ) : (
        <AuthForm />
      )}
      <MessageList />
      <Composer />
    </div>
  );
}

export default App;
