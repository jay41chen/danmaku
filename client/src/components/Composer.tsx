import { useState } from "react";
import { ApiError, apiFetch } from "../api/client";
import { ROOM_MESSAGES_PATH } from "../config";
import { useSession } from "../hooks/useSession";
import * as messages from "../realtime/messages";

const MAX_LEN = 100;

export function Composer() {
  const { status, user } = useSession();
  const [input, setInput] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const canSend = status !== "anonymous" && user !== null;

  const send = async () => {
    const content = input.trim();
    if (!content) return;
    // 以 Unicode code point 計；只是 UX，server 才是權威
    if ([...content].length > MAX_LEN) {
      setError(`內容最多 ${MAX_LEN} 個字元`);
      return;
    }
    setBusy(true);
    setError("");
    try {
      const res = await apiFetch(
        ROOM_MESSAGES_PATH,
        {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ content }),
        },
        { auth: true },
      );
      const msg: unknown = await res.json();
      // WS 廣播也會送到，這裡先加是為了 WS 斷線時自己的訊息也看得到；merge 會依 id 去重
      if (messages.isMessage(msg)) messages.merge([msg]);
      setInput("");
    } catch (e) {
      if (e instanceof ApiError) {
        if (e.status === 401) setError("登入已過期，請重新登入");
        else if (e.status === 404) setError("房間不存在");
        else if (e.status === 429)
          setError(
            e.retryAfter !== null
              ? `發送太頻繁，請 ${e.retryAfter} 秒後再試`
              : "發送太頻繁，請稍後再試",
          );
        else setError(e.message);
      } else {
        setError("網路錯誤，請稍後再試");
      }
    } finally {
      setBusy(false);
    }
  };

  return (
    <div>
      <input
        value={input}
        disabled={!canSend}
        onChange={(e) => setInput(e.target.value)}
        onKeyDown={(e) => {
          // 組字中（IME）按 Enter 是選字，不送出；keyCode 229 是給 Safari（它的 isComposing 在 keydown 已是 false）
          if (e.nativeEvent.isComposing || e.keyCode === 229) return;
          if (e.key === "Enter" && !busy) void send();
        }}
        placeholder={canSend ? "type a message..." : "登入後才能發言"}
      />
      <button disabled={!canSend || busy} onClick={() => void send()}>
        Send
      </button>
      {!canSend && <small> 登入後才能發言</small>}
      {error && <span style={{ color: "crimson" }}> {error}</span>}
    </div>
  );
}
