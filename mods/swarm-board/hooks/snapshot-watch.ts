export const WATCH_POLL_MS = 5_000

type FileStat = { mtimeMs: number; size: number }

export function shouldReload(prev: FileStat | null, stat: FileStat | null): boolean {
  if (stat === null) return false
  if (prev === null) return true
  return prev.mtimeMs !== stat.mtimeMs || prev.size !== stat.size
}
