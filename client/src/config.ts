// API / WebSocket 位址集中在這裡，避免各處各自寫一份 fallback
export const API_URL: string =
  import.meta.env.VITE_API_URL || "http://localhost:8080";
export const WS_URL: string =
  import.meta.env.VITE_WS_URL || "ws://localhost:8080/ws";
