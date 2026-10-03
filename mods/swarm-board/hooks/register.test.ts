import { test, expect } from 'claude-code/testing'

const mount = ($: any, surface: 'terminal' | 'desktop') =>
  $.ui.mount({ plugin: 'swarm-board', surface, component: 'Pane', requestId: 'swarm-board', props: { title: 'Swarm' } })

test('focal header draws', async $ => {
  const ui = await mount($, 'terminal')
  expect(await ui.find({ type: 'Text', text: /in flight/ })).toBeDefined()
  await ui.unmount()
})
test('lanes draw by default', async $ => {
  const ui = await mount($, 'terminal')
  expect(await ui.find({ type: 'Text', text: /实现中/ })).toBeDefined()
  expect(await ui.find({ type: 'Button', text: /lanes/ })).toBeDefined()
  await ui.unmount()
})
test('view toggle shows agents card', async $ => {
  const ui = await mount($, 'terminal')
  await ui.press({ key: 'view-swarm' })
  expect(await ui.find({ type: 'Text', text: /Agents/ })).toBeDefined()
  await ui.unmount()
})
test('queue disclosure toggles in swarm view', async $ => {
  const ui = await mount($, 'terminal')
  await ui.press({ key: 'view-swarm' })
  await ui.press({ key: 'toggle-queue' })
  expect(await ui.find({ type: 'Button', text: /queued/ })).toBeDefined()
  await ui.unmount()
})
