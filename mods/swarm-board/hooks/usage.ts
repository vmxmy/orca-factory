/** Disjoint token buckets: input excludes cache reads (including for Codex). */
export type UsageRecord = {
  cwd: string; model: string; input: number; output: number; cacheRead: number; cacheWrite: number
}
export type TicketUsage = Omit<UsageRecord, 'cwd' | 'model'> & {
  total: number; usd: number; known: boolean; partial: boolean; allocated: boolean; model: string
}
export type WorktreeToTicket = Record<string, string | readonly string[]>
// Board estimates, USD per million tokens: input, output, cache read, cache write.
// Unknown models remain explicitly unpriced; do not guess a price for all GPT models.
const prices: Record<string, readonly number[]> = {
  'claude-sonnet-5-5': [3, 15, 0.3, 3.75],
  'claude-opus-5-5': [15, 75, 1.5, 18.75],
  'claude-haiku-4-5': [0.8, 4, 0.08, 1],
  'gpt-6-astra': [1.25, 10, 0.125, 0],
}
const count = (v: unknown): number => typeof v === 'number' && Number.isFinite(v) && v >= 0 ? v : 0
const object = (v: unknown): Record<string, unknown> => v !== null && typeof v === 'object' ? v as Record<string, unknown> : {}
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
      const u = result[id] ??= { input: 0, output: 0, cacheRead: 0, cacheWrite: 0, total: 0, usd: 0, known: false, partial: false, allocated: false, model: r.model }
      const buckets = [r.input, r.output, r.cacheRead, r.cacheWrite].map(v => count(v) / ids.length)
      u.input += buckets[0]; u.output += buckets[1]; u.cacheRead += buckets[2]; u.cacheWrite += buckets[3]
      u.total += buckets.reduce((a, b) => a + b, 0)
      u.allocated ||= ids.length > 1
      const price = prices[r.model.replace(/^.*\//, '')]
      if (price) { u.known = true; u.usd += buckets.reduce((sum, n, i) => sum + n * price[i], 0) / 1e6 }
      else u.partial = true
    }
  }
  return result
}

/** Parse complete JSONL rows only: a running session may end with an incomplete row. */
export function parseSession(text: string, kind: 'claude' | 'codex', fallbackCwd = ''): UsageRecord[] {
  const messages = new Map<string, UsageRecord>()
  let cwd = fallbackCwd, model = '', last: UsageRecord | undefined
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
      last = { cwd, model, input: Math.max(0, count(usage.input_tokens) - cached), output: count(usage.output_tokens), cacheRead: cached, cacheWrite: 0 }
    } else {
      const message = object(row.message), usage = object(message.usage)
      if (!Object.keys(usage).length) continue
      // Repeated streaming snapshots of the same assistant message must not multiply usage.
      const key = typeof message.id === 'string' ? message.id : String(row.uuid ?? index)
      messages.set(key, { cwd, model: String(message.model ?? ''), input: count(usage.input_tokens), output: count(usage.output_tokens), cacheRead: count(usage.cache_read_input_tokens), cacheWrite: count(usage.cache_creation_input_tokens) })
    }
  }
  return kind === 'codex' ? (last ? [last] : []) : [...messages.values()]
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
  for (const { body } of comments) {
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
      const r = { cwd: '/', model: String(u.model ?? ''), input: count(u.input), output: count(u.output), cacheRead: count(u.cacheRead), cacheWrite: count(u.cacheWrite) }
      value.usage = aggregateUsage([r], { '/': row.ticket })[row.ticket]
      if (typeof u.usd === 'number' && Number.isFinite(u.usd) && u.usd >= 0) Object.assign(value.usage, { usd: u.usd, known: u.known !== false, partial: u.partial === true })
      value.usage.allocated = u.allocated === true
    }
    result[row.ticket] = value
  }
  return result
}
