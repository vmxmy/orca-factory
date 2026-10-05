export const WATCH_POLL_MS = 5_000

export const shouldReload = (
  prev: { mtimeMs: number; size: number } | null,
  stat: { mtimeMs: number; size: number } | null,
): boolean => stat !== null && (prev === null || prev.mtimeMs !== stat.mtimeMs || prev.size !== stat.size)
