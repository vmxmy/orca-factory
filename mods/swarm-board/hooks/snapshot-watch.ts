export const WATCH_POLL_MS = 5_000

export type FileStamp = { mtimeMs: number; size: number }

export const shouldReload = (prev: FileStamp | null, stat: FileStamp | null): boolean =>
  prev !== null && stat !== null && (prev.mtimeMs !== stat.mtimeMs || prev.size !== stat.size)

export const canStartReload = (s: { paneOpen: boolean; hasFile: boolean; reloading: boolean; ticking: boolean }): boolean =>
  s.paneOpen && s.hasFile && !s.reloading && !s.ticking

export const canStartTick = (s: { reloading: boolean; ticking: boolean }): boolean => !s.reloading && !s.ticking
