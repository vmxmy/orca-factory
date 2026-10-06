import { recordedUsage } from './usage-recorded'
import { test, expect } from 'claude-code/testing'
import { aggregateUsage, createUsageReader, ledgerUsage, parseSession } from './usage'

test('ledger uses the published driver contract including allocation and cost', () => {
  const row = ledgerUsage([{ body: 'ledger ' + JSON.stringify({ ticket: 'ZII-1', minutes: 12, usage: { ticket: 'ZII-1', input: 10, output: 5, cache_read: 20, cache_write: 30, reasoning: 2, cost_usd: 0.42, price_version: 'recorded', apportioned: true, by_role: {} } }) }])['ZII-1']
  expect(row.minutes).toBe(12)
  expect(row.usage?.total).toBe(65)
  expect(row.usage?.usd).toBe(0.42)
  expect(row.usage?.allocated).toBe(true)
})

test('session cache reuses unchanged files, reloads changes and evicts removals', async () => {
  const reader = createUsageReader()
  let mtime = 1, reads = 0, files = ['/home/u/.claude/projects/-w/a.jsonl']
  const io = { files: async () => files, mtime: async () => mtime, read: async () => { reads++; return JSON.stringify({ cwd: '/w', message: { usage: { input_tokens: reads }, model: 'unknown' } }) } }
  expect((await reader(io, ['/w']))[0].input).toBe(1)
  await reader(io, ['/w']); expect(reads).toBe(1)
  mtime++; expect((await reader(io, ['/w']))[0].input).toBe(2)
  files = []; expect(await reader(io, ['/w'])).toEqual([])
  files = ['/home/u/.claude/projects/-w/a.jsonl']; await reader(io, ['/w']); expect(reads).toBe(3)
})

test('Codex uses last cumulative count with cached input counted once', () => {
  const rows = parseSession([
    { type: 'session_meta', payload: { cwd: '/w' } },
    { type: 'turn_context', payload: { model: 'unknown' } },
    ...[10, 20].map(input_tokens => ({ payload: { type: 'token_count', info: { total_token_usage: { input_tokens, cached_input_tokens: 5, output_tokens: 7, reasoning_output_tokens: 3 } } } })),
  ].map(JSON.stringify).join('\n') + '\n{', 'codex')
  expect(rows.length).toBe(1)
  const u = aggregateUsage(rows, { '/w': ['ZII-1', 'ZII-2'] })['ZII-1']
  expect(u.total).toBe(13.5); expect(u.known).toBe(false); expect(u.usd).toBe(0); expect(u.allocated).toBe(true)
})

test('prices use the repository table and unknown models stay unpriced', () => {
  const u = aggregateUsage([{ cwd: '/w', model: 'claude-opus-5-5', input: 1e6, output: 0, cacheRead: 0, cacheWrite: 0 }], { '/w': 'ZII-1' })['ZII-1']
  expect(u.usd).toBe(4)
  expect(u.price_version).toBe('2026-10-06-standard-v2')
})

test('recorded Claude and Codex accounting agrees with the driver price table', () => {
  const rows = [...parseSession(recordedUsage.claude, 'claude'), ...parseSession(recordedUsage.codex, 'codex')]
  const u = aggregateUsage(rows, { '/work/unit': 'ZII-1', '/home/user/orca/workspaces/orca-factory/ZII-887-executor': 'ZII-2' })
  expect(u['ZII-1'].input).toBe(4)
  expect(u['ZII-1'].cache_write).toBe(52773)
  expect(Math.round(u['ZII-1'].usd * 1e7)).toBe(3491479)
  expect(u['ZII-2'].input).toBe(58412)
  expect(u['ZII-2'].cache_read).toBe(671488)
  expect(u['ZII-2'].reasoning).toBe(2219)
  expect(Math.round(u['ZII-2'].usd * 1e7)).toBe(2615328)
})

test('ledger with an unknown model does not present zero as a known price', () => {
  const u = ledgerUsage([{ body: 'ledger ' + JSON.stringify({ ticket: 'ZII-1', usage: { input: 10, output: 1, cost_usd: 0, price_version: 'recorded', by_model: { 'mystery-model': { input: 10, output: 1 } } } }) }])['ZII-1'].usage
  expect(u?.known).toBe(false)
  expect(u?.partial).toBe(true)
})
