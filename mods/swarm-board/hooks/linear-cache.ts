export const GRAPH_TTL_MS = 300_000
export const GRAPH_MAX_CALLS = 40

export type GraphSnapshot = {
  fetchedAt: number
  stale: boolean
  partial: boolean
  issues: {
    id: string
    state: string
    parent: string | null
    blockedBy: string[]
    incomplete: boolean
  }[]
}

type Runtime = {
  process: { run(argv: string[], options: { timeoutMs: number }): Promise<{ exitCode: number; stdout: string }> }
  fs: { read(path: string): Promise<string> }
}
type LinearIssue = {
  identifier: string
  state: { type: string }
  updatedAt: string
  parent?: { identifier: string } | null
  inverseRelations?: { nodes: { type: string; issue: { identifier: string } }[]; pageInfo?: { hasNextPage: boolean } }
}
type LinearResult = { issues?: LinearIssue[]; issue?: LinearIssue; truncated?: boolean }

let cache: GraphSnapshot | null = null
let lastSuccessAt: number | null = null
let window: { start: number; calls: number } | null = null
let countPath: Promise<string> | null = null

// Live verification (2026-10-05): list-issues returned 120 issues, none with parent.
// issue --relations also omitted parent/inverseRelations; missing fields stay incomplete.
export async function readGraphSnapshot(now: number, $: Runtime): Promise<GraphSnapshot | null> {
  if (cache && lastSuccessAt !== null && now - lastSuccessAt < GRAPH_TTL_MS) return cache
  if (!window || now - window.start >= GRAPH_TTL_MS) window = { start: now, calls: 0 }
  const stale = () => cache ? { ...cache, stale: true } : null

  const read = async (args: string[]): Promise<LinearResult | null> => {
    if (!window || window.calls >= GRAPH_MAX_CALLS) return null
    window = { start: window.start, calls: window.calls + 1 }
    try {
      if (!countPath) {
        countPath = $.process.run(['sh', '-c', 'printf %s "${AGENT_LINEAR_COUNT_FILE:-${TMPDIR:-/tmp}/orca-factory-linear-calls}"'], { timeoutMs: 20_000 })
          .then(result => {
            if (result.exitCode !== 0) throw new Error('Cannot resolve Linear count file')
            return result.stdout.trim()
          })
      }
      await $.process.run(['sh', '-c', 'printf %s "$1" >> "$2"', 'board', `${Math.floor(now / 1000)} board\n`, await countPath], { timeoutMs: 20_000 })
    } catch {
      // Accounting is best effort: an unwritable counter must not prevent Linear reads.
    }
    try {
      const response = await $.process.run(['orca', 'linear', ...args, '--json'], { timeoutMs: 20_000 })
      if (response.exitCode !== 0) return null
      const envelope = JSON.parse(response.stdout)
      if (envelope === null || envelope.ok === false) return null
      return envelope.result ?? null
    } catch {
      // A process rejection or malformed JSON is a failed read, including single-ticket reads.
      return null
    }
  }

  if (window.calls >= GRAPH_MAX_CALLS) return stale()
  let result: LinearResult | null
  try {
    const text = await $.fs.read('scripts/agent-gates/agent-graph.env')
    const team = /^AGENT_LINEAR_TEAM=([^\s#]+)/m.exec(text)?.[1]
    const project = /^AGENT_LINEAR_PROJECT=([^\s#]+)/m.exec(text)?.[1]
    if (!team || !project) return stale()
    result = await read(['list-issues', '--team', team, '--project', project])
  } catch {
    // Without the project profile a scoped read cannot succeed; retain the last snapshot.
    return stale()
  }
  if (!result || !Array.isArray(result.issues)) return stale()

  const dayStart = new Date(now)
  dayStart.setHours(0, 0, 0, 0)
  const selected = result.issues.filter(issue => issue.state.type !== 'canceled'
    && (issue.state.type !== 'completed' || Date.parse(issue.updatedAt) >= dayStart.getTime()))
  const issues: GraphSnapshot['issues'] = []
  for (const issue of selected) {
    let full = issue
    let incomplete = false
    if (issue.parent === undefined || !Array.isArray(issue.inverseRelations?.nodes)) {
      // One detail call supplies parent and inverse relations when the CLI exposes them.
      const detail = await read(['issue', issue.identifier, '--relations'])
      if (detail?.issue) full = { ...issue, ...detail.issue }
      else incomplete = true
    }
    const relations = full.inverseRelations
    incomplete ||= full.parent === undefined || !Array.isArray(relations?.nodes)
      || relations?.pageInfo?.hasNextPage === true
    issues.push({
      id: issue.identifier,
      state: full.state.type,
      parent: full.parent?.identifier ?? null,
      // A blocked by B is stored on B: A.inverseRelations[*].issue identifies B.
      blockedBy: (relations?.nodes ?? []).filter(relation => relation.type === 'blocks')
        .map(relation => relation.issue.identifier),
      incomplete,
    })
  }
  cache = { fetchedAt: now, stale: false, partial: result.truncated === true || issues.some(issue => issue.incomplete), issues }
  lastSuccessAt = now
  return cache
}
