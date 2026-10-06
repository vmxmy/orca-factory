import type { TicketUsage as SharedTicketUsage } from '../types'
import { usagePrices } from './usage-prices'
/** Disjoint token buckets: input excludes cache reads (including for Codex). */
export type UsageRecord = {
  cwd: string; model: string; input: number; output: number; cacheRead: number; cacheWrite: number; cacheWriteHour?: number; reasoning?: number
}
export type TicketUsage = SharedTicketUsage & Omit<UsageRecord, 'cwd' | 'model'> & {
  total: number; usd: number; known: boolean; partial: boolean; allocated: boolean; model: string
}
export type WorktreeToTicket = Record<string, string | readonly string[]>
const prices: Record<string, { input: number; output: number; read: number; write?: number; write_hour?: number }> = usagePrices.models
const count = (v: unknown): number => typeof v === 'number' && Number.isFinite(v) && v >= 0 ? v : 0
const object = (v: unknown): Record<string, unknown> => v !== null && typeof v === 'object' ? v as Record<string, unknown> : {}
const emptyTotals = () => ({ input: 0, output: 0, cache_read: 0, cache_write: 0, reasoning: 0, cost_usd: 0 })
const pathKey = (p: string) => p.replace(/\\/g, '/').replace(/\/$/, '')

/** Longest worktree wins; a shared coordinator path is divided among distinct tickets. */
export function aggregateUsage(records: readonly UsageRecord[], worktreeToTicket: WorktreeToTicket): Record<string, TicketUsage> {
  const result: Record<string, TicketUsage> = {}
  const paths = Object.entries(worktreeToTicket).map(([p, ids]) => [pathKey(p), [...new Set(typeof ids === 'string' ? [ids] : ids)]] as const)
    .sort((a, b) => b[0].length - a[0].length)
  for (const r of records) {
    const cwd = pathKey(r.cwd)
    const ids = paths.find(([p]) => cwd === p || cwd.startsWith(p + '/'))?.[1] ?? []
    for (const id of ids) {
      const u = result[id] ??= { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0, usd: 0, known: false, partial: false, allocated: false, model: r.model, ticket: id, cache_read: 0, cache_write: 0, reasoning: 0, cost_usd: 0, price_version: usagePrices.price_version, apportioned: false, by_role: { worker: emptyTotals(), reviewer: emptyTotals(), coordinator: emptyTotals(), session: emptyTotals() } }
      const buckets = [r.input, r.output, r.cacheRead, r.cacheWrite].map(v => count(v) / ids.length)
      u.input += buckets[0]; u.output += buckets[1]; u.cacheRead += buckets[2]; u.cacheWrite += buckets[3]
      u.total += buckets.reduce((a, b) => a + b, 0)
      u.allocated ||= ids.length > 1
      const model = r.model.replace(/^.*\//, '').replace(/-\d{8}$/, '')
      const price = prices[model]
      if (price) {
        u.known = true
        const hour = Math.min(count(r.cacheWriteHour) / ids.length, buckets[3])
        u.usd += (buckets[0] * price.input + buckets[1] * price.output + buckets[2] * price.read + (buckets[3] - hour) * (price.write ?? 0) + hour * (price.write_hour ?? 0)) / 1e6
      }
      else u.partial = true
      u.cache_read = u.cacheRead; u.cache_write = u.cacheWrite; u.cost_usd = u.usd; u.apportioned = u.allocated
      u.reasoning += count(r.reasoning) / ids.length
      u.by_role.session = { input: u.input, output: u.output, cache_read: u.cache_read, cache_write: u.cache_write, reasoning: u.reasoning, cost_usd: u.cost_usd }
    }
  }
  return result
}

/** Parse complete JSONL rows only: a running session may end with an incomplete row. */
export function parseSession(text: string, kind: 'claude' | 'codex', fallbackCwd = ''): UsageRecord[] {
  const messages = new Map<string, UsageRecord>()
  let cwd = fallbackCwd, model = ''
  let previous = { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, reasoning: 0 }
  const modelRows = new Map<string, UsageRecord>()
  for (const [index, line] of text.split('\n').entries()) {
    if (!line.trim()) continue
    let row: Record<string, unknown>
    try { row = object(JSON.parse(line)) } catch { continue /* partial or damaged JSONL row */ }
    if (typeof row.cwd === 'string') cwd = row.cwd
    const payload = object(row.payload)
    if (kind === 'codex') {
      if (row.type === 'session_meta' && typeof payload.cwd === 'string') cwd = payload.cwd
      if (row.type === 'turn_context' && typeof payload.model === 'string') model = payload.model
      if (payload.type !== 'token_count') continue
      const info = object(payload.info), usage = object(info.total_token_usage)
      if (!Object.keys(usage).length) continue
      const cached = count(usage.cached_input_tokens)
      const written = count(usage.cache_write_input_tokens)
      const current = { input: Math.max(0, count(usage.input_tokens) - cached - written), output: count(usage.output_tokens), cacheRead: cached, cacheWrite: written, reasoning: count(usage.reasoning_output_tokens) }
      const contribution = modelRows.get(model) ?? { cwd, model, input: 0, output: 0, cacheRead: 0, cacheWrite: 0, reasoning: 0 }
      let changed = false
      for (const key of ['input', 'output', 'cacheRead', 'cacheWrite', 'reasoning'] as const) {
        const delta = current[key] - previous[key]
        if (delta < 0) throw new Error('Codex 累计用量倒退，无法可靠估价')
        changed ||= delta > 0
        contribution[key] = (contribution[key] ?? 0) + delta
      }
      if (current.reasoning - previous.reasoning > current.output - previous.output) throw new Error('Codex 推理用量超出输出，无法可靠估价')
      if (changed) modelRows.set(model, contribution)
      previous = current
    } else {
      const message = object(row.message), usage = object(message.usage)
      if (!Object.keys(usage).length) continue
      // Repeated streaming snapshots of the same assistant message must not multiply usage.
      const key = typeof message.id === 'string' ? message.id : String(row.uuid ?? index)
      messages.set(key, { cwd, model: String(message.model ?? ''), input: count(usage.input_tokens), output: count(usage.output_tokens), cacheRead: count(usage.cache_read_input_tokens), cacheWrite: count(usage.cache_creation_input_tokens), cacheWriteHour: count(object(usage.cache_creation).ephemeral_1h_input_tokens) })
    }
  }
  return kind === 'codex' ? [...modelRows.values()] : [...messages.values()]
}

export type UsageIO = {
  files: () => Promise<string[]>
  mtime: (path: string) => Promise<number | undefined>
  read: (path: string) => Promise<string>
}
/** Keep this reader alive across refreshes. No unchanged transcript is reread. */
export function createUsageReader() {
  const cache = new Map<string, { mtime: number; rows: UsageRecord[] }>()
  return async (io: UsageIO, worktreePaths: readonly string[]): Promise<UsageRecord[]> => {
    const files = await io.files(), rows: UsageRecord[] = []
    for (const path of files) {
      const kind = path.includes('/.claude/projects/') ? 'claude' : 'codex'
      const escaped = kind === 'claude' ? path.split('/.claude/projects/')[1]?.split('/')[0] : ''
      const fallback = worktreePaths.find(p => p.replace(/[^a-zA-Z0-9]/g, '-') === escaped) ?? ''
      const mtime = await io.mtime(path)
      let entry = cache.get(path)
      if (!entry || mtime === undefined || mtime !== entry.mtime) {
        entry = { mtime: mtime ?? NaN, rows: parseSession(await io.read(path), kind, fallback) }
        cache.set(path, entry)
      }
      rows.push(...entry.rows)
    }
    const present = new Set(files)
    for (const path of cache.keys()) if (!present.has(path)) cache.delete(path)
    return rows
  }
}

export type LedgerUsage = { minutes?: number; usage?: TicketUsage }
/** Latest run per ticket; duplicate ledger comments do not double-count a completed run. */
export function ledgerUsage(comments: readonly { body: string }[]): Record<string, LedgerUsage> {
  const result: Record<string, LedgerUsage> = {}, latest: Record<string, string> = {}
  for (const comment of comments) for (const body of comment.body.split(/\r?\n/)) {
    if (!body.startsWith('ledger ')) continue
    let row: Record<string, unknown>
    try { row = object(JSON.parse(body.slice(7))) } catch { continue /* invalid ledger comment */ }
    if (typeof row.ticket !== 'string') continue
    const at = String(row.finished_at ?? row.started_at ?? '')
    if (latest[row.ticket] !== undefined && latest[row.ticket] > at) continue
    latest[row.ticket] = at
    const value: LedgerUsage = {}
    if (typeof row.minutes === 'number' && row.minutes >= 0 && Number.isFinite(row.minutes)) value.minutes = row.minutes
    const u = object(row.usage)
    if (Object.keys(u).length) {
      const r = { cwd: '/', model: String(u.model ?? ''), input: count(u.input), output: count(u.output), cacheRead: count(u.cache_read), cacheWrite: count(u.cache_write), reasoning: count(u.reasoning) }
      value.usage = aggregateUsage([r], { '/': row.ticket })[row.ticket]
      if (typeof u.cost_usd === 'number' && Number.isFinite(u.cost_usd) && u.cost_usd >= 0 && typeof u.price_version === 'string' && u.price_version) {
        Object.assign(value.usage, { usd: u.cost_usd, cost_usd: u.cost_usd, known: true, partial: false, price_version: u.price_version })
      }
      const models = Object.keys(object(u.by_model))
      if (models.length) {
        const priced = models.map(model => !!prices[model.replace(/^.*\//, '').replace(/-\d{8}$/, '')])
        value.usage.known = priced.some(Boolean)
        value.usage.partial = priced.some(known => !known)
      }
      value.usage.allocated = value.usage.apportioned = u.apportioned === true
    }
    result[row.ticket] = value
  }
  return result
}
