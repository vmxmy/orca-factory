import type { ComponentType } from 'react'
import type { FailureData } from '../../types'

export type FailureLedgerRow = {
  ticket?: string
  tier?: string
  lane?: number
  first_gate?: string
  corrections?: number
}
export type FailureComment = string | { body: string; createdAt?: string }
export type FailureInput = {
  ledger?: readonly FailureLedgerRow[] | null
  comments?: Readonly<Record<string, readonly FailureComment[] | null | undefined>> | null
  /** Epoch milliseconds supplied by the caller; no wall-clock reads in the builder. */
  now?: number
}
type ReasonCount = { reason: string; count: number }
type GateRate = { passed: number; total: number; rate: number }
export type FailureView = FailureData & {
  message: string
  handoffReasons: ReasonCount[]
  gateByTier: (GateRate & { tier: string })[]
  gateByLane: (GateRate & { lane: number })[]
}

const WEEK_MS = 7 * 24 * 60 * 60 * 1000
const rankedReasons = (counts: Map<string, number>): ReasonCount[] =>
  [...counts].map(([reason, count]) => ({ reason, count }))
    .sort((a, b) => b.count - a.count || a.reason.localeCompare(b.reason))

/** Aggregate only supplied snapshots. Missing historical fields never become failed gates. */
export function buildFailures(input: FailureInput = {}): FailureView {
  const handoffs = new Map<string, number>()
  const fails = new Map<string, number>()
  const tiers = new Map<string, GateRate>()
  const lanes = new Map<number, GateRate>()
  const corrections = new Map<string, number>()
  const countGate = <K,>(groups: Map<K, GateRate>, key: K, green: boolean) => {
    const group = groups.get(key) ?? { passed: 0, total: 0, rate: 0 }
    group.total++
    if (green) group.passed++
    group.rate = group.passed / group.total
    groups.set(key, group)
  }
  for (const row of input.ledger ?? []) {
    if (row.first_gate === 'green' || row.first_gate === 'red') {
      const green = row.first_gate === 'green'
      if (row.tier === 'fast' || row.tier === 'default' || row.tier === 'strong') countGate(tiers, row.tier, green)
      if (row.lane === 1 || row.lane === 2 || row.lane === 3) countGate(lanes, row.lane, green)
    }
    if (row.ticket && typeof row.corrections === 'number' && Number.isInteger(row.corrections) && row.corrections >= 0) {
      corrections.set(row.ticket, (corrections.get(row.ticket) ?? 0) + row.corrections)
    }
  }
  for (const comments of Object.values(input.comments ?? {})) {
    for (const comment of comments ?? []) {
      const body = typeof comment === 'string' ? comment : comment.body
      const at = typeof comment === 'string' ? NaN : Date.parse(comment.createdAt ?? '')
      for (const line of body.split(/\r?\n/)) {
        const fail = line.trim().match(/^FAIL:\s*approach=[^|]*\|\s*reason=([^|]+)(?:\||$)/)
        const reason = fail?.[1].trim()
        if (reason) fails.set(reason, (fails.get(reason) ?? 0) + 1)
        const handoff = line.trim().match(/^转人:\s*(.+)$/)?.[1].trim()
        if (handoff && typeof input.now === 'number' && Number.isFinite(input.now)
            && at >= input.now - WEEK_MS && at <= input.now) {
          handoffs.set(handoff, (handoffs.get(handoff) ?? 0) + 1)
        }
      }
    }
  }
  const handoffReasons = rankedReasons(handoffs)
  const gateByTier = [...tiers].map(([tier, rate]) => ({ tier, ...rate }))
    .sort((a, b) => ['fast', 'default', 'strong'].indexOf(a.tier) - ['fast', 'default', 'strong'].indexOf(b.tier))
  const gateByLane = [...lanes].map(([lane, rate]) => ({ lane, ...rate })).sort((a, b) => a.lane - b.lane)
  const repeatedFails = rankedReasons(fails).filter(row => row.count > 1)
  const topCorrections = [...corrections].map(([ticket, count]) => ({ ticket, corrections: count }))
    .sort((a, b) => b.corrections - a.corrections || a.ticket.localeCompare(b.ticket))
  return {
    message: handoffReasons.length || gateByTier.length || gateByLane.length || repeatedFails.length || topCorrections.length
      ? '' : '数据未就绪',
    handoffReasons, gateByTier, gateByLane, repeatedFails, topCorrections,
  }
}

export function renderFailures(
  Text: ComponentType<{ dimColor?: boolean; children?: string }>,
  data?: FailureView,
) {
  if (!data || data.message) return <Text dimColor>数据未就绪</Text>
  const gateLine = (label: string, gate: GateRate) =>
    `${label}: ${gate.passed}/${gate.total} (${Math.round(gate.rate * 100)}%)`
  const lines = [
    '最近 7 天转人原因',
    ...data.handoffReasons.map(row => `${row.reason}: ${row.count}`),
    ...(data.handoffReasons.length ? [] : ['无']),
    'gate 首跑通过率',
    ...data.gateByTier.map(row => gateLine(row.tier, row)),
    ...data.gateByLane.map(row => gateLine(`lane ${row.lane}`, row)),
    ...(data.gateByTier.length || data.gateByLane.length ? [] : ['无']),
    '重复 FAIL 摘要',
    ...data.repeatedFails.map(row => `${row.reason}: ${row.count}`),
    ...(data.repeatedFails.length ? [] : ['无']),
    'correction 最多的票',
    ...data.topCorrections.map(row => `${row.ticket}: ${row.corrections}`),
    ...(data.topCorrections.length ? [] : ['无']),
  ]
  return <>{lines.map((line, index) => <Text key={index}>{line}</Text>)}</>
}
