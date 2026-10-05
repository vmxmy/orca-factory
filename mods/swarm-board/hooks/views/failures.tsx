import type { ComponentType, ReactElement } from 'react'
import type { FailureData } from '../../types'

// The mod runtime injects h; avoiding JSX keeps snapshot checks package-free.
declare function h(
  component: ComponentType<{ dimColor?: boolean; children?: string }>,
  props: { dimColor?: boolean },
  ...children: string[]
): ReactElement

export type LedgerRow = {
  ticket?: string
  tier?: string
  lane?: number
  first_gate?: 'green' | 'red'
  corrections?: number
  finished_at?: string
}
export type FailuresInput = {
  ledger?: LedgerRow[]
  comments?: Record<string, string[]>
  now?: number
}
export type FailuresView = FailureData & {
  handoffReasons: { reason: string; count: number }[]
  firstPass: { key: string; runs: number; greenPct: number }[]
}

const WEEK_MS = 7 * 24 * 60 * 60 * 1000
const rankedReasons = (counts: Map<string, number>) =>
  [...counts].map(([reason, count]) => ({ reason, count }))
    .sort((a, b) => b.count - a.count || a.reason.localeCompare(b.reason))

export function buildFailures(input: FailuresInput = {}): FailuresView {
  const ledger = input.ledger ?? []
  const topCorrections = ledger
    .filter(row => (row.corrections ?? 0) > 0)
    .map(row => ({ ticket: row.ticket ?? '', corrections: row.corrections ?? 0 }))
    .sort((a, b) => b.corrections - a.corrections || a.ticket.localeCompare(b.ticket))
    .slice(0, 5)
  const finishes = new Map(ledger.map(row => [row.ticket, row.finished_at]))
  const fails = new Map<string, number>()
  const handoffs = new Map<string, number>()
  for (const [ticket, comments] of Object.entries(input.comments ?? {})) {
    const finishedAt = finishes.get(ticket)
    const at = finishedAt ? Date.parse(finishedAt) : undefined
    const recent = input.now === undefined || at === undefined
      || (at >= input.now - WEEK_MS && at <= input.now)
    for (const comment of comments) {
      for (const line of comment.split(/\r?\n/)) {
        const reason = line.match(/^FAIL: approach=.*? \| reason=(.*?) \| evidence=/)?.[1]
        if (reason !== undefined) fails.set(reason, (fails.get(reason) ?? 0) + 1)
        if (recent && line.startsWith('转人: ')) {
          const handoff = line.slice('转人: '.length)
          handoffs.set(handoff, (handoffs.get(handoff) ?? 0) + 1)
        }
      }
    }
  }

  const gates = new Map<string, { runs: number; green: number }>()
  for (const row of ledger) {
    if (row.first_gate === undefined) continue
    for (const key of [`tier=${row.tier ?? 'unknown'}`, `lane=${row.lane}`]) {
      const group = gates.get(key) ?? { runs: 0, green: 0 }
      gates.set(key, {
        runs: group.runs + 1,
        green: group.green + Number(row.first_gate === 'green'),
      })
    }
  }
  const firstPass = [...gates].map(([key, group]) => ({
    key, runs: group.runs, greenPct: Math.round(100 * group.green / group.runs),
  })).sort((a, b) => a.key.localeCompare(b.key))

  return {
    topCorrections,
    repeatedFails: rankedReasons(fails).filter(row => row.count >= 2),
    handoffReasons: rankedReasons(handoffs),
    firstPass,
  }
}

export const renderFailures = (Text: ComponentType<{ dimColor?: boolean; children?: string }>) =>
  h(Text, { dimColor: true }, '数据未就绪')
