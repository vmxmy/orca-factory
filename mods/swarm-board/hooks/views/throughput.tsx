import type { ThroughputData } from '../../types'

declare function h(type: unknown, props?: unknown, ...children: unknown[]): unknown
type TextComponent = (props: { dimColor?: boolean; children?: string }) => unknown

const STAGES = [
  ['queue', '等领取'],
  ['worker', 'worker'],
  ['gate', 'gate'],
  ['review', '审查'],
  ['merge', '开 PR 到合并'],
] as const
const REASONS = ['无可领票', 'SCOPE 冲突', '达到上限', '限流'] as const
type Stage = (typeof STAGES)[number][0]
export type ThroughputView = ThroughputData & {
  ready: boolean
  stages: { key: Stage; label: string; p50: number; p90: number; n: number }[]
  noTicket: { label: (typeof REASONS)[number]; count: number }[]
  inFlight: { avg: number; max: number }
}
export type LedgerRow = {
  outcome?: string
  finished_at?: string
  stage_secs?: Record<string, unknown>
}
export type AutomationRun = {
  createdAt?: string | number
  scheduledFor?: string | number
  startedAt?: string | number
  precheckResult?: { stdout?: string; stderr?: string; exitCode?: number } | null
}
const HOUR = 3_600_000
const timestamp = (value: unknown): number =>
  typeof value === 'number' ? value : typeof value === 'string' ? Date.parse(value) : NaN

export const buildThroughput = (input: {
  now: number
  ledger: LedgerRow[]
  runs: AutomationRun[]
  maxInFlight?: number
  inFlightAvg?: number
}): ThroughputView => {
  const { now, ledger, runs } = input
  const hourly = Array<number>(24).fill(0)
  for (const row of ledger) {
    const age = now - timestamp(row.finished_at)
    if (row.outcome === 'merged' && Number.isFinite(age) && age >= 0 && age < 24 * HOUR) {
      hourly[23 - Math.floor(age / HOUR)]++
    }
  }

  const stages: ThroughputView['stages'] = []
  for (const [key, label] of STAGES) {
    const samples: number[] = []
    for (const row of ledger) {
      // Legacy rows have no stage durations; skip them without estimating.
      if (!row.stage_secs) continue
      const seconds = row.stage_secs[key]
      if (typeof seconds === 'number' && Number.isFinite(seconds)) samples.push(seconds)
    }
    if (!samples.length) continue
    samples.sort((a, b) => a - b)
    stages.push({
      key, label,
      p50: samples[Math.ceil(0.5 * samples.length) - 1],
      p90: samples[Math.ceil(0.9 * samples.length) - 1],
      n: samples.length,
    })
  }

  const noTicket = REASONS.map(label => ({ label, count: 0 }))
  let inFlightSum = 0
  let inFlightCount = 0
  for (const run of runs) {
    const at = [run.createdAt, run.scheduledFor, run.startedAt].map(timestamp).find(Number.isFinite)
    if (at === undefined || now - at < 0 || now - at >= 24 * HOUR || !run.precheckResult) continue
    const { stdout = '', stderr = '', exitCode } = run.precheckResult
    const claimable = /\bclaimable=([^\s]*)/.exec(stdout)?.[1]
    const inFlightMatch = /\bin_flight=(\d+)\b/.exec(stdout)
    const maxMatch = /\bmax=(\d+)\b/.exec(stdout)
    const inFlight = inFlightMatch ? Number(inFlightMatch[1]) : undefined
    const max = maxMatch ? Number(maxMatch[1]) : input.maxInFlight ?? 0
    if (inFlight !== undefined && Number.isFinite(inFlight)) {
      inFlightSum += inFlight
      inFlightCount++
    }
    if (claimable) continue
    if (/rate limit/i.test(stderr) && (stderr.includes('不确定:') || exitCode === 3)) {
      noTicket[3].count++
    } else if (/\bskip ZII-\d+:[^\r\n]*scope/i.test(stderr)) {
      noTicket[1].count++
    } else if (claimable === '') {
      noTicket[inFlight !== undefined && inFlight >= max && max > 0 ? 2 : 0].count++
    }
  }
  return {
    hourly, stages, noTicket,
    ready: stages.length > 0,
    inFlight: { avg: inFlightCount ? inFlightSum / inFlightCount : 0, max: input.maxInFlight ?? 0 },
  }
}

export const renderThroughput = (Text: TextComponent, data?: ThroughputView) => {
  if (!data?.ready) return h(Text, { dimColor: true }, '数据未就绪')
  return [
    h(Text, { key: 'hourly' }, '每小时完成（最近 24 小时）：' + data.hourly.join(' · ')),
    ...data.stages.map(stage => h(Text, { key: stage.key }, `${stage.label} p50/p90：${stage.p50} / ${stage.p90} 秒`)),
    ...data.noTicket.map(reason => h(Text, { key: reason.label }, `${reason.label}：${reason.count}`)),
    h(Text, { key: 'inFlight' }, `在途均值 ${data.inFlight.avg.toFixed(1)} / 上限 ${data.inFlight.max}`),
  ]
}
