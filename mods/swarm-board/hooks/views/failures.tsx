declare function h(type: unknown, props?: unknown, ...children: unknown[]): unknown

export type LedgerRow = {
  ticket: string
  tier?: string
  lane?: number
  first_gate?: string
  corrections?: number
  finished_at?: string
}

export type CommentInput = string | { body: string; createdAt?: string }

export type FailureInput = {
  ledger?: LedgerRow[]
  comments?: Record<
    string, CommentInput[]
  >
  now?: number
}

export type FailureView = {
  topCorrections: { ticket: string; corrections: number }[]
  repeatedFails: { reason: string; count: number }[]
  handoffReasons: { reason: string; count: number }[]
  firstGate: { key: string; green: number; total: number }[]
}

const HANDOFF_WINDOW_MS = 7 * 86400000

const rankedReasons = (counts: Map<
  string, number
>) =>
  [...counts].map(([reason, count]) => ({ reason, count }))
    .sort((a, b) => b.count - a.count || a.reason.localeCompare(b.reason))

export const buildFailures = (input: FailureInput = {}): FailureView => {
  const ledger = input.ledger ?? []
  const now = input.now ?? Date.now()
  const topCorrections = ledger
    .filter(row => (row.corrections ?? 0) > 0)
    .map(row => ({ ticket: row.ticket, corrections: row.corrections ?? 0 }))
    .sort((a, b) => b.corrections - a.corrections)
    .slice(0, 5)

  const gates = new Map<
    string, { green: number; total: number }
  >()
  for (const row of ledger) {
    if (row.first_gate !== 'green' && row.first_gate !== 'red') continue
    const key = `${row.tier ?? 'unknown'}/lane${row.lane ?? 'unknown'}`
    const group = gates.get(key) ?? { green: 0, total: 0 }
    group.green += Number(row.first_gate === 'green')
    group.total += 1
    gates.set(key, group)
  }

  const fails = new Map<
    string, number
  >()
  const handoffs = new Map<
    string, number
  >()
  for (const comments of Object.values(input.comments ?? {})) {
    for (const comment of comments) {
      const body = typeof comment === 'string' ? comment : comment.body
      const reason = body.match(/^FAIL:.*?\|\s*reason=([^|]+?)\s*\|/m)?.[1].trim()
      if (reason) fails.set(reason, (fails.get(reason) ?? 0) + 1)

      const createdAt = typeof comment === 'string' || !comment.createdAt
        ? NaN : Date.parse(comment.createdAt)
      if (Number.isFinite(createdAt)
        && (createdAt < now - HANDOFF_WINDOW_MS || createdAt > now)) continue
      for (const line of body.split(/\r?\n/)) {
        if (!line.startsWith('转人:')) continue
        const handoffReason = line.slice('转人:'.length).trim()
        if (handoffReason) handoffs.set(handoffReason, (handoffs.get(handoffReason) ?? 0) + 1)
      }
    }
  }

  return {
    topCorrections,
    repeatedFails: rankedReasons(fails).filter(row => row.count >= 2),
    handoffReasons: rankedReasons(handoffs),
    firstGate: [...gates].map(([key, group]) => ({ key, ...group }))
      .sort((a, b) => a.key.localeCompare(b.key)),
  }
}

export const renderFailures = (
  Text: (props: { dimColor?: boolean; children?: string }) => unknown,
  data?: FailureView,
) => {
  if (!data || [data.handoffReasons, data.firstGate, data.repeatedFails, data.topCorrections]
    .every(rows => rows.length === 0)) {
    return h(Text, { dimColor: true }, '数据未就绪')
  }
  const lines = [
    { key: 'handoff-title', dimColor: false, text: '转人原因' },
    ...data.handoffReasons.map((row, index) => ({
      key: `handoff-${index}`, dimColor: true, text: `${row.reason}: ${row.count}`,
    })),
    { key: 'gate-title', dimColor: false, text: '首跑通过率' },
    ...data.firstGate.map(row => ({
      key: `gate-${row.key}`, dimColor: true,
      text: `${row.key}: ${row.green}/${row.total} (${Math.round(row.green / row.total * 100)}%)`,
    })),
    { key: 'fail-title', dimColor: false, text: '重复 FAIL 摘要' },
    ...data.repeatedFails.map((row, index) => ({
      key: `fail-${index}`, dimColor: true, text: `${row.reason}: ${row.count}`,
    })),
    { key: 'correction-title', dimColor: false, text: 'correction 最多的票' },
    ...data.topCorrections.map((row, index) => ({
      key: `correction-${index}`, dimColor: true, text: `${row.ticket}: ${row.corrections}`,
    })),
  ]
  return lines.map(({ key, dimColor, text }) => h(Text, { key, dimColor }, text))
}
