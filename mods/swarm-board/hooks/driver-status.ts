type DriverLimit = { remaining: number; reset: string }
export type DriverStatus = { state: 'unknown' | 'stale' } | {
  state: 'ok'
  generatedAt: string | number
  tickets: { repo: string; identifier: string; owner: string; phase: string; stage?: string }[]
  globalPause: { active: boolean; durationSeconds: number }
  linear: { requests: number; complexity: number; limits: { repo: string; requests: DriverLimit; complexity: DriverLimit }[] }
}
const record = (value: unknown): value is Record<string, unknown> => value !== null && typeof value === 'object'
const driverLimit = (value: unknown): value is DriverLimit =>
  record(value) && typeof value.remaining === 'number' && Number.isFinite(value.remaining) && typeof value.reset === 'string'
export const parseDriverStatus = (value: unknown): DriverStatus | undefined => {
  if (!record(value)) return undefined
  if (value.state === 'unknown' || value.state === 'stale') return { state: value.state }
  if (!Array.isArray(value.tickets) || !value.tickets.every(t => record(t) && ['repo', 'identifier', 'owner', 'phase'].every(k => typeof t[k] === 'string'))
      || (typeof value.generatedAt !== 'number' && typeof value.generatedAt !== 'string')) return undefined
  if (value.state !== 'ok' || !record(value.globalPause) || !record(value.linear)) return undefined
  const pause = value.globalPause, usage = value.linear
  if (typeof pause.active !== 'boolean' || typeof pause.durationSeconds !== 'number'
      || !Number.isFinite(pause.durationSeconds) || pause.durationSeconds < 0
      || typeof usage.requests !== 'number' || !Number.isFinite(usage.requests)
      || typeof usage.complexity !== 'number' || !Number.isFinite(usage.complexity)
      || !Array.isArray(usage.limits)) return undefined
  const limits: { repo: string; requests: DriverLimit; complexity: DriverLimit }[] = []
  for (const limit of usage.limits) {
    if (!record(limit) || typeof limit.repo !== 'string' || !driverLimit(limit.requests) || !driverLimit(limit.complexity)) return undefined
    limits.push({ repo: limit.repo, requests: limit.requests, complexity: limit.complexity })
  }
  return {
    state: 'ok', generatedAt: value.generatedAt, tickets: value.tickets, globalPause: { active: pause.active, durationSeconds: pause.durationSeconds },
    linear: { requests: usage.requests, complexity: usage.complexity, limits },
  }
}

// Precheck remains the fallback for absent, malformed, future, or expired cache.
export function pickStatusSource(status: unknown, _snapshot: unknown, now: number): 'driver' | 'snapshot' {
  if (!record(status)) return 'snapshot'
  const at = typeof status.generatedAt === 'number' ? status.generatedAt * 1000
    : typeof status.generatedAt === 'string' ? Date.parse(status.generatedAt) : NaN
  return Number.isFinite(at) && at > 0 && now >= at && now - at <= 120_000 ? 'driver' : 'snapshot'
}
