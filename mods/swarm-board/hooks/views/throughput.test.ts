import { test, expect } from 'bun:test'
import { readFileSync } from 'node:fs'
import { resolve } from 'node:path'
import { buildThroughput, type AutomationRun } from './throughput'

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
})

test('skips legacy stage rows and reports nearest-rank p50 and p90', () => {
  const legacy = buildThroughput({ now: 0, ledger: [{ outcome: 'merged' }], runs: [] })
  expect(legacy.ready).toBe(false)
  expect(legacy.stages).toEqual([])
  const ledger = [
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
  expect(data.ready).toBe(true)
  expect(data.stages).toEqual([
    { key: 'queue', label: '等领取', p50: 20, p90: 40, n: 4 },
    { key: 'worker', label: 'worker', p50: 30, p90: 50, n: 2 },
    { key: 'gate', label: 'gate', p50: 4, p90: 4, n: 1 },
    { key: 'review', label: '审查', p50: 5, p90: 5, n: 1 },
    { key: 'merge', label: '开 PR 到合并', p50: 6, p90: 6, n: 1 },
  ])
})

test('keeps four no-ticket reasons and averages only recent prechecks', () => {
  const fixture: { result: { runs: AutomationRun[] } } = JSON.parse(readFileSync(
    resolve(import.meta.dir, '../../../../driver/testdata/recorded/orca-automations-runs.json'),
    'utf8',
  ))
  const now = 1791203751734 + 60_000
  const data = buildThroughput({ now, ledger: [], runs: fixture.result.runs, maxInFlight: 10 })
  expect(data.noTicket).toEqual([
    { label: '无可领票', count: 2 },
    { label: 'SCOPE 冲突', count: 0 },
    { label: '达到上限', count: 0 },
    { label: '限流', count: 1 },
  ])
  expect(data.inFlight).toEqual({ avg: 1 / 3, max: 10 })

  const recent = new Date(now - 1000).toISOString()
  const runs: AutomationRun[] = [
    { scheduledFor: recent, precheckResult: { stdout: 'claimable= in_flight=10 max=10' } },
    { startedAt: recent, precheckResult: {
      stdout: 'claimable= in_flight=10 max=10', stderr: 'skip ZII-888: SCOPE overlaps',
    } },
    { createdAt: now, precheckResult: {
      stdout: 'claimable= in_flight=10 max=10',
      stderr: 'skip ZII-888: scope overlaps\n不确定: rate limit',
    } },
    { createdAt: recent, precheckResult: { exitCode: 3, stderr: 'rate limit' } },
    { createdAt: now, precheckResult: {
      stdout: 'claimable= in_flight=0 max=0',
      stderr: 'skip /path/scope: cleanup\ncleanup mentions rate limit',
    } },
    { createdAt: recent, precheckResult: {
      stdout: 'claimable=ZII-889 in_flight=2 max=10',
      stderr: 'skip ZII-888: scope overlaps\n不确定: rate limit',
    } },
    { createdAt: now - 86_400_000, precheckResult: { stdout: 'claimable= in_flight=100 max=10' } },
    { createdAt: now + 1, precheckResult: { stdout: 'claimable= in_flight=100 max=10' } },
    { createdAt: 'invalid', precheckResult: { stdout: 'claimable= in_flight=100 max=10' } },
    { createdAt: now },
  ]
  const classified = buildThroughput({ now, ledger: [], runs, maxInFlight: 7, inFlightAvg: 99 })
  expect(classified.noTicket).toEqual([
    { label: '无可领票', count: 1 },
    { label: 'SCOPE 冲突', count: 1 },
    { label: '达到上限', count: 1 },
    { label: '限流', count: 2 },
  ])
  expect(classified.inFlight).toEqual({ avg: 6.4, max: 7 })
  expect(buildThroughput({ now, ledger: [], runs: [], maxInFlight: 10 }).inFlight)
    .toEqual({ avg: 0, max: 10 })
})
