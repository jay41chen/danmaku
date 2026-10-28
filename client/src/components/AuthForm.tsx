import { useState } from "react";
import * as session from "../auth/session";

export function AuthForm() {
  const [mode, setMode] = useState<"login" | "register">("login");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  const submit = async () => {
    if (mode === "register") {
      // 只是 UX 提示，server 才是權威
      if (!/^\w{3,32}$/.test(username)) {
        setError("帳號需為 3–32 個英數字或底線");
        return;
      }
      const bytes = new TextEncoder().encode(password).length;
      if (bytes < 8 || bytes > 72) {
        setError("密碼需為 8–72 bytes");
        return;
      }
    }
    setBusy(true);
    setError("");
    try {
      if (mode === "login") await session.login(username, password);
      else await session.register(username, password);
    } catch (e) {
      setError(e instanceof Error ? e.message : "發生錯誤");
    } finally {
      setBusy(false);
    }
  };

  return (
    <form
      onSubmit={(e) => {
        e.preventDefault();
        void submit();
      }}
      style={{ display: "flex", gap: 8, flexWrap: "wrap", alignItems: "center" }}
    >
      <input
        value={username}
        onChange={(e) => setUsername(e.target.value)}
        placeholder="帳號"
        autoComplete="username"
      />
      <input
        type="password"
        value={password}
        onChange={(e) => setPassword(e.target.value)}
        placeholder="密碼"
        autoComplete={mode === "login" ? "current-password" : "new-password"}
      />
      <button type="submit" disabled={busy}>
        {mode === "login" ? "登入" : "註冊"}
      </button>
      <button
        type="button"
        onClick={() => {
          setMode(mode === "login" ? "register" : "login");
          setError("");
        }}
      >
        切換為{mode === "login" ? "註冊" : "登入"}
      </button>
      {mode === "register" && (
        <small>帳號 3–32 個英數字或底線；密碼 8–72 bytes</small>
      )}
      {error && <span style={{ color: "crimson" }}>{error}</span>}
    </form>
  );
}
