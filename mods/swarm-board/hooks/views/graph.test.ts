import { afterAll, afterEach, expect, spyOn, test } from 'bun:test'
import * as cache from '../linear-cache'
import { buildGraph, renderGraph, type GraphInput } from './graph'

const input: GraphInput = { issues: [
  { id: 'A', state: 'unstarted', parent: 'P', blockedBy: [] },
  { id: 'B', state: 'unstarted', parent: 'P', blockedBy: ['A'] },
  { id: 'C', state: 'unstarted', parent: 'P', blockedBy: ['B'] },
  { id: 'D', state: 'unstarted', parent: 'Q', blockedBy: ['A'] },
] }

// Model only the injected element factory; never execute a React component directly.
const originalH = Object.getOwnPropertyDescriptor(globalThis, 'h')
Object.defineProperty(globalThis, 'h', { configurable: true, value: (_: unknown, props: unknown, ...children: string[]) => ({ props, children }) })
const Text = () => null
const lines = (graph?: Parameters<typeof renderGraph>[1]) =>
  renderGraph(Text, graph).map(element => (element as unknown as { children: string[] }).children.join(''))
afterEach(() => { snapshot?.mockRestore(); snapshot = undefined })
let snapshot: ReturnType<typeof spyOn> | undefined

afterAll(() => {
  if (originalH) Object.defineProperty(globalThis, 'h', originalH)
  else Reflect.deleteProperty(globalThis, 'h')
})

test('pure graph preserves input, groups states and indents dependency levels', () => {
  const before = structuredClone(input)
  const graph = buildGraph(input)!
  expect(graph.criticalPath).toEqual(['A', 'B', 'C'])
  expect(graph.unlockRank).toEqual([{ id: 'A', count: 3 }, { id: 'B', count: 1 }])
  expect(graph.groups).toEqual([
    { parent: 'P', rows: input.issues.slice(0, 3).map((issue, depth) => ({ id: issue.id, state: issue.state, blockedBy: issue.blockedBy, depth })) },
    { parent: 'Q', rows: [{ id: 'D', state: 'unstarted', blockedBy: ['A'], depth: 0 }] },
  ])
  expect(lines(graph)).toContain('      C [unstarted] ← B')
  expect(input).toEqual(before)
})

test('missing blockers stay blocked and are named in the headline', () => {
  const graph = buildGraph({ issues: [{ id: 'B', state: 'unstarted', blockedBy: ['external'] }] })!
  expect(graph.headline).toContain('仅 0 张可领')
  expect(graph.headline).toContain('external')
})

test('started work is not advertised as claimable and downstream impact is not immediate unlocks', () => {
  const graph = buildGraph({ issues: input.issues.map(issue => ({ ...issue, state: issue.id === 'A' ? 'started' : issue.state })) })!
  expect(graph.headline).toContain('仅 0 张可领')
  expect(graph.headline).not.toContain('可放出 3 张')
  expect(graph.headline).toContain('A')
})

test('finished and canceled blockers do not extend unfinished paths', () => {
  const graph = buildGraph({ issues: [
    { id: 'A', state: 'completed', blockedBy: [] },
    { id: 'B', state: 'canceled', blockedBy: ['A'] },
    { id: 'C', state: 'unstarted', blockedBy: ['B', 'B'] },
  ] })!
  expect(graph.criticalPath).toEqual(['C'])
  expect(graph.unlockRank).toEqual([])
  expect(graph.headline).toContain('仅 1 张可领')
})

test('cycles terminate without repeating tickets or claiming they are ready', () => {
  const graph = buildGraph({ issues: [
    { id: 'A', state: 'unstarted', blockedBy: ['B'] },
    { id: 'B', state: 'unstarted', blockedBy: ['A'] },
  ] })!
  expect(graph.criticalPath).toEqual(['A', 'B'])
  expect(graph.unlockRank).toEqual([{ id: 'A', count: 1 }, { id: 'B', count: 1 }])
  expect(graph.headline).toContain('仅 0 张可领')
})

test('empty input differs from unavailable input', () => {
  expect(buildGraph()).toBeNull()
  expect(lines(null)).toEqual(['数据未就绪'])
  expect(lines(buildGraph({ issues: [] }))[0]).toBe('无未完成票')
})

test('default render consumes the cache snapshot including grouped rows', () => {
  const graph = buildGraph(input)!
  snapshot = spyOn(cache, 'readGraphSnapshot').mockReturnValue(graph)
  expect(lines()).toContain('      C [unstarted] ← B')
  expect(snapshot).toHaveBeenCalledWith(expect.any(Number))
})

test('legacy GraphData snapshots render summary without requiring new fields', () => {
  snapshot = spyOn(cache, 'readGraphSnapshot').mockReturnValue({ criticalPath: ['A', 'B'], unlockRank: [{ id: 'A', count: 1 }] })
  expect(lines()).toContain('关键路径：A → B')
  expect(lines()).toContain('解锁排行：A(1)')
})
