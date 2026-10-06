import type { ThroughputData as BaseThroughputData } from '../../types'

declare function h(type: unknown, props?: Record<string, unknown> | null, ...children: unknown[]): unknown
type TextComponent = (props: { dimColor?: boolean; children?: string }) => unknown

const STAGES = [
  ['queue', '等领取'],
  ['worker', 'worker'],
  ['gate', 'gate'],
  ['review', '审查'],
  ['merge', '开 PR 到合并'],
] as const
const REASONS = [
  ['noClaimable', '无可领票'],
  ['scopeConflict', 'SCOPE 冲突'],
  ['atCap', '达到上限'],
  ['rateLimited', '限流'],
] as const
type Stage = (typeof STAGES)[number][0]
export type ThroughputData = BaseThroughputData & {
  stages: Partial<Record<Stage, { p50: number; p90: number; n: number }>> | null
  noTicket: Record<(typeof REASONS)[number][0], number>
  inFlightAvg: number | null
  maxInFlight: number | null
}
export type LedgerRow = {
  outcome?: string
  finished_at?: string
  stage_secs?: Record<string, number>
}
export type Run = {
  createdAt?: string | number
  startedAt?: string | number
  precheckResult?: { stdout?: string; stderr?: string; timedOut?: boolean } | null
}
const HOUR = 3_600_000
const timestamp = (value: unknown): number =>
  typeof value === 'number' ? value : typeof value === 'string' ? Date.parse(value) : NaN

export const buildThroughput = (input: {
  now: number
  ledger: LedgerRow[]
  runs: Run[]
  maxInFlight?: number
}): ThroughputData => {
  const { now, ledger, runs } = input
  const hourly = Array<number>(24).fill(0)
  for (const row of ledger) {
    const age = now - timestamp(row.finished_at)
    if (row.outcome === 'merged' && Number.isFinite(age) && age >= 0 && age < 24 * HOUR) {
      hourly[23 - Math.floor(age / HOUR)]++
    }
  }

  const stages: NonNullable<ThroughputData['stages']> = {}
  for (const [key] of STAGES) {
    const samples: number[] = []
    for (const row of ledger) {
      // Legacy rows have no stage durations; skip them without estimating.
      if (!row.stage_secs) continue
      const seconds = row.stage_secs[key]
      if (typeof seconds === 'number' && Number.isFinite(seconds)) samples.push(seconds)
    }
    if (!samples.length) continue
    samples.sort((a, b) => a - b)
    stages[key] = {
      p50: (samples[Math.floor((samples.length - 1) / 2)] + samples[Math.floor(samples.length / 2)]) / 2,
      p90: samples[Math.ceil(0.9 * samples.length) - 1],
      n: samples.length,
    }
  }

  const noTicket = { noClaimable: 0, scopeConflict: 0, atCap: 0, rateLimited: 0 }
  let inFlightSum = 0
  let inFlightCount = 0
  let latestMax: number | null = null
  let latestAt = -Infinity
  for (const run of runs) {
    const at = timestamp(run.startedAt ?? run.createdAt)
    if (!Number.isFinite(at) || at <= now - 24 * HOUR || at > now || !run.precheckResult) continue
    const { stdout = '', stderr = '', timedOut } = run.precheckResult
    const match = /^claimable=(.*?) in_flight=(\d+) max=(\d+)/m.exec(stdout)
    if (match) {
      const inFlight = Number(match[2])
      inFlightSum += inFlight
      inFlightCount++
      if (at > latestAt) {
        latestAt = at
        latestMax = Number(match[3])
      }
    }
    if (match?.[1]) continue
    if (stderr.includes('不确定: linear rate limited') || (timedOut && stderr.includes('rate limited'))) {
      noTicket.rateLimited++
    } else if (match && match[1] === '') {
      if (Number(match[2]) >= Number(match[3])) noTicket.atCap++
      else if (/^skip \S+: scope overlaps .+/m.test(stderr)) noTicket.scopeConflict++
      else noTicket.noClaimable++
    }
  }
  return {
    hourly, stages: Object.keys(stages).length ? stages : null, noTicket,
    inFlightAvg: inFlightCount ? inFlightSum / inFlightCount : null,
    maxInFlight: input.maxInFlight ?? latestMax,
  }
}

export const renderThroughput = (Text: TextComponent, data?: ThroughputData) => {
  if (!data) return h(Text, { dimColor: true }, '数据未就绪')
  return [
    h(Text, null, '每小时完成（最近 24 小时）：' + data.hourly.join(' · ')),
    ...(data.stages ? STAGES.flatMap(([key, label]) => {
      const stage = data.stages?.[key]
      return stage ? [h(Text, null, `${label} p50/p90：${stage.p50} / ${stage.p90} 秒`)] : []
    }) : [h(Text, { dimColor: true }, '数据未就绪')]),
    ...REASONS.map(([key, label]) => h(Text, null, `${label}：${data.noTicket[key]}`)),
    h(Text, null, `在途均值 ${data.inFlightAvg?.toFixed(1) ?? '数据未就绪'} / 上限 ${data.maxInFlight ?? '数据未就绪'}`),
  ]
}
