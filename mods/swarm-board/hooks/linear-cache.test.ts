import { expect, test } from 'claude-code/testing'
import type { Runner } from './linear-cache'
import * as graphCache from './linear-cache'

const fresh = () => { graphCache.__resetGraphCacheForTests(); return graphCache }
const now = Date.parse('2026-10-06T03:00:00Z')
const ctx = { team: 'ZII', project: 'orca-factory', tmpDir: '/tmp/graph-tests' }
// Orca CLI 2026-10-06: list/issue omit parent; children is an array of issue rows.
// Recorded probes: /tmp/orca-takeover/886-{list,issue,children}-probe.json.
const row = (identifier: string, type = 'started', extra = {}) => ({
  identifier, title: identifier, state: { type }, updatedAt: new Date(now).toISOString(), ...extra,
})
const blocked = (id: string) => ({ relationship: 'blockedBy', relatedIssue: { identifier: id } })
function fixture(issues: ReturnType<typeof row>[], roots: string[], details: Record<string, unknown>) {
  const calls: string[][] = []
  const counts: { path: string; text: string }[] = []
  const runner: Runner = {
    fs: { append: async (path, text) => { counts.push({ path, text }) } },
    process: { run: async argv => {
      calls.push(argv)
      const result = argv[2] === 'list-issues'
        ? { issues: argv.includes('--parent-id') ? issues.filter(r => roots.includes(r.identifier)) : issues }
        : details[argv[3]]
      return { stdout: JSON.stringify({ ok: true, result }), exitCode: 0 }
    } },
  }
  return { runner, calls, counts }
}

test('reads parents and completed blockers when Orca omits parent on both list and issue', async () => {
  const cache = await fresh()
  const f = fixture([row('ZII-1'), row('ZII-2'), row('ZII-3', 'completed')], ['ZII-1', 'ZII-3'], {
    'ZII-1': { issue: row('ZII-1'), children: [row('ZII-2')], relations: [] },
    'ZII-2': { issue: row('ZII-2'), children: [], relations: [blocked('ZII-3')] },
    'ZII-3': { issue: row('ZII-3', 'completed'), children: [], relations: [] },
  })
  const snapshot = await cache.readGraphSnapshot(now, f.runner, ctx)
  expect(snapshot).toEqual({ at: now, stale: false, incomplete: [], issues: [
    { id: 'ZII-1', title: 'ZII-1', state: 'started', parent: null, blockedBy: [] },
    { id: 'ZII-2', title: 'ZII-2', state: 'started', parent: 'ZII-1', blockedBy: ['ZII-3'] },
    { id: 'ZII-3', title: 'ZII-3', state: 'completed', parent: null, blockedBy: [] },
  ] })
})

test('a failed relation refresh retains the entire old snapshot and exposes stale to redraws', async () => {
  const cache = await fresh()
  const f = fixture([row('ZII-1', 'started', { parent: null })], [], {
    'ZII-1': { issue: row('ZII-1'), children: [], relations: [blocked('ZII-2')] },
  })
  const old = await cache.readGraphSnapshot(now, f.runner, ctx)
  const run = f.runner.process.run
  f.runner.process.run = async argv => argv[2] === 'issue'
    ? { exitCode: 1, stdout: '' } : run(argv, { timeoutMs: 30000 })
  const failed = await cache.readGraphSnapshot(now + 300000, f.runner, ctx)
  expect(failed).toEqual({ ...old, stale: true })
  expect(cache.peekGraphSnapshot()).toEqual(failed)
})

test('finds a parent outside the recent-completed display window', async () => {
  const cache = await fresh()
  const f = fixture([row('ZII-1'), row('ZII-2', 'completed', { updatedAt: '2020-01-01T00:00:00Z' })], ['ZII-2'], {
    'ZII-1': { issue: row('ZII-1'), children: [], relations: [] },
    'ZII-2': { issue: row('ZII-2'), children: [row('ZII-1')], relations: [] },
  })
  const snapshot = await cache.readGraphSnapshot(now, f.runner, ctx)
  expect(snapshot.issues).toEqual([{ id: 'ZII-1', title: 'ZII-1', state: 'started', parent: 'ZII-2', blockedBy: [] }])
  expect(snapshot.incomplete).toEqual([])
})

test('partial or capped Linear responses never replace last-good data', async () => {
  const cache = await fresh()
  const f = fixture([row('ZII-1', 'started', { parent: null })], [], {
    'ZII-1': { issue: row('ZII-1'), children: [], relations: [blocked('ZII-2')] },
  })
  const old = await cache.readGraphSnapshot(now, f.runner, ctx)
  const run = f.runner.process.run
  for (const [index, partial] of [
    { meta: { partial: true, includeErrors: [{ section: 'relations' }] } },
    { meta: { sections: { relations: { hasMore: true } } } },
    { truncated: true },
  ].entries()) {
    f.runner.process.run = async argv => {
      const response = await run(argv, { timeoutMs: 30000 })
      const parsed = JSON.parse(response.stdout)
      Object.assign(parsed.result, partial)
      return { ...response, stdout: JSON.stringify(parsed) }
    }
    expect(await cache.readGraphSnapshot(now + (index + 1) * 300000, f.runner, ctx)).toEqual({ ...old, stale: true })
  }
})

test('concurrent refreshes share one read, then TTL hits and redraws spend no calls', async () => {
  const cache = await fresh()
  const f = fixture([row('ZII-1', 'started', { parent: null })], [], {
    'ZII-1': { issue: row('ZII-1'), children: [], relations: [] },
  })
  const [a, b] = await Promise.all([
    cache.readGraphSnapshot(now, f.runner, ctx), cache.readGraphSnapshot(now, f.runner, ctx),
  ])
  expect(a).toBe(b)
  expect(f.calls).toHaveLength(2)
  expect(await cache.readGraphSnapshot(now + 299999, f.runner, ctx)).toBe(a)
  for (let i = 0; i < 10; i++) cache.peekGraphSnapshot()
  expect(f.calls).toHaveLength(2)
  await cache.readGraphSnapshot(now + 300000, f.runner, ctx)
  expect(f.calls).toHaveLength(4)
})

test('budget permits only 40 calls including failed attempts and resets at five minutes', async () => {
  const cache = await fresh()
  const rows = Array.from({ length: 45 }, (_, i) => row(`ZII-${i + 1}`, 'started', { parent: null }))
  const details = Object.fromEntries(rows.map(r => [r.identifier, { issue: r, children: [], relations: [] }]))
  const f = fixture(rows, [], details)
  const snapshot = await cache.readGraphSnapshot(now, f.runner, ctx)
  expect(f.calls).toHaveLength(40)
  expect(snapshot.incomplete).toEqual(['ZII-40', 'ZII-41', 'ZII-42', 'ZII-43', 'ZII-44', 'ZII-45'])
  expect(f.counts).toHaveLength(40)
  expect(f.counts.every(c => c.path === '/tmp/graph-tests/orca-factory-linear-calls' && c.text === '1791255600 board\n')).toBe(true)
  await cache.readGraphSnapshot(now + 299999, f.runner, ctx)
  expect(f.calls).toHaveLength(40)
  await cache.readGraphSnapshot(now + 300000, f.runner, ctx)
  expect(f.calls).toHaveLength(80)

  const failing = await fresh()
  const bad = fixture([], [], {})
  bad.runner.process.run = async argv => { bad.calls.push(argv); throw new Error('offline') }
  for (let i = 0; i < 45; i++) expect((await failing.readGraphSnapshot(now + i, bad.runner, ctx)).stale).toBe(true)
  expect(bad.calls).toHaveLength(40)
  expect(bad.counts).toHaveLength(40)
  await failing.readGraphSnapshot(now + 300000, bad.runner, ctx)
  expect(bad.calls).toHaveLength(41)
})

test('shell counter fallback passes directory and clock as literal arguments', async () => {
  const cache = await fresh()
  const f = fixture([], [], {})
  delete f.runner.fs.append
  const commands: string[][] = []
  const run = f.runner.process.run
  f.runner.process.run = async argv => {
    if (argv[0] !== 'sh') return run(argv, { timeoutMs: 30000 })
    commands.push(argv)
    return { stdout: '', exitCode: 0 }
  }
  await cache.readGraphSnapshot(now, f.runner, { ...ctx, tmpDir: "/tmp/graph-'$-/" })
  expect(commands).toEqual([['sh', '-c', 'printf "%s board\\n" "$1" >> "$2"',
    'board-counter', '1791255600', "/tmp/graph-'$-/orca-factory-linear-calls"]])
})

test('different projects never share cached issues or in-flight reads', async () => {
  const cache = await fresh()
  const a = fixture([row('ZII-1', 'started', { parent: null })], [], {
    'ZII-1': { issue: row('ZII-1'), children: [], relations: [] },
  })
  const b = fixture([row('ZII-2', 'started', { parent: null })], [], {
    'ZII-2': { issue: row('ZII-2'), children: [], relations: [] },
  })
  const [first, second] = await Promise.all([
    cache.readGraphSnapshot(now, a.runner, ctx),
    cache.readGraphSnapshot(now, b.runner, { ...ctx, project: 'other' }),
  ])
  expect(first.issues.map(r => r.id)).toEqual(['ZII-1'])
  expect(second.issues.map(r => r.id)).toEqual(['ZII-2'])
  expect(await cache.readGraphSnapshot(now + 1, a.runner, ctx)).toBe(first)
  expect(a.calls).toHaveLength(2)
})

test('malformed root and child reads cannot silently erase known relationships', async () => {
  for (const broken of ['roots', 'children', 'identity']) {
    const cache = await fresh()
    const f = fixture([row('ZII-1')], ['ZII-1'], {
      'ZII-1': { issue: row('ZII-1'), children: [], relations: [blocked('ZII-2')] },
    })
    const old = await cache.readGraphSnapshot(now, f.runner, ctx)
    const run = f.runner.process.run
    f.runner.process.run = async argv => {
      const response = await run(argv, { timeoutMs: 30000 })
      const parsed = JSON.parse(response.stdout)
      if (broken === 'roots' && argv.includes('--parent-id')) parsed.result = {}
      if (broken === 'children' && argv[2] === 'issue') parsed.result.children = [{ identifier: 12 }]
      if (broken === 'identity' && argv[2] === 'issue') parsed.result.issue = row('ZII-99')
      return { ...response, stdout: JSON.stringify(parsed) }
    }
    expect(await cache.readGraphSnapshot(now + 300000, f.runner, ctx)).toEqual({ ...old, stale: true })
  }
})

// Projected verbatim from `orca linear issue ZII-886 --children --depth 1
// --relations --json`, 2026-10-06. Parent is absent even on issue detail.
const recorded886 = {
  issue: { identifier: 'ZII-886', title: '看板 Linear 读取缓存层（5 分钟 TTL、每窗口 40 次预算）', state: { type: 'started' } },
  children: [],
  relations: [{ relationship: 'blockedBy', relatedIssue: { identifier: 'ZII-884' } }],
  meta: {
    partial: false, includeErrors: [],
    sections: {
      children: { returned: 0, cap: 200, capReached: false, mayHaveMore: false },
      relations: { returned: 1, cap: 100, capReached: false, hasMore: false },
    },
  },
}
test('accepts the recorded Orca envelope without inventing a missing parent field', async () => {
  const cache = await fresh()
  const f = fixture([row('ZII-886')], ['ZII-886'], { 'ZII-886': recorded886 })
  const snapshot = await cache.readGraphSnapshot(now, f.runner, ctx)
  expect(snapshot.issues[0].blockedBy).toEqual(['ZII-884'])
  expect(snapshot.issues[0].parent).toBe(null)
  expect(snapshot.incomplete).toEqual([])
  expect(snapshot.stale).toBe(false)
})
