export const WATCH_POLL_MS = 5_000

export type SnapshotStat = { mtimeMs: number; size: number }

export const shouldReload = (
  prev: SnapshotStat | null | undefined,
  stat: SnapshotStat | null | undefined,
): boolean => !!prev && !!stat && (prev.mtimeMs !== stat.mtimeMs || prev.size !== stat.size)
