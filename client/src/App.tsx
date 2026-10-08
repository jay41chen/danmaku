import { useState, useRef, useEffect } from "react";

const API_URL = import.meta.env.VITE_API_URL || "http://localhost:8080";
const WS_URL = import.meta.env.VITE_WS_URL || "ws://localhost:8080/ws";

function App() {
  const [msgs, setMsgs] = useState<string[]>([]);
  const [input, setInput] = useState("");
  const wsRef = useRef<WebSocket | null>(null);

  useEffect(() => {
    fetch(`${API_URL}/messages`)
      .then((res) => res.json())
      .then((data) => {
        setMsgs(data.map((m: { content: string }) => m.content));
      })
      .catch(console.error);

    const ws = new WebSocket(WS_URL);
    wsRef.current = ws;

    ws.onmessage = (e) => {
      setMsgs((prev) => [...prev, e.data]);
    };

    ws.onopen = () => console.log("connected");
    ws.onclose = () => console.log("disconnected");

    return () => ws.close();
  }, []);

  const send = () => {
    if (input.trim() && wsRef.current?.readyState === WebSocket.OPEN) {
      wsRef.current.send(input);
      setInput("");
    }
  };

  return (
    <div style={{ padding: 20 }}>
      <h2>Danmaku Chat</h2>
      <ul>
        {msgs.map((msg, i) => (
          <li key={i}>{msg}</li>
        ))}
      </ul>
      <input
        value={input}
        onChange={(e) => setInput(e.target.value)}
        onKeyDown={(e) => e.key === "Enter" && send()}
        placeholder="type a message..."
      />
      <button onClick={send}>Send</button>
    </div>
  );
}

export default App;
