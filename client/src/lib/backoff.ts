// equal jitter 退避：wait = base/2 + random * base/2
// base 從 baseMs 開始，每失敗一次翻倍，封頂 capMs。
// 保留一半固定值是為了避免 wait 隨機到接近 0（等於沒退避），
// 另一半隨機是為了讓大量 client 不會同時重連。
export function equalJitter(
  attempt: number,
  baseMs: number,
  capMs: number,
): number {
  const base = Math.min(capMs, baseMs * 2 ** attempt);
  return base / 2 + Math.random() * (base / 2);
}
