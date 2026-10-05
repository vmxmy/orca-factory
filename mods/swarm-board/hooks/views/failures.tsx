import type { ComponentType } from 'react'
import type { FailureData } from '../../types'

export type LedgerRow = {
  ticket?: string
  tier?: string
  lane?: number
  first_gate?: string
  corrections?: number
}
export type FailureComment = string | { body: string; createdAt: string }
export type FailureInput = {
  ledger: LedgerRow[]
  comments: Record<string, FailureComment[]>
}
type ReasonCount = { reason: string; count: number }
type FirstPassRate = { key: string; runs: number; greenPct: number }
export type FailureView = FailureData & {
  handoffReasons: ReasonCount[]
  firstPass: { tier: FirstPassRate[]; lane: FirstPassRate[] }
}

const WEEK_MS = 7 * 24 * 60 * 60 * 1000
const rankedReasons = (counts: Map<string, number>): ReasonCount[] =>
  [...counts].map(([reason, count]) => ({ reason, count }))
    .sort((a, b) => b.count - a.count || a.reason.localeCompare(b.reason))

export function buildFailures(input: FailureInput, now = Date.now()): FailureView {
  if (!input || typeof input !== 'object'
      || !Array.isArray(input.ledger) || !input.ledger.length
      || !input.ledger.every(row => row && typeof row === 'object' && !Array.isArray(row))
      || !input.comments || typeof input.comments !== 'object' || Array.isArray(input.comments)
      || !Object.keys(input.comments).length
      || !Object.values(input.comments).every(comments => Array.isArray(comments)
        && comments.every(comment => typeof comment === 'string'
          || (comment && typeof comment === 'object'
            && typeof comment.body === 'string' && typeof comment.createdAt === 'string')))
      || !Object.values(input.comments).some(comments => comments.length > 0)) {
    return { topCorrections: [], repeatedFails: [], handoffReasons: [], firstPass: { tier: [], lane: [] } }
  }

  const topCorrections = input.ledger
    .filter(row => typeof row.ticket === 'string' && row.ticket.length > 0
      && typeof row.corrections === 'number' && row.corrections > 0)
    .map(row => ({ ticket: row.ticket ?? '', corrections: row.corrections ?? 0 }))
    .sort((a, b) => b.corrections - a.corrections || a.ticket.localeCompare(b.ticket))
    .slice(0, 5)
  const fails = new Map<string, number>()
  const handoffs = new Map<string, number>()
  for (const comments of Object.values(input.comments)) {
    for (const comment of comments) {
      const body = typeof comment === 'string' ? comment : comment.body
      const at = typeof comment === 'string' ? undefined : Date.parse(comment.createdAt)
      const recent = at === undefined || (at >= now - WEEK_MS && at <= now)
      for (const line of body.split(/\r?\n/)) {
        const reason = line.match(/^FAIL:.*\|\s*reason=([^|]+?)\s*(?:\||$)/)?.[1].trim()
        if (reason) fails.set(reason, (fails.get(reason) ?? 0) + 1)
        if (recent && line.startsWith('转人:')) {
          const handoff = line.slice('转人:'.length).trim()
          if (handoff) handoffs.set(handoff, (handoffs.get(handoff) ?? 0) + 1)
        }
      }
    }
  }

  const gates = input.ledger.filter(row => row.first_gate === 'green' || row.first_gate === 'red')
  const rates = (keyOf: (row: LedgerRow) => string): FirstPassRate[] => {
    const groups = gates.reduce((counts, row) => {
      const key = keyOf(row)
      const group = counts.get(key) ?? { runs: 0, green: 0 }
      counts.set(key, { runs: group.runs + 1, green: group.green + Number(row.first_gate === 'green') })
      return counts
    }, new Map<string, { runs: number; green: number }>())
    return [...groups].map(([key, group]) => ({
      key, runs: group.runs, greenPct: Math.round(100 * group.green / group.runs),
    })).sort((a, b) => a.key.localeCompare(b.key))
  }
  return {
    topCorrections,
    repeatedFails: rankedReasons(fails).filter(row => row.count >= 2),
    handoffReasons: rankedReasons(handoffs),
    firstPass: {
      tier: rates(row => row.tier ?? 'unknown'),
      lane: rates(row => String(row.lane ?? 'unknown')),
    },
  }
}

export const renderFailures = (Text: ComponentType<{ dimColor?: boolean; children?: string }>) => (
  <Text dimColor>数据未就绪</Text>
)
