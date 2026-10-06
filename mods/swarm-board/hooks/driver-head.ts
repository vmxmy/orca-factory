type DriverHeadStatus = { state: string; generatedAt?: string; tickets?: number }

export const driverHeadText = (status: DriverHeadStatus | undefined, nowMs: number): string | null => {
  if (status?.state !== 'ok' || typeof status.generatedAt !== 'string' || typeof status.tickets !== 'number') return null
  const generatedAt = Date.parse(status.generatedAt)
  if (!Number.isFinite(generatedAt)) return null
  const seconds = Math.max(0, Math.floor((nowMs - generatedAt) / 1000))
  const elapsed = seconds > 60 ? Math.floor(seconds / 60) + ' 分钟前' : seconds + ' 秒前'
  return '🤖 驱动器 · 在做 ' + status.tickets + ' · 更新于 ' + elapsed
}
