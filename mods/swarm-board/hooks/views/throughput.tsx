import type { ThroughputData as BaseThroughputData } from '../../types'

declare function h(type: unknown, props?: unknown, ...children: unknown[]): unknown
type TextComponent = (props: { dimColor?: boolean; children?: string }) => unknown

// Canonical ledger keys: driver/internal/ledger/ledger_test.go, TestStageSecsAppend.
const STAGES = [
  ['queue', '等领取'],
  ['worker', 'worker'],
  ['gate', 'gate'],
  ['review', '审查'],
  ['merge', '开 PR 到合并'],
] as const
type Stage = (typeof STAGES)[number][0]
type Reason = '无可领票' | 'SCOPE 冲突' | '达到上限' | '限流'
export type ThroughputData = BaseThroughputData & {
  stages: { key: Stage; label: string; median: number; p90: number }[]
  reasons: Record<Reason, number>
  inFlightAvg: number
  maxInFlight: number
}
type LedgerRow = { outcome?: string; finished_at?: string; stage_secs?: Record<string, number> }
type Run = {
  startedAt?: string | number
  finishedAt?: string | number
  createdAt?: string | number
  precheckResult?: { stdout?: string; stderr?: string }
}
const HOUR = 3_600_000
const isRecord = (value: unknown): value is Record<string, unknown> =>
  value !== null && typeof value === 'object' && !Array.isArray(value)
const timestamp = (value: unknown): number =>
  typeof value === 'number' ? value : typeof value === 'string' ? Date.parse(value) : NaN

export const buildThroughput = (input: {
  now: number; ledger: LedgerRow[]; runs: Run[]; maxInFlight?: number
}): ThroughputData => {
  const { now, ledger, runs } = input
  const start = now - 24 * HOUR
  const hourly = Array<number>(24).fill(0)
  for (const row of ledger) {
    const finished = typeof row.finished_at === 'string' ? Date.parse(row.finished_at) : NaN
    // Right-closed rolling buckets: the final bucket is (now - 1h, now].
    if (row.outcome === 'merged' && Number.isFinite(finished) && finished > start && finished <= now) {
      hourly[Math.ceil((finished - start) / HOUR) - 1]++
    }
  }

  const stages: ThroughputData['stages'] = []
  for (const [key, label] of STAGES) {
    const samples: number[] = []
    for (const row of ledger) {
      if (!isRecord(row.stage_secs)) continue
      const seconds = row.stage_secs[key]
      if (typeof seconds === 'number' && Number.isFinite(seconds)) samples.push(seconds)
    }
    if (!samples.length) continue
    const sorted = [...samples].sort((a, b) => a - b)
    const middle = Math.floor(sorted.length / 2)
    stages.push({
      key, label,
      median: sorted.length % 2 ? sorted[middle] : (sorted[middle - 1] + sorted[middle]) / 2,
      p90: sorted[Math.ceil(0.9 * sorted.length) - 1],
    })
  }

  const reasons: ThroughputData['reasons'] = { '无可领票': 0, 'SCOPE 冲突': 0, '达到上限': 0, '限流': 0 }
  let inFlightSum = 0
  let inFlightCount = 0
  let latestMaxAt = -Infinity
  let latestMax = 0
  for (const run of runs) {
    const at = [run.startedAt, run.finishedAt, run.createdAt].map(timestamp).find(Number.isFinite)
    if (at === undefined || at <= start || at > now || !isRecord(run.precheckResult)) continue
    const stdout = typeof run.precheckResult.stdout === 'string' ? run.precheckResult.stdout : ''
    const stderr = typeof run.precheckResult.stderr === 'string' ? run.precheckResult.stderr : ''
    const claimable = /\bclaimable=([^\s]*)/.exec(stdout)?.[1]
    const inFlightMatch = /\bin_flight=(\d+)\b/.exec(stdout)
    const maxMatch = /\bmax=(\d+)\b/.exec(stdout)
    const inFlight = inFlightMatch ? Number(inFlightMatch[1]) : undefined
    const max = maxMatch ? Number(maxMatch[1]) : undefined
    if (inFlight !== undefined && Number.isFinite(inFlight)) {
      inFlightSum += inFlight
      inFlightCount++
    }
    if (max !== undefined && Number.isFinite(max) && at >= latestMaxAt) {
      latestMaxAt = at
      latestMax = max
    }
    if (/rate limit/i.test(stderr)) reasons['限流']++
    else if (claimable === '') {
      if (inFlight !== undefined && max !== undefined && inFlight >= max && max > 0) reasons['达到上限']++
      else if (/\bskip [^\n]*?: scope overlaps\b/.test(stderr)) reasons['SCOPE 冲突']++
      else reasons['无可领票']++
    }
  }
  return {
    hourly, stages, reasons,
    inFlightAvg: inFlightCount ? inFlightSum / inFlightCount : 0,
    maxInFlight: input.maxInFlight ?? latestMax,
  }
}

export const renderThroughput = (Text: TextComponent, data?: ThroughputData) => {
  if (!data) return h(Text, { dimColor: true }, '数据未就绪')
  return [
    h(Text, null, '最近 24 小时（每小时完成）：' + data.hourly.join(' · ')),
    h(Text, null, '阶段耗时（中位数 / p90，秒）：'),
    ...(data.stages.length
      ? data.stages.map(stage => h(Text, { key: stage.key }, `${stage.label}：${stage.median} / ${stage.p90}`))
      : [h(Text, { dimColor: true }, '数据未就绪')]),
    h(Text, null, '无票原因：'),
    ...Object.entries(data.reasons).map(([reason, count]) => h(Text, { key: reason }, `${reason}：${count}`)),
    h(Text, null, `在途均值 ${data.inFlightAvg.toFixed(1)} / 上限 ${data.maxInFlight}`),
  ]
}
