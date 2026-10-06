import { test, expect } from 'claude-code/testing'
import { ledgerRows } from './view-data'
import { buildThroughput } from './views/throughput'
import { buildFailures } from './views/failures'

test('batched ledger comments deduplicate attempts and feed both metrics views', () => {
  const now = Date.parse('2026-10-06T12:00:00Z')
  const row = { ticket: 'ZII-1', started_at: '2026-10-06T10:00:00Z', finished_at: '2026-10-06T11:40:00Z', outcome: 'merged', stage_secs: { worker: 42 }, corrections: 2, tier: 'fast', lane: 2, first_gate: 'green' }
  const rows = ledgerRows([{ body: `ledger ${JSON.stringify(row)}\nledger bad\nledger []\nledger ${JSON.stringify(row)}` }])
  expect(rows.length).toBe(1)
  expect(buildThroughput({ now, ledger: rows, runs: [] }).hourly[23]).toBe(1)
  expect(buildFailures({ now, ledger: rows }).topCorrections[0]).toEqual({ ticket: 'ZII-1', corrections: 2 })
})

test('legacy rows retain missing durations and malformed optional metrics are ignored', () => {
  const rows = ledgerRows([{ body: 'ledger {"ticket":"A"}\nledger {"ticket":"B","corrections":"bad","stage_secs":{"worker":"oops"}}' }])
  expect(rows.length).toBe(2)
  expect(buildThroughput({ now: Date.now(), ledger: rows, runs: [] }).stages).toBe(null)
  expect(buildFailures({ ledger: rows }).topCorrections.length).toBe(0)
})
