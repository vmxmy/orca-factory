export type LedgerRow = { outcome?: string; finished_at?: string; stage_secs?: unknown }
type Stage = 'queue' | 'worker' | 'gate' | 'review' | 'merge'
type StageStats = { p50: number | null; p90: number | null; n: number }
export type ThroughputData = {
  /** Oldest first; the last bucket is (now - 1h, now]. */
  hourly: number[]
  stages: Record<Stage, StageStats>
  stagesReady: boolean
  noTicket: { rateLimited: number; atLimit: number; scopeConflict: number; noClaimable: number }
  inFlight: { avg: number | null; max: number | null }
}

const HOUR = 3_600_000
const STAGES: readonly Stage[] = ['queue', 'worker', 'gate', 'review', 'merge']
const isObject = (value: unknown): value is Record<string, unknown> =>
  value !== null && typeof value === 'object' && !Array.isArray(value)

// Match register.tsx: ISO, UTC without a timezone, or epoch milliseconds.
const timestamp = (value: unknown): number => {
  if (typeof value === 'number') return value
  if (typeof value !== 'string' || value === '') return NaN
  if (/^\d+$/.test(value)) return Number(value)
  return Date.parse(/^\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}$/.test(value) ? value.replace(' ', 'T') + 'Z' : value)
}
const withinDay = (age: number): boolean => Number.isFinite(age) && age >= 0 && age < 24 * HOUR
const counter = (stdout: string, key: 'in_flight' | 'max'): number | null => {
  const match = new RegExp('\\b' + key + '=(\\d+)').exec(stdout)
  if (!match) return null
  const value = Number(match[1])
  return Number.isFinite(value) ? value : null
}
const summarize = (values: number[]): StageStats => {
  values.sort((a, b) => a - b)
  const n = values.length
  // Nearest rank: rank = ceil(p * n), with ranks starting at one.
  return { p50: n ? values[Math.ceil(0.5 * n) - 1] : null, p90: n ? values[Math.ceil(0.9 * n) - 1] : null, n }
}

export const buildThroughput = (input: {
  now: number
  ledger: readonly LedgerRow[]
  runs: readonly unknown[]
  maxInFlight?: number
}): ThroughputData => {
  const hourly = Array<number>(24).fill(0)
  const samples: Record<Stage, number[]> = { queue: [], worker: [], gate: [], review: [], merge: [] }
  for (const row of input.ledger) {
    if (row.outcome === 'merged' && typeof row.finished_at === 'string') {
      const age = input.now - Date.parse(row.finished_at)
      if (withinDay(age)) hourly[23 - Math.floor(age / HOUR)]++
    }
    if (!isObject(row.stage_secs)) continue
    for (const stage of STAGES) {
      const value = row.stage_secs[stage]
      if (typeof value === 'number' && Number.isFinite(value)) samples[stage].push(value)
    }
  }
  const stages = {
    queue: summarize(samples.queue), worker: summarize(samples.worker), gate: summarize(samples.gate),
    review: summarize(samples.review), merge: summarize(samples.merge),
  }
  const noTicket = { rateLimited: 0, atLimit: 0, scopeConflict: 0, noClaimable: 0 }
  let inFlightTotal = 0
  let inFlightCount = 0
  let latestMax: number | null = null
  let latestMaxAt = -Infinity
  for (const run of input.runs) {
    if (!isObject(run)) continue
    const at = timestamp(run.scheduledFor ?? run.dispatchedAt ?? run.startedAt ?? run.created_at ?? '')
    if (!withinDay(input.now - at) || !isObject(run.precheckResult)) continue
    const stdout = typeof run.precheckResult.stdout === 'string' ? run.precheckResult.stdout : ''
    const stderr = typeof run.precheckResult.stderr === 'string' ? run.precheckResult.stderr : ''
    const inFlight = counter(stdout, 'in_flight')
    const max = counter(stdout, 'max')
    if (inFlight !== null) { inFlightTotal += inFlight; inFlightCount++ }
    if (max !== null && at > latestMaxAt) { latestMax = max; latestMaxAt = at }
    const claim = /^claimable=([^\s]*)/m.exec(stdout)
    if (claim && claim[1] !== '') continue
    if (/不确定: linear rate limited|Rate limit/.test(stderr)) noTicket.rateLimited++
    else if (claim) {
      if (inFlight !== null && max !== null && inFlight >= max) noTicket.atLimit++
      else if (/skip ZII-\d+: scope overlaps/.test(stderr)) noTicket.scopeConflict++
      else noTicket.noClaimable++
    }
  }
  return {
    hourly, stages, stagesReady: STAGES.some(stage => stages[stage].n > 0), noTicket,
    inFlight: { avg: inFlightCount ? inFlightTotal / inFlightCount : null, max: input.maxInFlight ?? latestMax },
  }
}

// Keep the board's renderThroughput(Text) call valid without importing its host runtime.
// A caller supplying computed data gets plain text that React can render as a child.
export const renderThroughput = (_Text: unknown, data?: ThroughputData): string => {
  if (!data) return '数据未就绪'
  const lines = ['每小时完成（过去 24 小时，从旧到新）：' + data.hourly.join(' ')]
  if (!data.stagesReady) lines.push('阶段耗时：数据未就绪')
  else for (const stage of STAGES) {
    const stats = data.stages[stage]
    lines.push(`${stage}：p50 ${stats.p50 ?? '—'}s · p90 ${stats.p90 ?? '—'}s · n=${stats.n}`)
  }
  lines.push(
    `无可领票：${data.noTicket.noClaimable}`,
    `SCOPE 冲突：${data.noTicket.scopeConflict}`,
    `达到上限：${data.noTicket.atLimit}`,
    `限流：${data.noTicket.rateLimited}`,
    `在途均值 / 上限：${data.inFlight.avg ?? '—'} / ${data.inFlight.max ?? '—'}`,
  )
  return lines.join('\n')
}
