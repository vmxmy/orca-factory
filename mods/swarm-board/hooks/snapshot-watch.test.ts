import { test, expect } from 'claude-code/testing'
import { shouldReload } from './snapshot-watch'

test('mtime changes reload the snapshot', () => {
  expect(shouldReload({ mtimeMs: 1, size: 10 }, { mtimeMs: 2, size: 10 })).toBe(true)
})

test('size changes reload the snapshot', () => {
  expect(shouldReload({ mtimeMs: 1, size: 10 }, { mtimeMs: 1, size: 20 })).toBe(true)
})

test('unchanged mtime and size do not reload the snapshot', () => {
  expect(shouldReload({ mtimeMs: 1, size: 10 }, { mtimeMs: 1, size: 10 })).toBe(false)
})

test('failed stat does not reload the snapshot', () => {
  expect(shouldReload({ mtimeMs: 1, size: 10 }, null)).toBe(false)
})
