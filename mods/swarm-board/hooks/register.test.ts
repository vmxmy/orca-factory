import { test, expect, mock } from 'claude-code/testing'

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
const automationFixture = async ($: any, on: any, installed = true, failure = '') => {
  let enabled = true
  let refreshed: () => void = () => {}
  const ready = new Promise<void>(resolve => { refreshed = resolve })
  mock.clock(on)
  on('ui.open', () => ({ value: undefined }))
  on('ui.status', () => { refreshed(); return { value: undefined } })
  on('fs.read', () => ({ value: 'AGENT_GH_REPO=owner/test-repo' }))
  on('process.run', (_: any, e: any) => {
    const argv = e.argv
    if (argv[0] === 'orca' && argv[1] === 'automations' && argv[2] === 'edit') {
      expect(argv[3]).toBe('auto-1')
      if (failure) return { value: { stdout: '', stderr: failure, exitCode: 1 } }
      expect(argv[4]).toBe(enabled ? '--disabled' : '--enabled')
      enabled = !enabled
    }
    return { value: { stdout: JSON.stringify({ ok: true, result: { automations: installed ? [{ id: 'auto-1', name: 'agent-linear-scan (test-repo)', enabled }] : [] } }), stderr: '', exitCode: 0 } }
  })
  await $.command.run({ command: 'swarm', args: '' })
  await ready
}

test('automation button remains available after toggle', async ($, on) => {
  await automationFixture($, on)
  const ui = await mount($, 'terminal')
  expect(await ui.find({ type: 'Button', text: /auto/ })).toBeDefined()
  expect(await ui.find({ type: 'Button', text: /● auto on/ })).toBeDefined()
  await ui.press({ key: 'toggle-auto' })
  await ui.redraw()
  expect(await ui.find({ type: 'Button', text: /auto/ })).toBeDefined()
  expect(await ui.find({ type: 'Button', text: /○ auto off/ })).toBeDefined()
  await ui.press({ key: 'toggle-auto' })
  await ui.redraw()
  expect(await ui.find({ type: 'Button', text: /● auto on/ })).toBeDefined()
  await ui.unmount()
})

test('failed automation edit preserves state and shows the reason', async ($, on) => {
  await automationFixture($, on, true, 'Automation unavailable')
  const ui = await mount($, 'terminal')
  await ui.press({ key: 'toggle-auto' })
  await ui.redraw()
  expect(await ui.find({ type: 'Button', text: /● auto on/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /Automation unavailable/ })).toBeDefined()
  await ui.unmount()
})

test('missing automation shows an uninstalled notice without a toggle', async ($, on) => {
  await automationFixture($, on, false)
  const ui = await mount($, 'terminal')
  expect(await ui.find({ type: 'Text', text: /auto: 未安装/ })).toBeDefined()
  expect(await ui.find({ type: 'Button', text: /auto/ })).toBeUndefined()
  await ui.unmount()
})

const failedFixture = async ($: any, on: any) => {
  let comments: unknown = { result: { comments: [
    { body: '转人: 旧卡点\n需要你: 旧动作', createdAt: '2026-10-01T00:00:00Z' },
    { body: '转人: 缺少凭据\n证据: gate red\n需要你: 配置凭据后重试', createdAt: '2026-10-02T00:00:00Z' },
    { body: '普通进度评论', createdAt: '2026-10-03T00:00:00Z' },
  ] } }
  let refreshed = () => {}
  mock.clock(on)
  on('ui.open', () => ({ value: undefined }))
  on('ui.status', () => { refreshed(); return { value: undefined } })
  on('fs.read', () => ({ value: 'AGENT_GH_REPO=owner/test-repo' }))
  on('process.run', (_: any, e: any) => {
    let result: unknown = { ok: true, result: {} }
    if (e.argv[1] === 'linear' && e.argv[2] === 'list-issues') result = { ok: true, result: { issues: [
      { identifier: 'ZII-807', title: '需要处理的票', state: { type: 'started' }, labels: [{ name: 'auto-failed' }, { name: 'auto-impl' }] },
    ] } }
    if (e.argv[1] === 'linear' && e.argv[2] === 'issue') result = comments
    return { value: { stdout: JSON.stringify(result), stderr: '', exitCode: 0 } }
  })
  const refresh = async () => {
    const ready = new Promise<void>(resolve => { refreshed = resolve })
    await $.command.run({ command: 'swarm', args: '' })
    await ready
  }
  await refresh()
  return { refresh, setComments: (value: unknown) => { comments = value } }
}

test('needs-you cards show the latest handoff and intake guidance in both views', async ($, on) => {
  await failedFixture($, on)
  const ui = await mount($, 'terminal')
  for (const key of ['view-task', 'view-swarm']) {
    await ui.press({ key })
    expect(await ui.find({ type: 'Text', text: /卡点：缺少凭据/ })).toBeDefined()
    expect(await ui.find({ type: 'Text', text: /需要你：配置凭据后重试/ })).toBeDefined()
    expect(await ui.find({ type: 'Text', text: /处理好后，开 intake 会话：按 scripts\/agent-gates\/roles\/intake.md 工作，处理 ZII-807/ })).toBeDefined()
  }
  await ui.unmount()
})
