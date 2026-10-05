export const GRAPH_TTL_MS = 300_000
export const GRAPH_MAX_CALLS = 40

export type GraphSnapshot = {
  fetchedAt: number
  stale: boolean
  error?: string
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
  relations?: LinearRelation[]
}
type LinearRelation = { relationship: string; relatedIssue: { identifier: string } }
type LinearResult = { issues?: LinearIssue[]; issue?: LinearIssue; relations?: LinearRelation[]; truncated?: boolean }

let cache: GraphSnapshot | null = null
let lastSuccessAt: number | null = null
let window: { start: number; calls: number } | null = null
let countPath: Promise<string> | null = null

// Verified 2026-10-05: orca linear list-issues --team <T> --project <P> --json and orca linear issue <id> --relations --json omit parent; detail relations are result.relations.
export async function readGraphSnapshot(now: number, $: Runtime): Promise<GraphSnapshot | null> {
  if (cache && lastSuccessAt !== null && now - lastSuccessAt < GRAPH_TTL_MS) return cache
  if (!window || now - window.start >= GRAPH_TTL_MS) window = { start: now, calls: 0 }
  const stale = () => cache ? { ...cache, stale: true } : null

  const read = async (args: string[]): Promise<LinearResult | null> => {
    if (!window || window.calls >= GRAPH_MAX_CALLS) return null
    window = { start: window.start, calls: window.calls + 1 }
    if (!countPath) {
      countPath = $.process.run(['sh', '-c', 'printf %s "${AGENT_LINEAR_COUNT_FILE:-${TMPDIR:-/tmp}/orca-factory-linear-calls}"'], { timeoutMs: 20_000 })
        .then(result => {
          if (result.exitCode !== 0) throw new Error('Cannot resolve Linear count file')
          return result.stdout.trim()
        })
    }
    await $.process.run(['sh', '-c', '{ printf %s "$1" >> "$2"; } 2>/dev/null || true', 'board', `${Math.floor(now / 1000)} board\n`, await countPath], { timeoutMs: 20_000 })
    const response = await $.process.run(['orca', 'linear', ...args, '--json'], { timeoutMs: 20_000 })
    if (response.exitCode !== 0) throw new Error(`Linear ${args[0]} failed with exit code ${response.exitCode}`)
    const envelope = JSON.parse(response.stdout)
    if (envelope === null || envelope.ok === false || envelope.result == null) {
      throw new Error(typeof envelope?.error === 'string' ? envelope.error : `Linear ${args[0]} returned a failed response`)
    }
    return envelope.result
  }

  if (window.calls >= GRAPH_MAX_CALLS) return stale()
  try {
    const text = await $.fs.read('scripts/agent-gates/agent-graph.env')
    const team = /^AGENT_LINEAR_TEAM=([^\s#]+)/m.exec(text)?.[1]
    const project = /^AGENT_LINEAR_PROJECT=([^\s#]+)/m.exec(text)?.[1]
    if (!team || !project) throw new Error('Missing Linear team or project in agent profile')
    const result = await read(['list-issues', '--team', team, '--project', project])
    if (!result || !Array.isArray(result.issues)) throw new Error('Linear list-issues returned no issue list')

    const dayStart = new Date(now)
    dayStart.setHours(0, 0, 0, 0)
    const selected = result.issues.filter(issue => issue.state.type !== 'canceled'
      && (issue.state.type !== 'completed' || Date.parse(issue.updatedAt) >= dayStart.getTime()))
    const issues: GraphSnapshot['issues'] = []
    for (const issue of selected) {
      let full = issue
      let incomplete = false
      if (!Array.isArray(issue.relations)) {
        const detail = await read(['issue', issue.identifier, '--relations'])
        if (detail?.issue && Array.isArray(detail.relations)) full = { ...issue, ...detail.issue, relations: detail.relations }
        else incomplete = true
      }
      issues.push({
        id: issue.identifier,
        state: full.state.type,
        parent: full.parent?.identifier ?? null,
        blockedBy: (full.relations ?? []).filter(relation => relation.relationship === 'blockedBy')
          .map(relation => relation.relatedIssue.identifier),
        incomplete,
      })
    }
    cache = { fetchedAt: now, stale: false, partial: result.truncated === true || issues.some(issue => issue.incomplete), issues }
    lastSuccessAt = now
    return cache
  } catch (error) {
    return cache ? { ...cache, stale: true, error: error instanceof Error ? error.message : String(error) } : null
  }
}
