import { test, expect } from 'bun:test'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { buildThroughput, renderThroughput, type LedgerRow, type Run } from './throughput'

test('counts merged completions in 24 rolling hourly buckets', () => {
  const now = Date.parse('2026-10-05T12:00:00Z')
  const r = buildThroughput({
    now,
    ledger: [
      { outcome: 'merged', finished_at: '2026-10-05T11:10:00Z' },
      { outcome: 'merged', finished_at: '2026-10-05T11:40:00Z' },
    ],
    runs: [],
  })
  expect(r.hourly.length).toBe(24)
  expect(r.hourly[23]).toBe(2)

  const boundaries = buildThroughput({
    now,
    ledger: [
      { outcome: 'merged', finished_at: '2026-10-05T12:00:00Z' },
      { outcome: 'merged', finished_at: '2026-10-05T11:00:00Z' },
      { outcome: 'merged', finished_at: '2026-10-04T12:00:00.001Z' },
      { outcome: 'merged', finished_at: '2026-10-04T12:00:00Z' },
      { outcome: 'merged', finished_at: '2026-10-05T12:00:00.001Z' },
      { outcome: 'merged', finished_at: 'invalid' },
      { outcome: 'escalated', finished_at: '2026-10-05T11:40:00Z' },
    ],
    runs: [],
  })
  expect(boundaries.hourly[23]).toBe(1)
  expect(boundaries.hourly[22]).toBe(1)
  expect(boundaries.hourly[0]).toBe(1)
  expect(boundaries.hourly.reduce((sum, count) => sum + count, 0)).toBe(3)

  const ledger: LedgerRow[] = [
    { stage_secs: { queue: 10, worker: 30, gate: 4, review: 5, merge: 6 } },
    { stage_secs: { queue: 20, worker: 50, gate: NaN, review: Infinity } },
    { stage_secs: { queue: 30 } },
    { stage_secs: { queue: 40 } },
    { outcome: 'merged' },
  ]
  for (const row of ledger) {
    if (row.stage_secs) Object.freeze(row.stage_secs)
    Object.freeze(row)
  }
  Object.freeze(ledger)
  const data = buildThroughput({ now: 0, ledger, runs: [] })
  expect(data.stages).toEqual({
    queue: { p50: 20, p90: 40, n: 4 },
    worker: { p50: 30, p90: 50, n: 2 },
    gate: { p50: 4, p90: 4, n: 1 },
    review: { p50: 5, p90: 5, n: 1 },
    merge: { p50: 6, p90: 6, n: 1 },
  })
})

test('keeps null legacy stages, four reasons and recent precheck averages', () => {
  const legacy = buildThroughput({ now: 0, ledger: [{ outcome: 'merged' }], runs: [] })
  expect(legacy.stages).toBeNull()
  expect(legacy.noTicket).toEqual({ noClaimable: 0, scopeConflict: 0, atCap: 0, rateLimited: 0 })
  expect(legacy.inFlightAvg).toBeNull()
  expect(buildThroughput({ now: 0, ledger: [{ stage_secs: { queue: NaN, worker: Infinity } }], runs: [] }).stages)
    .toBeNull()
  const fixture: { result: { runs: Run[] } } = JSON.parse(readFileSync(
    resolve(import.meta.dir, '../../../../driver/testdata/recorded/orca-automations-runs.json'),
    'utf8',
  ))
  const now = 1791203751734 + 60_000
  const data = buildThroughput({ now, ledger: [], runs: fixture.result.runs, maxInFlight: 10 })
  expect(data.noTicket).toEqual({ noClaimable: 2, scopeConflict: 1, atCap: 1, rateLimited: 1 })
  expect(data.inFlightAvg).toBe(12 / 5)
  expect(data.maxInFlight).toBe(10)

  const recent = new Date(now - 1000).toISOString()
  const runs: Run[] = [
    { createdAt: recent, precheckResult: { stdout: 'claimable= in_flight=10 max=10' } },
    { startedAt: recent, precheckResult: {
      stdout: 'claimable= in_flight=10 max=10', stderr: 'skip ZII-888: scope overlaps path',
    } },
    { createdAt: now - 500, precheckResult: {
      stdout: 'claimable= in_flight=10 max=10',
      stderr: 'skip ZII-888: scope overlaps\n不确定: linear rate limited',
    } },
    { createdAt: recent, precheckResult: { timedOut: true, stderr: 'rate limited' } },
    { createdAt: now - 500, precheckResult: {
      stdout: 'claimable= in_flight=0 max=0',
      stderr: 'skip /path/scope: cleanup\ncleanup mentions rate limit',
    } },
    { createdAt: now - 500, precheckResult: {
      stdout: 'claimable=ZII-889 in_flight=2 max=10',
      stderr: 'skip ZII-888: scope overlaps\n不确定: linear rate limited',
    } },
    { createdAt: now - 86_400_000, precheckResult: { stdout: 'claimable= in_flight=100 max=10' } },
    { createdAt: now + 1, precheckResult: { stdout: 'claimable= in_flight=100 max=10' } },
    { createdAt: 'invalid', precheckResult: { stdout: 'claimable= in_flight=100 max=10' } },
    { createdAt: now },
    { createdAt: now - 86_400_000, startedAt: recent, precheckResult: {
      stdout: 'diagnostic output\nclaimable= in_flight=1 max=10',
      stderr: 'skip OTHER-900: scope overlaps mods/swarm-board/hooks/views/throughput.tsx',
    } },
    { createdAt: now, startedAt: now + 1, precheckResult: { stdout: 'claimable= in_flight=100 max=10' } },
    { createdAt: now, precheckResult: { stdout: 'diagnostic output\nclaimable= in_flight=2 max=12' } },
    { createdAt: recent, precheckResult: { stdout: 'in_flight=100 max=99' } },
    { createdAt: now, startedAt: 'invalid', precheckResult: { stdout: 'claimable= in_flight=100 max=10' } },
  ]
  const classified = buildThroughput({ now, ledger: [], runs, maxInFlight: 7 })
  expect(classified.noTicket).toEqual({ noClaimable: 1, scopeConflict: 1, atCap: 3, rateLimited: 2 })
  expect(classified.inFlightAvg).toBe(5)
  expect(classified.maxInFlight).toBe(7)
  expect(buildThroughput({ now, ledger: [], runs }).maxInFlight).toBe(12)
  expect(buildThroughput({ now, ledger: [], runs: [...runs].reverse() }).maxInFlight).toBe(12)
  expect(buildThroughput({ now, ledger: [], runs: [], maxInFlight: 10 }).maxInFlight).toBe(10)

  const previousH = Object.getOwnPropertyDescriptor(globalThis, 'h')
  Object.defineProperty(globalThis, 'h', {
    configurable: true,
    value: (type: unknown, props: unknown, ...children: unknown[]) => ({ type, props, children }),
  })
  try {
    const Text = () => undefined
    expect(renderThroughput(Text)).toEqual({ type: Text, props: { dimColor: true }, children: ['数据未就绪'] })
    const lines = renderThroughput(Text, classified)
    expect(lines).toContainEqual({ type: Text, props: { dimColor: true }, children: ['数据未就绪'] })
    expect(lines).toContainEqual({ type: Text, props: null, children: ['SCOPE 冲突：1'] })
    expect(lines).toContainEqual({ type: Text, props: null, children: ['在途均值 5.0 / 上限 7'] })
  } finally {
    if (previousH) Object.defineProperty(globalThis, 'h', previousH)
    else Reflect.deleteProperty(globalThis, 'h')
  }
})
