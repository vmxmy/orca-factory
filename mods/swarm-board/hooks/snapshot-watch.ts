export const WATCH_POLL_MS = 5_000

export type SnapshotStat = { mtimeMs: number; size: number }

export const shouldReload = (prev: SnapshotStat | null, stat: SnapshotStat | null): boolean => {
  if (stat === null) return false
  if (prev === null) return true
  return prev.mtimeMs !== stat.mtimeMs || prev.size !== stat.size
}
