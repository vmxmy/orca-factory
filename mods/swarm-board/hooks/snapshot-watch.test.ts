import { test, expect } from 'claude-code/testing'
import { canStartReload, canStartTick, shouldReload } from './snapshot-watch'

const idle = { paneOpen: true, hasFile: true, reloading: false, ticking: false }

test('snapshot reload starts only when the pane is open, a file is configured and nothing else is running', async () => {
  expect(canStartReload(idle)).toBe(true)
  expect(canStartReload({ ...idle, paneOpen: false })).toBe(false)
  expect(canStartReload({ ...idle, hasFile: false })).toBe(false)
  expect(canStartReload({ ...idle, reloading: true })).toBe(false)
})

// A tick reads the board, spends several awaits on live calls, then overwrites the board.
// A reload landing in the middle would be overwritten with older data while the stamp
// already moved on, so the board would stay stale until the next scheduled refresh.
test('snapshot reload never overlaps a running tick, and a tick never overlaps a reload', async () => {
  expect(canStartReload({ ...idle, ticking: true })).toBe(false)
  expect(canStartTick({ reloading: true, ticking: false })).toBe(false)
  expect(canStartTick({ reloading: false, ticking: true })).toBe(false)
  expect(canStartTick({ reloading: false, ticking: false })).toBe(true)
})

test('a changed stamp seen after a tick finished still triggers a reload', async () => {
  const atTickStart = { mtimeMs: 1, size: 10 }
  const afterTick = { mtimeMs: 2, size: 10 }
  expect(shouldReload(atTickStart, afterTick)).toBe(true)
  expect(shouldReload(atTickStart, atTickStart)).toBe(false)
  expect(shouldReload(null, afterTick)).toBe(false)
})
