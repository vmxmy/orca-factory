import type { ComponentType, ReactElement } from 'react'
import type { ThroughputData } from '../../types'

type TextComponent = ComponentType<{ dimColor?: boolean; children?: string }>
declare function h(Text: TextComponent, props: { dimColor?: boolean }, child: string): ReactElement

type LedgerRow = { outcome?: string; finished_at?: string; stage_secs?: Record<string, number> }
const stageLabels = {
  wait_claim: '等领取', worker: 'worker', gate: 'gate', review: '审查', pr_to_merge: '开 PR 到合并',
}
type Stage = keyof typeof stageLabels
type StageStats = { median: number; p90: number }
type Throughput = ThroughputData & {
  stages: Record<Stage, StageStats | null> | null
  reasons: Record<string, number>
  inFlight: { avg: number | null; max: number | null }
}
const hour = 3_600_000
const isRecord = (value: unknown): value is Record<string, unknown> =>
  value !== null && typeof value === 'object' && !Array.isArray(value)
const timestamp = (value: unknown): number =>
  typeof value === 'number' ? value : typeof value === 'string' ? Date.parse(value) : NaN

const parsePrecheck = (run: Record<string, unknown>) => {
  const result = run.precheckResult
  if (!isRecord(result)) return null
  const stdout = String(result.stdout ?? '')
  const stderr = String(result.stderr ?? '')
  const exitCode = Number(result.exitCode ?? 0)
  const timedOut = result.timedOut === true
  const error = String(result.error ?? '')
  const claimable = /^claimable=([^\s]*)/m.exec(stdout)?.[1]
  const inFlight = /\bin_flight=(\d+)\b/.exec(stdout)
  const max = /\bmax=(\d+)\b/.exec(stdout)
  return {
    claimable, stderr,
    inFlight: inFlight ? Number(inFlight[1]) : null,
    max: max ? Number(max[1]) : null,
    uncertain: timedOut || error !== '' || !Number.isFinite(exitCode) || exitCode === 3 ||
      /^不确定:/m.test(stderr) || (exitCode !== 0 && claimable === undefined),
  }
}

export const buildThroughput = (input: {
  now: number; ledger: LedgerRow[]; runs: unknown[]; maxInFlight?: number
}): Throughput => {
  const { now, ledger, runs } = input
  const end = Math.ceil(now / hour) * hour
  const start = end - 24 * hour
  const hourly = Array<number>(24).fill(0)
  const samples: Record<Stage, number[]> = { wait_claim: [], worker: [], gate: [], review: [], pr_to_merge: [] }
  for (const row of ledger) {
    const finished = timestamp(row.finished_at)
    if (row.outcome === 'merged' && Number.isFinite(finished) && finished >= start && finished < end && finished <= now) {
      hourly[Math.floor((finished - start) / hour)]++
    }
    if (!row.stage_secs) continue
    for (const key of Object.keys(stageLabels) as Stage[]) {
      const seconds = row.stage_secs[key]
      if (Number.isFinite(seconds) && seconds >= 0) samples[key].push(seconds)
    }
  }
  const stages: Record<Stage, StageStats | null> = { wait_claim: null, worker: null, gate: null, review: null, pr_to_merge: null }
  let hasStages = false
  for (const key of Object.keys(stageLabels) as Stage[]) {
    const values = samples[key].sort((a, b) => a - b)
    if (!values.length) continue
    hasStages = true
    const middle = Math.floor(values.length / 2)
    stages[key] = {
      median: values.length % 2 ? values[middle] : (values[middle - 1] + values[middle]) / 2,
      // Nearest-rank p90; the median averages the middle pair for even samples.
      p90: values[Math.ceil(values.length * 0.9) - 1],
    }
  }

  const reasons: Record<string, number> = { '无可领票': 0, 'SCOPE 冲突': 0, '达到上限': 0, '限流': 0 }
  let sum = 0
  let count = 0
  let latestMaxAt = -Infinity
  let max: number | null = null
  for (const run of runs) {
    if (!isRecord(run)) continue
    const at = timestamp(run.scheduledFor ?? run.dispatchedAt ?? run.startedAt ?? run.created_at)
    if (!Number.isFinite(at) || at < now - 24 * hour || at > now) continue
    const precheck = parsePrecheck(run)
    if (!precheck) continue
    if (precheck.inFlight !== null && Number.isFinite(precheck.inFlight)) {
      sum += precheck.inFlight
      count++
    }
    if (precheck.max !== null && Number.isFinite(precheck.max) && precheck.max > 0 && at >= latestMaxAt) {
      latestMaxAt = at
      max = precheck.max
    }
    // Count one reason per empty scan; uncertain reads are not an empty queue.
    if (precheck.claimable) continue
    if (/rate limit/i.test(precheck.stderr) || /限流/.test(precheck.stderr)) {
      reasons['限流']++
    } else if (!precheck.uncertain && precheck.claimable === '') {
      if (precheck.inFlight !== null && precheck.max !== null && precheck.max > 0 && precheck.inFlight >= precheck.max) {
        reasons['达到上限']++
      } else if (/^skip [A-Z]+-\d+: scope overlaps\b/m.test(precheck.stderr)) {
        reasons['SCOPE 冲突']++
      } else {
        reasons['无可领票']++
      }
    }
  }
  if (input.maxInFlight !== undefined && Number.isFinite(input.maxInFlight) && input.maxInFlight > 0) {
    max = input.maxInFlight
  }
  return {
    hourly, stages: hasStages ? stages : null, reasons,
    inFlight: { avg: count ? sum / count : null, max: count ? max : null },
  }
}

export const renderThroughput = (Text: TextComponent, data?: Throughput) => {
  if (!data) return h(Text, { dimColor: true }, '数据未就绪')
  const lines = ['最近 24 小时（每小时完成）：' + data.hourly.join(' · '), '阶段耗时（中位数 / p90，秒）：']
  if (!data.stages) lines.push('数据未就绪')
  else for (const key of Object.keys(stageLabels) as Stage[]) {
    const stats = data.stages[key]
    lines.push(stageLabels[key] + '：' + (stats ? `${stats.median} / ${stats.p90}` : '数据未就绪'))
  }
  lines.push('无票原因：')
  for (const [reason, count] of Object.entries(data.reasons)) lines.push(`${reason}：${count}`)
  const { avg, max } = data.inFlight
  lines.push('在途均值 / 上限：' + (avg === null ? '数据未就绪' : `${avg.toFixed(1)} / ${max ?? '数据未就绪'}`))
  return h(Text, {}, lines.join('\n'))
}
