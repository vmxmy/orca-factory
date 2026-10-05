import type { ComponentType, ReactElement } from 'react'
import type { FailureData } from '../../types'

// h 由 mod 运行时注入；本文件不用 JSX，因为 gate 在没有 node_modules 的检出里跑 CHECK。
declare function h(
  component: ComponentType<{ dimColor?: boolean; children?: string }>,
  props: { dimColor?: boolean; key?: number },
  ...children: string[]
): ReactElement

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
  gateByTierLane: (GateRate & { tier: string; lane: number })[]
}

const WEEK_MS = 7 * 24 * 60 * 60 * 1000
export const TOP_CORRECTIONS_LIMIT = 10
const rankedReasons = (counts: Map<string, number>): ReasonCount[] =>
  [...counts].map(([reason, count]) => ({ reason, count }))
    .sort((a, b) => b.count - a.count || a.reason.localeCompare(b.reason))

/** Aggregate only supplied snapshots. Missing historical fields never become failed gates. */
export function buildFailures(input: FailureInput | null = {}): FailureView {
  const snapshot = input ?? {}
  const handoffs = new Map<string, number>()
  const fails = new Map<string, number>()
  const gates = new Map<string, GateRate & { tier: string; lane: number }>()
  const corrections = new Map<string, number>()
  for (const row of snapshot.ledger ?? []) {
    if ((row.first_gate === 'green' || row.first_gate === 'red')
        && (row.tier === 'fast' || row.tier === 'default' || row.tier === 'strong')
        && (row.lane === 1 || row.lane === 2 || row.lane === 3)) {
      const key = `${row.tier}:${row.lane}`
      const group = gates.get(key) ?? { tier: row.tier, lane: row.lane, passed: 0, total: 0, rate: 0 }
      group.total++
      if (row.first_gate === 'green') group.passed++
      group.rate = group.passed / group.total
      gates.set(key, group)
    }
    if (row.ticket && typeof row.corrections === 'number' && Number.isInteger(row.corrections) && row.corrections >= 0) {
      corrections.set(row.ticket, (corrections.get(row.ticket) ?? 0) + row.corrections)
    }
  }
  for (const comments of Object.values(snapshot.comments ?? {})) {
    for (const comment of comments ?? []) {
      const body = typeof comment === 'string' ? comment : comment.body
      const at = typeof comment === 'string' ? NaN : Date.parse(comment.createdAt ?? '')
      for (const line of body.split(/\r?\n/)) {
        const fail = line.trim().match(/^FAIL:\s*approach=[^|]*\|\s*reason=([^|]+)(?:\||$)/)
        const reason = fail?.[1].trim()
        if (reason) fails.set(reason, (fails.get(reason) ?? 0) + 1)
        const handoff = line.trim().match(/^转人:\s*(.+)$/)?.[1].trim()
        if (handoff && typeof snapshot.now === 'number' && Number.isFinite(snapshot.now)
            && at >= snapshot.now - WEEK_MS && at <= snapshot.now) {
          handoffs.set(handoff, (handoffs.get(handoff) ?? 0) + 1)
        }
      }
    }
  }
  const handoffReasons = rankedReasons(handoffs)
  const gateByTierLane = [...gates.values()]
    .sort((a, b) => ['fast', 'default', 'strong'].indexOf(a.tier) - ['fast', 'default', 'strong'].indexOf(b.tier)
      || a.lane - b.lane)
  const repeatedFails = rankedReasons(fails).filter(row => row.count > 1)
  const topCorrections = [...corrections].map(([ticket, count]) => ({ ticket, corrections: count }))
    .sort((a, b) => b.corrections - a.corrections || a.ticket.localeCompare(b.ticket))
    .slice(0, TOP_CORRECTIONS_LIMIT)
  return {
    message: handoffReasons.length || gateByTierLane.length || repeatedFails.length || topCorrections.length
      ? '' : '数据未就绪',
    handoffReasons, gateByTierLane, repeatedFails, topCorrections,
  }
}

export function renderFailures(
  Text: ComponentType<{ dimColor?: boolean; children?: string }>,
  data?: FailureView,
) {
  if (!data || data.message) return h(Text, { dimColor: true }, '数据未就绪')
  const gateLine = (label: string, gate: GateRate) =>
    `${label}: ${gate.passed}/${gate.total} (${Math.round(gate.rate * 100)}%)`
  const lines = [
    '最近 7 天转人原因',
    ...data.handoffReasons.map(row => `${row.reason}: ${row.count}`),
    ...(data.handoffReasons.length ? [] : ['无']),
    'gate 首跑通过率',
    ...data.gateByTierLane.map(row => gateLine(`${row.tier} × lane ${row.lane}`, row)),
    ...(data.gateByTierLane.length ? [] : ['无']),
    '重复 FAIL 摘要',
    ...data.repeatedFails.map(row => `${row.reason}: ${row.count}`),
    ...(data.repeatedFails.length ? [] : ['无']),
    'correction 最多的票',
    ...data.topCorrections.map(row => `${row.ticket}: ${row.corrections}`),
    ...(data.topCorrections.length ? [] : ['无']),
  ]
  return lines.map((line, i) => h(Text, { key: i }, line))
}
