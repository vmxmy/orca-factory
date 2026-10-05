import { test, expect } from 'claude-code/testing'
import { WATCH_POLL_MS, shouldReload } from './snapshot-watch'

test('snapshot stat comparison and polling interval', () => {
  expect(WATCH_POLL_MS).toBe(5000)
  expect(shouldReload({ mtimeMs: 1, size: 1 }, { mtimeMs: 2, size: 1 })).toBe(true)
  expect(shouldReload({ mtimeMs: 1, size: 1 }, { mtimeMs: 1, size: 2 })).toBe(true)
  expect(shouldReload({ mtimeMs: 1, size: 1 }, { mtimeMs: 1, size: 1 })).toBe(false)
  expect(shouldReload({ mtimeMs: 1, size: 1 }, null)).toBe(false)
  expect(shouldReload(null, null)).toBe(false)
  expect(shouldReload(null, { mtimeMs: 1, size: 1 })).toBe(true)
})
