import type { LedgerRow as ThroughputRow } from './views/throughput'
import type { LedgerRow as FailureRow } from './views/failures'

export function ledgerRows(comments: readonly { body: string }[]): (ThroughputRow & FailureRow)[] {
  const rows: (ThroughputRow & FailureRow)[] = []
  const seen = new Set<string>()
  for (const comment of comments) for (const line of comment.body.split(/\r?\n/)) {
    if (!line.startsWith('ledger ')) continue
    let raw: Record<string, unknown>
    try {
      const parsed: unknown = JSON.parse(line.slice(7))
      if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) continue
      raw = parsed as Record<string, unknown>
    } catch { continue }
    if (typeof raw.ticket !== 'string') continue
    const key = JSON.stringify([raw.ticket, raw.started_at ?? null])
    if (seen.has(key)) continue
    seen.add(key)
    const stages = raw.stage_secs && typeof raw.stage_secs === 'object'
      ? Object.fromEntries(Object.entries(raw.stage_secs).filter((entry): entry is [string, number] =>
        typeof entry[1] === 'number' && Number.isFinite(entry[1]) && entry[1] >= 0)) : undefined
    rows.push({
      ticket: raw.ticket,
      outcome: typeof raw.outcome === 'string' ? raw.outcome : undefined,
      finished_at: typeof raw.finished_at === 'string' ? raw.finished_at : undefined,
      stage_secs: stages,
      tier: typeof raw.tier === 'string' ? raw.tier : undefined,
      lane: typeof raw.lane === 'number' ? raw.lane : undefined,
      first_gate: typeof raw.first_gate === 'string' ? raw.first_gate : undefined,
      corrections: typeof raw.corrections === 'number' && Number.isFinite(raw.corrections) ? raw.corrections : undefined,
    })
  }
  return rows
}
