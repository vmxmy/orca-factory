import type { ComponentType, ReactElement } from 'react'
import type { ThroughputData } from '../../types'

// Claude Code supplies the same h factory used by its JSX transform.
declare function h(
  component: ComponentType<{ dimColor?: boolean; children?: string }>,
  props: { dimColor?: boolean },
  children: string,
): ReactElement

type Stage = 'queue' | 'worker' | 'gate' | 'review' | 'merge'
export type LedgerRow = {
  outcome: string
  finished_at?: string
  stage_secs?: Partial<Record<Stage, number>> | null
}
type StageStats = { median: number; p90: number }
type Reason = '无可领票' | 'SCOPE 冲突' | '达到上限' | '限流'
export type ThroughputViewData = ThroughputData & {
  stages: Record<Stage, StageStats | null> | null
  reasons: Record<Reason, number>
  inFlight: { mean: number; max: number }
}
const HOUR = 3600000
const stageNames: Record<Stage, string> = {
  queue: '等领取', worker: 'worker', gate: 'gate', review: '审查', merge: '开 PR 到合并',
}
const record = (value: unknown): Record<string, unknown> | null =>
  value !== null && typeof value === 'object' && !Array.isArray(value)
    ? value as Record<string, unknown> : null

const stageStats = (ledger: LedgerRow[], stage: Stage): StageStats | null => {
  const samples = ledger.map(row => row.stage_secs?.[stage])
    .filter((value): value is number => typeof value === 'number' && Number.isFinite(value) && value >= 0)
    .sort((a, b) => a - b)
  if (!samples.length) return null
  const middle = Math.floor(samples.length / 2)
  return {
    median: samples.length % 2 ? samples[middle] : (samples[middle - 1] + samples[middle]) / 2,
    // Nearest-rank p90; the median averages the two central values for even sample counts.
    p90: samples[Math.ceil(samples.length * 0.9) - 1],
  }
}

export const buildThroughput = (input: {
  now: number; ledger: LedgerRow[]; runs: unknown[]; maxInFlight?: number
}): ThroughputViewData => {
  const end = Math.ceil(input.now / HOUR) * HOUR
  const start = end - 24 * HOUR
  const hourly = Array<number>(24).fill(0)
  for (const row of input.ledger) {
    if (row.outcome !== 'merged' || !row.finished_at) continue
    const finished = Date.parse(row.finished_at)
    if (Number.isFinite(finished) && finished >= start && finished <= input.now) {
      hourly[Math.min(23, Math.floor((finished - start) / HOUR))]++
    }
  }
  const stages: Record<Stage, StageStats | null> = {
    queue: stageStats(input.ledger, 'queue'), worker: stageStats(input.ledger, 'worker'),
    gate: stageStats(input.ledger, 'gate'), review: stageStats(input.ledger, 'review'),
    merge: stageStats(input.ledger, 'merge'),
  }
  const reasons: Record<Reason, number> = { '无可领票': 0, 'SCOPE 冲突': 0, '达到上限': 0, '限流': 0 }
  let total = 0
  let count = 0
  let latestMax = 0
  let latestAt = -Infinity
  for (const raw of input.runs) {
    const run = record(raw)
    if (!run) continue
    const timestamp = run.scheduledFor ?? run.dispatchedAt ?? run.startedAt ?? run.created_at
    const at = typeof timestamp === 'number' ? timestamp
      : typeof timestamp === 'string' ? Date.parse(timestamp) : NaN
    if (!Number.isFinite(at) || at < input.now - 24 * HOUR || at > input.now) continue
    const precheck = record(run.precheckResult)
    if (!precheck) continue
    const stdout = String(precheck.stdout ?? '')
    const stderr = String(precheck.stderr ?? '')
    const claim = /^claimable=([^\s]*)/m.exec(stdout)
    const flightMatch = /\bin_flight=(\d+)/.exec(stdout)
    const maxMatch = /\bmax=(\d+)/.exec(stdout)
    const inFlight = Number(flightMatch?.[1] ?? 0)
    const max = Number(maxMatch?.[1] ?? input.maxInFlight ?? 0)
    if (flightMatch) { total += inFlight; count++ }
    if (maxMatch && at > latestAt) { latestAt = at; latestMax = max }
    const empty = claim !== null && !claim[1].split(',').filter(Boolean).length
    if (empty && max > 0 && inFlight >= max) reasons['达到上限']++
    else if (/限流|(?:^|[^a-z])rate(?:[^a-z]|$)/i.test(stderr)) reasons['限流']++
    else if (empty && stderr.split('\n').some(line => /^skip [A-Z]+-\d+: .*SCOPE/i.test(line.trim()))) {
      reasons['SCOPE 冲突']++
    } else if (empty) reasons['无可领票']++
  }
  return {
    hourly, stages: Object.values(stages).some(Boolean) ? stages : null, reasons,
    inFlight: { mean: count ? total / count : 0, max: input.maxInFlight ?? latestMax },
  }
}

export const renderThroughput = (
  Text: ComponentType<{ dimColor?: boolean; children?: string }>, data?: ThroughputViewData,
) => {
  if (!data) return h(Text, { dimColor: true }, '数据未就绪')
  const stageLines = data.stages
    ? Object.entries(stageNames).map(([stage, label]) => {
      const stats = data.stages?.[stage as Stage]
      return label + '：' + (stats ? `中位数 ${stats.median} 秒 · p90 ${stats.p90} 秒` : '数据未就绪')
    }) : ['阶段耗时：数据未就绪']
  const lines = [
    '每小时完成（最近 24 小时，从早到晚）：' + data.hourly.join(' '),
    ...stageLines,
    '无票原因（最近 24 小时）',
    ...Object.entries(data.reasons).map(([reason, count]) => reason + '：' + count),
    '在途均值：' + Number(data.inFlight.mean.toFixed(2)) + ' / ' + data.inFlight.max,
  ]
  return h(Text, {}, lines.join('\n'))
}
