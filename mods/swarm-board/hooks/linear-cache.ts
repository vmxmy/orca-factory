const GRAPH_TTL_MS = 300_000
const GRAPH_MAX_CALLS = 40
const COUNTER_SOURCE = 'board'
const RECENT_COMPLETED_MS = 86_400_000

export type GraphIssue = {
  id: string
  state: string
  parentId: string | null
  blockedBy: string[]
  complete: boolean
}

export type GraphSnapshot = {
  at: number
  issues: GraphIssue[]
  stale: boolean
  incomplete: string[]
}

export type RunFn = (argv: string[]) => Promise<{ exitCode: number; stdout: string }>

type Row = Record<string, unknown>
type ListedIssue = Row & { identifier: string; state: { type: string } }

let cache: GraphSnapshot | null = null
let windowStart: number | null = null
let windowCalls = 0
let failedAt: number | null = null
let pending: Promise<GraphSnapshot | null> | null = null
let counterPath: string | null = null

function isRecord(value: unknown): value is Row {
  return typeof value === 'object' && value !== null && !Array.isArray(value)
}

function isIssue(value: unknown): value is ListedIssue {
  return isRecord(value) && typeof value.identifier === 'string' && value.identifier.length > 0
    && isRecord(value.state) && typeof value.state.type === 'string'
}

function isPartial(result: Row): boolean {
  const meta = isRecord(result.meta) ? result.meta : null
  const sections = meta && isRecord(meta.sections) ? Object.values(meta.sections) : []
  return result.truncated === true || meta?.partial === true || meta?.hasMore === true
    || sections.some(section => isRecord(section)
      && (section.capReached === true || section.hasMore === true || section.mayHaveMore === true))
}

async function safeRun(run: RunFn, argv: string[]): ReturnType<RunFn> {
  try {
    return await run(argv)
  } catch {
    // Sandbox transport rejections are read failures; counter failures must also leave reads available.
    return { exitCode: 1, stdout: '' }
  }
}

async function appendCount(now: number, run: RunFn): Promise<boolean> {
  if (counterPath === null) {
    const resolved = await safeRun(run, ['sh', '-c',
      'printf %s "${AGENT_LINEAR_COUNT_FILE:-${TMPDIR:-/tmp}/orca-factory-linear-calls}"'])
    if (resolved.exitCode !== 0 || !resolved.stdout) return false
    counterPath = resolved.stdout
  }
  const written = await safeRun(run, ['sh', '-c',
    `printf "%s ${COUNTER_SOURCE}\\n" "$1" >> "$2"`, 'sh', String(Math.floor(now / 1000)), counterPath])
  return written.exitCode === 0
}

async function readLinear(args: string[], now: number, run: RunFn): Promise<Row | null> {
  if (windowCalls >= GRAPH_MAX_CALLS) return null
  windowCalls += 1
  const response = await safeRun(run, ['orca', 'linear', ...args, '--json'])
  await appendCount(now, run)
  if (response.exitCode !== 0) return null
  let envelope: unknown
  try {
    envelope = JSON.parse(response.stdout)
  } catch {
    return null
  }
  return isRecord(envelope) && envelope.ok !== false && isRecord(envelope.result) ? envelope.result : null
}

function parentOf(issue: Row, identifiers: Map<string, string>): string | null | undefined {
  if (issue.parent === null) return null
  if (isRecord(issue.parent)) {
    if (typeof issue.parent.identifier === 'string' && issue.parent.identifier) return issue.parent.identifier
    if (typeof issue.parent.id === 'string' && issue.parent.id) return identifiers.get(issue.parent.id) ?? issue.parent.id
  }
  if (issue.parentId === null) return null
  if (typeof issue.parentId === 'string' && issue.parentId) return identifiers.get(issue.parentId) ?? issue.parentId
  return undefined
}

function blockersOf(value: unknown): string[] | null {
  if (!Array.isArray(value)) return null
  const blockers: string[] = []
  for (const relation of value) {
    if (!isRecord(relation) || typeof relation.relationship !== 'string') return null
    if (relation.relationship !== 'blockedBy') continue
    if (!isRecord(relation.relatedIssue) || typeof relation.relatedIssue.identifier !== 'string'
      || !relation.relatedIssue.identifier) return null
    blockers.push(relation.relatedIssue.identifier)
  }
  return [...new Set(blockers)]
}

async function enrichIssue(issue: ListedIssue, identifiers: Map<string, string>, now: number, run: RunFn): Promise<GraphIssue> {
  // Verified list-issues omits parent/parentId; issue --relations also currently omits them, so unknown parents stay incomplete.
  const parent = parentOf(issue, identifiers)
  const blockers = blockersOf(issue.relations)
  const base: GraphIssue = {
    id: issue.identifier, state: issue.state.type, parentId: parent ?? null,
    blockedBy: blockers ?? [], complete: false,
  }
  if (parent !== undefined && blockers !== null) return { ...base, complete: true }
  const detail = await readLinear(['issue', issue.identifier, '--relations'], now, run)
  if (!detail || !isIssue(detail.issue) || detail.issue.identifier !== issue.identifier) return base
  const resolvedParent = parent !== undefined ? parent : parentOf(detail.issue, identifiers)
  const resolvedBlockers = blockersOf(detail.relations)
  return {
    id: issue.identifier,
    state: detail.issue.state.type,
    parentId: resolvedParent ?? null,
    blockedBy: resolvedBlockers ?? base.blockedBy,
    complete: resolvedParent !== undefined && resolvedBlockers !== null && !isPartial(detail),
  }
}

function staleSnapshot(): GraphSnapshot | null {
  return cache ? { ...cache, stale: true } : null
}

async function refresh(now: number, run: RunFn, env: { team: string; project: string }): Promise<GraphSnapshot | null> {
  const listed = await readLinear(['list-issues', '--team', env.team, '--project', env.project], now, run)
  if (!listed || !Array.isArray(listed.issues) || !listed.issues.every(isIssue) || isPartial(listed)) {
    failedAt = now
    return staleSnapshot()
  }
  const identifiers = new Map<string, string>()
  for (const issue of listed.issues) {
    if (typeof issue.id === 'string') identifiers.set(issue.id, issue.identifier)
  }
  const selected = listed.issues.filter(issue => issue.state.type !== 'canceled'
    && (issue.state.type !== 'completed' || (typeof issue.updatedAt === 'string'
      && Date.parse(issue.updatedAt) >= now - RECENT_COMPLETED_MS)))
  const issues: GraphIssue[] = []
  for (const issue of selected) issues.push(await enrichIssue(issue, identifiers, now, run))
  cache = { at: now, issues, stale: false, incomplete: issues.filter(issue => !issue.complete).map(issue => issue.id) }
  failedAt = null
  return cache
}

export function readGraphSnapshot(now: number, run: RunFn, env: { team: string; project: string }): Promise<GraphSnapshot | null> {
  if (cache && now - cache.at < GRAPH_TTL_MS) return Promise.resolve(cache)
  if (pending) return pending
  if (failedAt !== null && now - failedAt < GRAPH_TTL_MS) return Promise.resolve(staleSnapshot())
  if (windowStart === null || now - windowStart >= GRAPH_TTL_MS) {
    windowStart = now
    windowCalls = 0
  }
  pending = refresh(now, run, env).finally(() => { pending = null })
  return pending
}
