import { expect, test } from 'claude-code/testing'
import { buildFailures } from './failures'

test('counts FAIL reasons with or without a trailing evidence field', () => {
  const data = buildFailures({ comments: { 'ZII-1': [
    'FAIL: approach=a | reason=gate',
    'FAIL: approach=b | reason=gate | evidence=test failed',
  ] } })
  expect(data.repeatedFails).toEqual([{ reason: 'gate', count: 2 }])
})

test('only counts handoffs with timestamps in the last seven days', () => {
  const now = Date.parse('2026-10-06T12:00:00Z')
  const data = buildFailures({ now, comments: { 'ZII-1': [
    { body: '转人: scope', createdAt: '2026-10-05T12:00:00Z' },
    { body: '转人: old', createdAt: '2026-09-01T12:00:00Z' },
    { body: '转人: future', createdAt: '2026-10-07T12:00:00Z' },
    { body: '转人: invalid', createdAt: 'invalid' },
    '转人: undated',
  ] } })
  expect(data.handoffReasons).toEqual([{ reason: 'scope', count: 1 }])
})

test('ranks corrections and excludes unknown first-gate outcomes', () => {
  const data = buildFailures({ ledger: [
    { ticket: 'X-1', tier: 'fast', lane: 2, first_gate: 'red', corrections: 3 },
    { ticket: 'X-2', tier: 'fast', lane: 2, first_gate: 'green', corrections: 0 },
    { ticket: 'X-3', tier: 'fast', lane: 2 },
  ] })
  expect(data.topCorrections).toEqual([{ ticket: 'X-1', corrections: 3 }])
  expect(data.firstGate).toEqual([{ key: 'fast/lane2', green: 1, total: 2 }])
})
