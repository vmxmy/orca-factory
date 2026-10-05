import { test, expect, mock } from 'claude-code/testing'

import type { Ticket } from '../types'
import { laneStage } from './register'

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
test('default lane view shows 实现中 head with its emoji', async $ => {
  const ui = await mount($, 'terminal')
  expect(await ui.find({ type: 'Text', text: /🔨 实现中/ })).toBeDefined()
  await ui.unmount()
})
test('wide lane view shows every lane head with its emoji', async $ => {
  const ui = await $.ui.mount({
    plugin: 'swarm-board', surface: 'terminal', component: 'Pane', requestId: 'swarm-board',
    props: { title: 'Swarm', bodyColumns: 160 },
  })
  expect(await ui.find({ type: 'Text', text: /🙋 需要人/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /✅ 今日完成/ })).toBeDefined()
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
    const automations = installed
      ? [{ id: 'auto-1', name: 'agent-linear-scan (test-repo)', enabled }]
      : [{ id: 'other-1', name: 'unrelated-automation', enabled: true }]
    return { value: { stdout: JSON.stringify({ ok: true, result: { automations } }), stderr: '', exitCode: 0 } }
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

test('refresh countdown shows after the first tick', async ($, on) => {
  await automationFixture($, on)
  const ui = await mount($, 'terminal')
  expect(await ui.find({ type: 'Text', text: /下次刷新/ })).toBeDefined()
  await ui.unmount()
})

test('manual refresh button stays available after press', async ($, on) => {
  await automationFixture($, on)
  const ui = await mount($, 'terminal')
  expect(await ui.find({ type: 'Button', text: /刷新/ })).toBeDefined()
  await ui.press({ key: 'refresh' })
  await ui.redraw()
  expect(await ui.find({ type: 'Button', text: /刷新/ })).toBeDefined()
  await ui.unmount()
})

test('ticket with no worker lives in the implement lane', async () => {
  const t: Ticket = { id: 'ZII-999', title: '', state: 'started', labels: [], priority: 0, updatedAt: '', claimedAt: '' }
  expect(laneStage(t, [])).toBe(1)
})

const failedFixture = async ($: any, on: any, count = 1) => {
  let commentExitCode = 0
  const reads: string[] = []
  let comments: unknown = { result: { comments: [
    { body: '转人: 旧卡点\n需要你: 旧动作', createdAt: '2026-10-01T00:00:00Z' },
    { body: '转人: 缺少凭据\n证据: gate red\n需要你: 配置凭据后重试', createdAt: '2026-10-02T00:00:00Z' },
    { body: '已领取 (ZII-' + '807)', createdAt: '2026-10-02T00:30:00Z' },
    { body: '普通进度评论', createdAt: '2026-10-03T00:00:00Z' },
  ] } }
  let refreshed = () => {}
  mock.clock(on)
  on('ui.open', () => ({ value: undefined }))
  on('ui.status', () => { refreshed(); return { value: undefined } })
  on('fs.read', () => ({ value: 'AGENT_GH_REPO=owner/test-repo\nAGENT_LINEAR_TEAM=ZII\nAGENT_LINEAR_PROJECT=Factory' }))
  on('process.run', (_: any, e: any) => {
    let result: unknown = { ok: true, result: {} }
    if (e.argv[1] === 'linear' && e.argv[2] === 'list-issues') {
      result = { ok: true, result: { issues: Array.from({ length: count }, (_, i) => (
        { identifier: 'ZII-' + (807 + i), title: '需要处理的票', state: { type: 'started' }, labels: [{ name: 'auto-failed' }, { name: 'auto-impl' }], updatedAt: '2026-10-01T00:00:00Z' }
      )) } }
    }
    if (e.argv[1] === 'linear' && e.argv[2] === 'issue') {
      reads.push(e.argv[3])
      expect(e.argv.slice(4)).toEqual(['--comments', '--json'])
      return { value: { stdout: JSON.stringify(comments), stderr: commentExitCode ? 'Linear read failed' : '', exitCode: commentExitCode } }
    }
    return { value: { stdout: JSON.stringify(result), stderr: '', exitCode: 0 } }
  })
  const refresh = async () => {
    const ready = new Promise<void>(resolve => { refreshed = resolve })
    await $.command.run({ command: 'swarm', args: '' })
    await ready
  }
  await refresh()
  return { refresh, reads: () => reads.slice(), setComments: (value: unknown, exitCode = 0) => { comments = value; commentExitCode = exitCode } }
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

test('empty comment reads keep the last successful handoff in both views', async ($, on) => {
  const fixture = await failedFixture($, on)
  const ui = await mount($, 'terminal')
  expect(await ui.find({ type: 'Text', text: /卡点：缺少凭据/ })).toBeDefined()
  fixture.setComments({ result: { comments: [] } })
  await fixture.refresh()
  await ui.redraw()
  for (const key of ['view-task', 'view-swarm']) {
    await ui.press({ key })
    expect(await ui.find({ type: 'Text', text: /卡点：缺少凭据/ })).toBeDefined()
    expect(await ui.find({ type: 'Text', text: /需要你：配置凭据后重试/ })).toBeDefined()
  }
  await ui.unmount()
})

test('failed comment commands preserve the last successful handoff', async ($, on) => {
  const fixture = await failedFixture($, on)
  const ui = await mount($, 'terminal')
  fixture.setComments({ result: { comments: [
    { body: '转人: 不完整响应\n需要你: 不应显示', createdAt: '2026-10-04T00:00:00Z' },
  ] } }, 1)
  await fixture.refresh()
  await ui.redraw()
  expect(await ui.find({ type: 'Text', text: /卡点：缺少凭据/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /需要你：配置凭据后重试/ })).toBeDefined()
  await ui.unmount()
})

test('incomplete handoff comments fall back to their first line in both views', async ($, on) => {
  const fixture = await failedFixture($, on)
  fixture.setComments({ result: { comments: [
    { body: '转人: 等待人工确认\n证据: gate red', createdAt: '2026-10-04T00:00:00Z' },
  ] } })
  await fixture.refresh()
  const ui = await mount($, 'terminal')
  for (const key of ['view-task', 'view-swarm']) {
    await ui.press({ key })
    expect(await ui.find({ type: 'Text', text: /^转人: 等待人工确认$/ })).toBeDefined()
  }
  await ui.unmount()
})

test('refresh countdown stays within one refresh window', async ($, on) => {
  await automationFixture($, on)
  const ui = await mount($, 'terminal')
  // Guards against the old bug that printed epoch seconds (10+ digits).
  expect(await ui.find({ type: 'Text', text: /下次刷新 ([1-9]?\d|60)s/ })).toBeDefined()
  await ui.unmount()
})

test('linear rate-limit response keeps the last-good ticket list and warns in the header', async ($, on) => {
  let linearOk = true
  let refreshed: () => void = () => {}
  mock.clock(on)
  on('ui.open', () => ({ value: undefined }))
  on('ui.status', () => { refreshed(); return { value: undefined } })
  on('fs.read', () => ({ value: 'AGENT_GH_REPO=owner/test-repo\nAGENT_LINEAR_TEAM=ZII\nAGENT_LINEAR_PROJECT=Factory' }))
  on('process.run', (_: any, e: any) => {
    if (e.argv[1] === 'linear' && e.argv[2] === 'list-issues') {
      if (!linearOk) return { value: { stdout: JSON.stringify({ ok: false, error: 'Rate limit exceeded' }), stderr: '', exitCode: 0 } }
      return { value: { stdout: JSON.stringify({ ok: true, result: { issues: [
        { identifier: 'ZII-900', title: '限流前的在途', state: { type: 'started' }, labels: [{ name: 'auto-impl' }], updatedAt: '2026-10-01T00:00:00Z' },
      ] } }), stderr: '', exitCode: 0 } }
    }
    return { value: { stdout: JSON.stringify({ ok: true, result: {} }), stderr: '', exitCode: 0 } }
  })
  const refresh = async () => { const ready = new Promise<void>(resolve => { refreshed = resolve }); await $.command.run({ command: 'swarm', args: '' }); await ready }
  await refresh()
  const ui = await $.ui.mount({ plugin: 'swarm-board', surface: 'terminal', component: 'Pane', requestId: 'swarm-board', props: { title: 'Swarm', bodyColumns: 160 } })
  expect(await ui.find({ type: 'Text', text: /ZII-900/ })).toBeDefined()
  linearOk = false
  await refresh()
  await ui.redraw()
  expect(await ui.find({ type: 'Text', text: /ZII-900/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /Linear 限流，沿用旧数据/ })).toBeDefined()
  await ui.unmount()
})

test('ticket in both failed and inflight shows only in the 需要人 lane', async ($, on) => {
  await failedFixture($, on)
  const ui = await $.ui.mount({ plugin: 'swarm-board', surface: 'terminal', component: 'Pane', requestId: 'swarm-board', props: { title: 'Swarm', bodyColumns: 160 } })
  expect(await ui.find({ type: 'Text', text: /🔨 实现中 \(0\)/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /🙋 需要人 \(1\)/ })).toBeDefined()
  await ui.unmount()
})

test('handoff parsing stops after three failed tickets while inflight claimedAt still reads them', async ($, on) => {
  // Five tickets labeled auto-failed+auto-impl: all are in-flight AND failed.
  // Handoff parsing is bounded to the first three failed tickets; the in-flight loop
  // still reads comments for every ticket (for claimedAt), sharing the cache so
  // overlapping failed+inflight tickets are fetched at most once.
  const fixture = await failedFixture($, on, 5)
  const reads = fixture.reads()
  expect(reads).toHaveLength(5)
  expect(new Set(reads).size).toBe(5)
  const ui = await mount($, 'terminal')
  expect(await ui.find({ type: 'Text', text: /处理 ZII-811/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /卡点：缺少凭据/ })).toBeDefined()
  await ui.unmount()
})
