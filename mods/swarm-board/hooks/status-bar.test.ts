import { test, expect } from 'claude-code/testing'
import { statusBarRows } from './status-bar'

const text = (rows: ReturnType<typeof statusBarRows>) => rows.flat().map(segment => segment.text).join(' ')

 test('footer distinguishes an absent snapshot from a throttled Linear fallback', () => {
  const base = { inFlight: 0, max: 10, failed: 0, queue: 0, prs: 0, doneToday: 0, driver: 'none' as const }
  expect(text(statusBarRows(base, 100))).toContain('快照缺失')
  expect(text(statusBarRows(base, 100))).not.toContain('读取已节流')
  expect(text(statusBarRows({ ...base, linearThrottled: true }, 100))).toContain('快照缺失，Linear 读取已节流')
})
