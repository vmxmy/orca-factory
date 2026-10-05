import { test, expect, mock } from 'claude-code/testing'

import type { Ticket } from '../types'
import {
  __resetSwarmBoardCachesForTests,
  RESET_LINEAR_THROTTLE_COMMAND,
  SIMULATE_CLOSE_COMMAND,
  laneStage,
  ticketPhase,
} from './register'

const mount = ($: any, surface: 'terminal' | 'desktop') =>
  $.ui.mount({ plugin: 'swarm-board', surface, component: 'Pane', requestId: 'swarm-board', props: { title: 'Swarm' } })

test('focal header draws', async $ => {
  const ui = await mount($, 'terminal')
  expect(await ui.find({ type: 'Text', text: /在做/ })).toBeDefined()
  await ui.unmount()
})
test('lanes draw by default', async $ => {
  const ui = await mount($, 'terminal')
  expect(await ui.find({ type: 'Text', text: /实现中/ })).toBeDefined()
  expect(await ui.find({ type: 'Button', text: /泳道/ })).toBeDefined()
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
  expect(await ui.find({ type: 'Button', text: /待领/ })).toBeDefined()
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
  expect(await ui.find({ type: 'Button', text: /定时/ })).toBeDefined()
  expect(await ui.find({ type: 'Button', text: /● 定时 开/ })).toBeDefined()
  await ui.press({ key: 'toggle-auto' })
  await ui.redraw()
  expect(await ui.find({ type: 'Button', text: /定时/ })).toBeDefined()
  expect(await ui.find({ type: 'Button', text: /○ 定时 关/ })).toBeDefined()
  await ui.press({ key: 'toggle-auto' })
  await ui.redraw()
  expect(await ui.find({ type: 'Button', text: /● 定时 开/ })).toBeDefined()
  await ui.unmount()
})

test('failed automation edit preserves state and shows the reason', async ($, on) => {
  await automationFixture($, on, true, 'Automation unavailable')
  const ui = await mount($, 'terminal')
  await ui.press({ key: 'toggle-auto' })
  await ui.redraw()
  expect(await ui.find({ type: 'Button', text: /● 定时 开/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /Automation unavailable/ })).toBeDefined()
  await ui.unmount()
})

test('missing automation shows an uninstalled notice without a toggle', async ($, on) => {
  await automationFixture($, on, false)
  const ui = await mount($, 'terminal')
  expect(await ui.find({ type: 'Text', text: /定时：未安装/ })).toBeDefined()
  expect(await ui.find({ type: 'Button', text: /定时/ })).toBeUndefined()
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

test('header shows the mod version', async ($, on) => {
  await automationFixture($, on)
  const ui = await mount($, 'terminal')
  expect(await ui.find({ type: 'Text', text: /v\d+\.\d+\.\d+/ })).toBeDefined()
  await ui.unmount()
})

test('header warns when a newer mod version is already installed', async ($, on) => {
  let refreshed: () => void = () => {}
  mock.clock(on)
  on('ui.open', () => ({ value: undefined }))
  on('ui.status', () => { refreshed(); return { value: undefined } })
  on('fs.read', (_: unknown, e: { path: string }) => {
    if (e.path.endsWith('/installed_plugins.json')) {
      return { value: JSON.stringify({ plugins: { 'swarm-board@orca-factory': [{ version: '99.0.0' }] } }) }
    }
    return { value: 'AGENT_GH_REPO=owner/test-repo\nAGENT_LINEAR_TEAM=ZII\nAGENT_LINEAR_PROJECT=Factory' }
  })
  on('process.run', () => ({ value: { stdout: JSON.stringify({ ok: true, result: {} }), stderr: '', exitCode: 0 } }))
  const ready = new Promise<void>(resolve => { refreshed = resolve })
  await $.command.run({ command: 'swarm', args: '' })
  await ready
  const ui = await mount($, 'terminal')
  expect(await ui.find({ type: 'Text', text: /→ 已安装 v99\.0\.0，请重启/ })).toBeDefined()
  await ui.unmount()
})

test('ticket with no worker lives in the implement lane', async () => {
  const t: Ticket = { id: 'ZII-999', title: '', state: 'started', labels: [], priority: 0, updatedAt: '', claimedAt: '' }
  expect(laneStage(t, [])).toBe(1)
})

const failedFixture = async ($: any, on: any, count = 1) => {
  __resetSwarmBoardCachesForTests()
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
    // Clear the plugin VM's 5-min Linear throttle so each refresh exercises its intended
    // live-Linear path (test imports and the VM have separate module instances).
    await $.command.run({ command: RESET_LINEAR_THROTTLE_COMMAND, args: '' })
    const ready = new Promise<void>(resolve => { refreshed = resolve })
    await $.command.run({ command: 'swarm', args: '' })
    await ready
  }
  await refresh()
  return { refresh, reads: () => reads.slice(), setComments: (value: unknown, exitCode = 0) => { comments = value; commentExitCode = exitCode } }
}

test('health badge is green with no auto-failed tickets', async ($, on) => {
  await failedFixture($, on, 0)
  const ui = await mount($, 'terminal')
  expect(await ui.find({ type: 'Text', text: /🟢 需要人 0/ })).toBeDefined()
  await ui.unmount()
})

test('health badge is red with three auto-failed tickets', async ($, on) => {
  await failedFixture($, on, 3)
  const ui = await mount($, 'terminal')
  expect(await ui.find({ type: 'Text', text: /🔴 需要人 3/ })).toBeDefined()
  await ui.unmount()
})

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
  const refresh = async () => { await $.command.run({ command: RESET_LINEAR_THROTTLE_COMMAND, args: '' }); const ready = new Promise<void>(resolve => { refreshed = resolve }); await $.command.run({ command: 'swarm', args: '' }); await ready }
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

const queueFixture = async ($: any, on: any, queueCount: number, precheck: { exitCode: number; runNumber: number; stdout: string; stderr: string }) => {
  let refreshed: () => void = () => {}
  const ready = new Promise<void>(resolve => { refreshed = resolve })
  mock.clock(on)
  on('ui.open', () => ({ value: undefined }))
  on('ui.status', () => { refreshed(); return { value: undefined } })
  on('fs.read', () => ({ value: 'AGENT_GH_REPO=owner/test-repo\nAGENT_LINEAR_TEAM=ZII\nAGENT_LINEAR_PROJECT=Factory\nAGENT_MAX_IN_FLIGHT=10' }))
  on('process.run', (_: any, e: any) => {
    const argv = e.argv
    if (argv[1] === 'linear' && argv[2] === 'list-issues') {
      const issues = Array.from({ length: queueCount }, (_, i) => (
        { identifier: 'ZII-' + (i + 1), title: '待领票 ' + (i + 1), state: { type: 'unstarted' }, labels: [{ name: 'ready-for-agent' }], updatedAt: '2026-10-01T00:00:00Z', priority: 2 }
      ))
      return { value: { stdout: JSON.stringify({ ok: true, result: { issues } }), stderr: '', exitCode: 0 } }
    }
    if (argv[1] === 'automations' && argv[2] === 'list') {
      return { value: { stdout: JSON.stringify({ ok: true, result: { automations: [
        { id: 'a1', name: 'agent-linear-scan (test-repo)', enabled: true, nextRunAt: Date.now() + 90_000 },
      ] } }), stderr: '', exitCode: 0 } }
    }
    if (argv[1] === 'automations' && argv[2] === 'runs') {
      return { value: { stdout: JSON.stringify({ ok: true, result: { runs: [
        { runNumber: precheck.runNumber, status: 'succeeded', scheduledFor: new Date().toISOString(),
          precheckResult: { exitCode: precheck.exitCode, timedOut: false, error: '', durationMs: 1000, stdout: precheck.stdout, stderr: precheck.stderr } },
      ] } }), stderr: '', exitCode: 0 } }
    }
    return { value: { stdout: JSON.stringify({ ok: true, result: {} }), stderr: '', exitCode: 0 } }
  })
  await $.command.run({ command: 'swarm', args: '' })
  await ready
}

test('queue warning fires when precheck declares 0 claimable', async ($, on) => {
  await queueFixture($, on, 2, { exitCode: 1, runNumber: 7,
    stdout: 'claimable= in_flight=0 max=10\nstale=\nhealth=live=0 max=10 released=0 escalated=0 failed_backlog=0',
    stderr: 'linear_calls=5' })
  const ui = await mount($, 'terminal')
  expect(await ui.find({ type: 'Text', text: /待领 2 张。precheck 判定可领 0 张。/ })).toBeDefined()
  await ui.unmount()
})

test('queue status shows claimable count and tickets marked chosen', async ($, on) => {
  await queueFixture($, on, 2, { exitCode: 0, runNumber: 8,
    stdout: 'claimable=ZII-1,ZII-2 in_flight=0 max=10\nstale=\nhealth=live=2 max=10 released=0 escalated=0 failed_backlog=0',
    stderr: '' })
  const ui = await mount($, 'terminal')
  expect(await ui.find({ type: 'Text', text: /可领 2 张/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /已入选/ })).toBeDefined()
  await ui.unmount()
})

test('queue card shows the skip reason when precheck lists one', async ($, on) => {
  await queueFixture($, on, 3, { exitCode: 0, runNumber: 9,
    stdout: 'claimable=ZII-1,ZII-2 in_flight=0 max=10\nstale=\nhealth=live=2 max=10 released=0 escalated=0 failed_backlog=0',
    stderr: 'skip ZII-3: scope overlaps ZII-9\nlinear_calls=3' })
  const ui = await mount($, 'terminal')
  expect(await ui.find({ type: 'Text', text: /scope overlaps ZII-9/ })).toBeDefined()
  await ui.unmount()
})

test('queue status becomes uncertain when precheck stdout is empty on a non-zero exit', async ($, on) => {
  await queueFixture($, on, 2, { exitCode: 3, runNumber: 184,
    stdout: '', stderr: '不确定: linear rate limited\nlinear_calls=1' })
  const ui = await mount($, 'terminal')
  expect(await ui.find({ type: 'Text', text: /读取失败/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /linear rate limited/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /可领 0 张/ })).toBeUndefined()
  await ui.unmount()
})

test('queue card falls back to the uncertain explanation when precheck could not read data', async ($, on) => {
  await queueFixture($, on, 1, { exitCode: 3, runNumber: 184,
    stdout: '', stderr: '不确定: linear rate limited\nlinear_calls=1' })
  const ui = await mount($, 'terminal')
  expect(await ui.find({ type: 'Text', text: /读取失败：linear rate limited/ })).toBeDefined()
  await ui.unmount()
})

test('uncertain fires on exit 3 even when stdout carries prune output', async ($, on) => {
  await queueFixture($, on, 1, { exitCode: 3, runNumber: 195,
    stdout: '已删除分支 feat/x（原为 abc）。', stderr: '不确定: linear rate limited\nlinear_calls=2' })
  const ui = await mount($, 'terminal')
  expect(await ui.find({ type: 'Text', text: /读取失败/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /linear rate limited/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /⚠ 读取失败：/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /未选中/ })).toBeUndefined()
  await ui.unmount()
})

test('exit 1 with claimable line below prune output stays a normal no-ticket result', async ($, on) => {
  await queueFixture($, on, 1, { exitCode: 1, runNumber: 196,
    stdout: '已删除分支 feat/x。\nclaimable= in_flight=0 max=10', stderr: '' })
  const ui = await mount($, 'terminal')
  expect(await ui.find({ type: 'Text', text: /读取失败/ })).toBeUndefined()
  expect(await ui.find({ type: 'Text', text: /可领 0 张/ })).toBeDefined()
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

const phaseTicket: Ticket = { id: 'ZII-848', title: '阶段卡片', state: 'started', labels: ['auto-impl'], priority: 0, updatedAt: '', claimedAt: '' }
test('claimed ticket without a run is splitting', async () => {
  expect(ticketPhase(phaseTicket, { run: null, tasks: [] })).toBe('📋 拆分')
})
test('dispatched task is implementing', async () => {
  expect(ticketPhase(phaseTicket, { run: { id: 'run-1' }, tasks: [{ status: 'dispatched' }] })).toBe('🔨 实现')
  expect(ticketPhase(phaseTicket, { run: { id: 'run-1' }, tasks: [{ status: 'dispatched' }], gate: { state: 'red', sec: 0, steps: [] } })).toBe('🔨 实现')
  expect(ticketPhase(phaseTicket, { tasks: [{ status: 'dispatched' }], gate: { state: 'green', sec: 0, steps: [] }, lane: 2 })).toBe('🔨 实现')
  expect(ticketPhase(phaseTicket, { tasks: [{ status: 'pending' }], gate: { state: 'green', sec: 0, steps: [] }, lane: 2 })).toBe('📤 派工')
  expect(ticketPhase(phaseTicket, { tasks: [{ status: 'ready' }], gate: { state: 'red', sec: 0, steps: [] } })).toBe('📤 派工')
  expect(ticketPhase(phaseTicket, { tasks: [{ status: 'completed' }], gate: { state: 'green', sec: 0, steps: [] }, lane: 1 })).toBe('📬 待收尾')
  expect(ticketPhase(phaseTicket, { tasks: [{ status: 'completed' }], gate: { state: 'green', sec: 0, steps: [] }, lane: 2 })).toBe('🔍 审查（推断）')
  expect(ticketPhase(phaseTicket, { tasks: [{ status: 'completed' }], gate: { state: 'running', sec: 0, steps: [] } })).toBe('🧪 gate')
})
test('driver phase on the worktree card wins over task inference', async () => {
  const done = [{ status: 'completed' }]
  expect(ticketPhase(phaseTicket, { tasks: done, driver: 'driver: worker 运行中' })).toBe('🔨 实现')
  expect(ticketPhase(phaseTicket, { tasks: done, driver: 'driver: 写 correction' })).toBe('🔨 实现')
  expect(ticketPhase(phaseTicket, { tasks: done, driver: 'driver: 分支检查' })).toBe('🧪 gate')
  expect(ticketPhase(phaseTicket, { tasks: done, driver: 'driver: gate' })).toBe('🧪 gate')
  expect(ticketPhase(phaseTicket, { tasks: done, driver: 'driver: 审查' })).toBe('🔍 审查（推断）')
  expect(ticketPhase(phaseTicket, { tasks: done, driver: 'driver: 排队合并' })).toBe('🚦 CI/合并')
  expect(ticketPhase(phaseTicket, { tasks: done, driver: '人工备注' })).toBe('📬 待收尾')
})
test('completed workers without gate or PR await wrap-up', async () => {
  expect(ticketPhase(phaseTicket, { run: { id: 'run-1' }, tasks: [{ status: 'completed' }] })).toBe('📬 待收尾')
  expect(ticketPhase(phaseTicket, { run: { id: 'run-1' }, tasks: [{ status: 'failed' }, { status: 'completed' }], previous: '🔨 实现' })).toBe('📬 待收尾')
  expect(ticketPhase(phaseTicket, { run: { id: 'run-1' }, tasks: [{ status: 'failed' }], previous: '🔨 实现' })).toBe('🔨 实现')
  expect(ticketPhase(phaseTicket, { run: { id: 'run-1' }, tasks: [{ status: 'failed' }] })).toBe('📤 派工')
})
const linearUsageFixture = async (
  $: any,
  on: any,
  counter: { content?: string; missing?: boolean },
) => {
  __resetSwarmBoardCachesForTests()
  let ledgerCalls = 0
  let linearListCalls = 0
  let refreshed: () => void = () => {}
  mock.clock(on)
  on('ui.open', () => ({ value: undefined }))
  on('ui.status', () => { refreshed(); return { value: undefined } })
  on('fs.read', (_: unknown, e: { path: string }) => {
    if (e.path.endsWith('/orca-factory-linear-calls')) {
      if (counter.missing) throw new Error('ENOENT')
      return { value: counter.content ?? '' }
    }
    return { value: 'AGENT_GH_REPO=owner/test-repo\nAGENT_LINEAR_TEAM=ZII\nAGENT_LINEAR_PROJECT=Factory' }
  })
  on('process.run', (_: unknown, e: { argv: string[] }) => {
    const argv = e.argv
    if (argv[0] === 'orca' && argv[1] === 'linear' && argv[2] === 'list-issues') linearListCalls++
    if (argv[0] === 'bash' && argv[2] === 'ledger' && argv[3] === 'stats') {
      ledgerCalls++
      return { value: { stdout: 'ledger stat line\n', stderr: '', exitCode: 0 } }
    }
    return { value: { stdout: JSON.stringify({ ok: true, result: {} }), stderr: '', exitCode: 0 } }
  })
  const refresh = async () => {
    const ready = new Promise<void>(resolve => { refreshed = resolve })
    await $.command.run({ command: 'swarm', args: '' })
    await ready
  }
  return { refresh, ledgerCalls: () => ledgerCalls, linearListCalls: () => linearListCalls }
}

test('ledger stats stays cached across refreshes within ten minutes', async ($, on) => {
  const fixture = await linearUsageFixture($, on, { content: '' })
  await fixture.refresh()
  await fixture.refresh()
  expect(fixture.ledgerCalls()).toBe(1)
})

test('header sums counter-file entries and the board own orca linear calls', async ($, on) => {
  const nowSec = Math.floor(Date.now() / 1000)
  const content = [
    nowSec + ' precheck',
    (nowSec - 60) + ' precheck',
    (nowSec - 120) + ' precheck',
    (nowSec - 30) + ' prune',
    (nowSec - 7200) + ' precheck',
  ].join('\n')
  const fixture = await linearUsageFixture($, on, { content })
  await fixture.refresh()
  const boardCount = fixture.linearListCalls()
  expect(boardCount).toBeGreaterThan(0)
  const ui = await mount($, 'terminal')
  expect(await ui.find({ type: 'Text', text: new RegExp('Linear 本小时 ' + (4 + boardCount) + '\\/2500') })).toBeDefined()
  await ui.press({ key: 'toggle-detail' })
  await ui.redraw()
  expect(await ui.find({ type: 'Text', text: new RegExp('看板 ' + boardCount + '(?![0-9])') })).toBeDefined()
  await ui.unmount()
})

test('header falls back to Linear 用量未知 when the counter file is missing', async ($, on) => {
  const fixture = await linearUsageFixture($, on, { missing: true })
  await fixture.refresh()
  const ui = await mount($, 'terminal')
  expect(await ui.find({ type: 'Text', text: /Linear 用量未知/ })).toBeDefined()
  await ui.unmount()
})

test('wrap-up older than ten minutes warns only without a coordinator', async ($, on) => {
  const completedAt = new Date(Date.now() - 11 * 60_000).toISOString()
  let refreshed = () => {}
  let ps = '   123 S node ordinary-worker'
  let psExit = 0
  let emptyTasks = false
  let taskStatus = 'completed'
  let gateLog = ''
  mock.clock(on)
  on('ui.open', () => ({ value: undefined }))
  on('ui.status', () => { refreshed(); return { value: undefined } })
  on('fs.read', (_: unknown, e: { path: string }) => ({ value: e.path.endsWith('/gate.log') ? gateLog : 'AGENT_GH_REPO=owner/test-repo\nAGENT_LINEAR_TEAM=ZII\nAGENT_LINEAR_PROJECT=Factory' }))
  on('fs.list', () => ({ value: gateLog ? [{ name: 'main' }] : [] }))
  on('process.run', (_: unknown, e: { argv: string[] }) => {
    if (e.argv[0] === 'ps') return { value: { stdout: ps, stderr: '', exitCode: psExit } }
    let result: unknown = { ok: true, result: {} }
    if (e.argv[2] === 'list-issues') result = { ok: true, result: { issues: [{ identifier: 'ZII-848', title: '阶段卡片', state: { type: 'started' }, labels: [{ name: 'auto-impl' }], updatedAt: completedAt }] } }
    if (e.argv[2] === 'run-list') result = { ok: true, result: { runs: [{ id: 'run-1', objective: 'ZII-848', created_at: completedAt }] } }
    if (e.argv[2] === 'task-list') result = { ok: true, result: { tasks: emptyTasks ? [] : [{ id: 'task-1', status: taskStatus, completed_at: completedAt }] } }
    return { value: { stdout: JSON.stringify(result), stderr: '', exitCode: 0 } }
  })
  const refresh = async () => {
    await $.command.run({ command: RESET_LINEAR_THROTTLE_COMMAND, args: '' })
    const ready = new Promise<void>(resolve => { refreshed = resolve })
    await $.command.run({ command: 'swarm', args: '' })
    await ready
  }
  await refresh()
  const ui = await mount($, 'terminal')
  expect(await ui.find({ type: 'Text', text: /无 coordinator 在跑/ })).toBeDefined()
  emptyTasks = true
  await refresh(); await ui.redraw()
  expect(await ui.find({ type: 'Text', text: /无 coordinator 在跑/ })).toBeDefined()
  ps = '   235 S /home/u/.local/bin/orca-factory-driver serve'
  await refresh(); await ui.redraw()
  expect(await ui.find({ type: 'Text', text: /无 coordinator 在跑/ })).toBeUndefined()
  ps = '   234 S node scripts/agent-gates/COORDINATOR.md'
  await refresh(); await ui.redraw()
  expect(await ui.find({ type: 'Text', text: /无 coordinator 在跑/ })).toBeUndefined()
  expect(await ui.find({ type: 'Text', text: /📬 待收尾/ })).toBeDefined()
  psExit = 1
  await refresh(); await ui.redraw()
  expect(await ui.find({ type: 'Text', text: /无 coordinator 在跑/ })).toBeUndefined()
  emptyTasks = false
  gateLog = '=== gate main'
  await refresh(); await ui.redraw()
  expect(await ui.find({ type: 'Text', text: /🧪 gate ·/ })).toBeDefined()
  taskStatus = 'dispatched'
  gateLog = ''
  await refresh(); await ui.redraw()
  expect(await ui.find({ type: 'Text', text: /🔨 实现 ·/ })).toBeDefined()
  taskStatus = 'completed'
  gateLog = 'VERDICT green\nLANE 2'
  await refresh(); await ui.redraw()
  expect(await ui.find({ type: 'Text', text: /🔍 审查（推断） ·/ })).toBeDefined()
  taskStatus = 'ready'
  gateLog = 'VERDICT green'
  await refresh(); await ui.redraw()
  expect(await ui.find({ type: 'Text', text: /📤 派工 ·/ })).toBeDefined()
  await ui.unmount()
})

const snapshotFixture = async (
  $: any,
  on: any,
  opts: {
    generatedAtOffsetMs?: number
    issues?: unknown[]
    comments?: Record<string, unknown[]>
    blockedBy?: Record<string, string[]>
    missing?: boolean
  },
) => {
  __resetSwarmBoardCachesForTests()
  let orcaLinearCalls = 0
  let refreshed: () => void = () => {}
  mock.clock(on)
  on('ui.open', () => ({ value: undefined }))
  on('ui.status', () => { refreshed(); return { value: undefined } })
  on('fs.read', (_: unknown, e: { path: string }) => {
    if (e.path.endsWith('/orca-factory-linear-snapshot.json')) {
      if (opts.missing) throw new Error('ENOENT')
      const generatedAt = Math.floor((Date.now() + (opts.generatedAtOffsetMs ?? 0)) / 1000)
      return { value: JSON.stringify({ generatedAt, issues: opts.issues ?? [], comments: opts.comments ?? {}, blockedBy: opts.blockedBy }) }
    }
    return { value: 'AGENT_GH_REPO=owner/test-repo\nAGENT_LINEAR_TEAM=ZII\nAGENT_LINEAR_PROJECT=Factory\nAGENT_MAX_IN_FLIGHT=10' }
  })
  on('process.run', (_: unknown, e: { argv: string[] }) => {
    if (e.argv[0] === 'sh' && e.argv[2]?.includes('AGENT_SNAPSHOT_FILE')) {
      return { value: { stdout: '/tmp/orca-factory-linear-snapshot.json', stderr: '', exitCode: 0 } }
    }
    if (e.argv[0] === 'orca' && e.argv[1] === 'linear') orcaLinearCalls++
    if (e.argv[0] === 'orca' && e.argv[1] === 'linear' && e.argv[2] === 'list-issues') {
      return { value: { stdout: JSON.stringify({ ok: true, result: { issues: opts.issues ?? [] } }), stderr: '', exitCode: 0 } }
    }
    if (e.argv[0] === 'orca' && e.argv[1] === 'linear' && e.argv[2] === 'issue') {
      const id = e.argv[3]
      return { value: { stdout: JSON.stringify({ result: { comments: opts.comments?.[id] ?? [] } }), stderr: '', exitCode: 0 } }
    }
    return { value: { stdout: JSON.stringify({ ok: true, result: {} }), stderr: '', exitCode: 0 } }
  })
  const refresh = async () => {
    const ready = new Promise<void>(resolve => { refreshed = resolve })
    await $.command.run({ command: 'swarm', args: '' })
    await ready
  }
  await refresh()
  return { refresh, orcaLinearCalls: () => orcaLinearCalls }
}

test('fresh snapshot refresh avoids live Linear calls and uses snapshot data', async ($, on) => {
  const updatedAt = new Date(Date.now() - 2 * 60_000).toISOString()
  const fixture = await snapshotFixture($, on, {
    issues: [{ identifier: 'ZII-900', title: '快照票', state: { type: 'started' }, labels: [{ name: 'auto-impl' }], updatedAt, priority: 0 }],
    comments: { 'ZII-900': [{ body: '已领取 ZII-900', createdAt: new Date(Date.now() - 60_000).toISOString() }] },
  })
  expect(fixture.orcaLinearCalls()).toBe(0)
  const ui = await mount($, 'terminal')
  expect(await ui.find({ type: 'Text', text: /ZII-900/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /数据来自快照/ })).toBeDefined()
  await ui.unmount()
})

test('snapshot polling redraws changed data without live Linear calls', async ($, on) => {
  __resetSwarmBoardCachesForTests()
  const clock = mock.clock(on)
  let mtimeMs = 1, statCalls = 0, snapshotReads = 0, linearCalls = 0
  let missing = false
  let title = '快照原值'
  on('ui.open', () => ({ value: undefined }))
  on('ui.close', () => ({ value: undefined }))
  on('ui.status', () => ({ value: undefined }))
  on('fs.stat', () => {
    statCalls++
    if (missing) throw new Error('ENOENT')
    return { value: { kind: 'file', mtimeMs, size: 100, isLink: false } }
  })
  on('fs.read', (_: unknown, e: { path: string }) => {
    if (e.path.endsWith('/orca-factory-linear-snapshot.json')) {
      snapshotReads++
      return { value: JSON.stringify({
        generatedAt: Math.floor(Date.now() / 1000),
        issues: [{ identifier: 'ZII-900', title, state: { type: 'unstarted' },
          labels: [{ name: 'ready-for-agent' }], priority: 1 }], comments: {},
      }) }
    }
    return { value: 'AGENT_GH_REPO=owner/test-repo\nAGENT_LINEAR_TEAM=ZII\nAGENT_LINEAR_PROJECT=Factory' }
  })
  on('process.run', (_: unknown, e: { argv: string[] }) => {
    if (e.argv[0] === 'sh' && e.argv[2]?.includes('AGENT_SNAPSHOT_FILE')) {
      return { value: { stdout: '/tmp/orca-factory-linear-snapshot.json', stderr: '', exitCode: 0 } }
    }
    if (e.argv[0] === 'orca' && e.argv[1] === 'linear') linearCalls++
    return { value: { stdout: JSON.stringify({ ok: true, result: {} }), stderr: '', exitCode: 0 } }
  })
  await $.command.run({ command: 'swarm', args: '' })
  await clock.settle()
  const ui = await mount($, 'terminal')
  const initialReads = snapshotReads
  await clock.advance(5000)
  expect(statCalls).toBe(1)
  expect(snapshotReads).toBe(initialReads)
  title = '快照已更新'
  mtimeMs++
  await clock.advance(5000)
  await ui.redraw()
  expect(await ui.find({ type: 'Text', text: /快照已更新/ })).toBeDefined()
  expect(snapshotReads).toBe(initialReads + 1)
  expect(linearCalls).toBe(0)
  missing = true
  await clock.advance(5000)
  expect(snapshotReads).toBe(initialReads + 1)
  await $.command.run({ command: SIMULATE_CLOSE_COMMAND, args: '' })
  const beforeClose = statCalls
  await clock.advance(5000)
  expect(statCalls).toBe(beforeClose)
  await ui.unmount()
})

test('stale snapshot falls back to live Linear and throttles the second refresh', async ($, on) => {
  const fixture = await snapshotFixture($, on, { generatedAtOffsetMs: -11 * 60_000, issues: [], comments: {} })
  const after1 = fixture.orcaLinearCalls()
  expect(after1).toBeGreaterThan(0)
  await fixture.refresh()
  expect(fixture.orcaLinearCalls()).toBe(after1)
})

test('missing snapshot falls back to live Linear without error', async ($, on) => {
  const fixture = await snapshotFixture($, on, { missing: true })
  expect(fixture.orcaLinearCalls()).toBeGreaterThan(0)
})

test('blockedBy renders 等依赖 reason on a queue card', async ($, on) => {
  await snapshotFixture($, on, {
    issues: [{ identifier: 'ZII-855', title: '等依赖的票', state: { type: 'unstarted' }, labels: [{ name: 'ready-for-agent' }], updatedAt: '2026-10-04T00:00:00Z', priority: 2 }],
    blockedBy: { 'ZII-855': ['ZII-853'] },
  })
  const ui = await $.ui.mount({ plugin: 'swarm-board', surface: 'terminal', component: 'Pane', requestId: 'swarm-board', props: { title: 'Swarm', bodyColumns: 160 } })
  expect(await ui.find({ type: 'Text', text: /等 853/ })).toBeDefined()
  await ui.unmount()
})

test('blockedBy suppresses the 0-claimable warning when every queue ticket is blocked', async ($, on) => {
  __resetSwarmBoardCachesForTests()
  let refreshed: () => void = () => {}
  mock.clock(on)
  on('ui.open', () => ({ value: undefined }))
  on('ui.status', () => { refreshed(); return { value: undefined } })
  on('fs.read', (_: unknown, e: { path: string }) => {
    if (e.path.endsWith('/orca-factory-linear-snapshot.json')) {
      return { value: JSON.stringify({
        generatedAt: Math.floor(Date.now() / 1000),
        issues: [
          { identifier: 'ZII-855', title: '等依赖', state: { type: 'unstarted' }, labels: [{ name: 'ready-for-agent' }], updatedAt: '2026-10-04T00:00:00Z', priority: 2 },
          { identifier: 'ZII-861', title: '等依赖2', state: { type: 'unstarted' }, labels: [{ name: 'ready-for-agent' }], updatedAt: '2026-10-04T00:00:00Z', priority: 2 },
        ],
        comments: {},
        blockedBy: { 'ZII-855': ['ZII-853'], 'ZII-861': ['ZII-857', 'ZII-855'] },
      }) }
    }
    return { value: 'AGENT_GH_REPO=owner/test-repo\nAGENT_LINEAR_TEAM=ZII\nAGENT_LINEAR_PROJECT=Factory\nAGENT_MAX_IN_FLIGHT=10' }
  })
  on('process.run', (_: unknown, e: { argv: string[] }) => {
    if (e.argv[0] === 'sh' && e.argv[2]?.includes('AGENT_SNAPSHOT_FILE')) {
      return { value: { stdout: '/tmp/orca-factory-linear-snapshot.json', stderr: '', exitCode: 0 } }
    }
    if (e.argv[1] === 'automations' && e.argv[2] === 'runs') {
      return { value: { stdout: JSON.stringify({ ok: true, result: { runs: [
        { runNumber: 1, status: 'succeeded', scheduledFor: new Date().toISOString(),
          precheckResult: { exitCode: 0, timedOut: false, error: '', stdout: 'claimable= in_flight=0 max=10\n', stderr: '' } },
      ] } }), stderr: '', exitCode: 0 } }
    }
    if (e.argv[1] === 'automations' && e.argv[2] === 'list') {
      return { value: { stdout: JSON.stringify({ ok: true, result: { automations: [
        { id: 'a1', name: 'agent-linear-scan (test-repo)', enabled: true },
      ] } }), stderr: '', exitCode: 0 } }
    }
    return { value: { stdout: JSON.stringify({ ok: true, result: {} }), stderr: '', exitCode: 0 } }
  })
  const ready = new Promise<void>(resolve => { refreshed = resolve })
  await $.command.run({ command: 'swarm', args: '' })
  await ready
  const ui = await mount($, 'terminal')
  expect(await ui.find({ type: 'Text', text: /全部待领票都在等依赖/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /最先放行的阻塞者：ZII-853/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /precheck 判定可领 0 张/ })).toBeUndefined()
  await ui.unmount()
})

test('precheck failed line surfaces as the uncertain reason without duplicating the exit code', async ($, on) => {
  await queueFixture($, on, 1, {
    exitCode: 3, runNumber: 202, stdout: '',
    stderr: 'orca linear issue ZII-855 --relations failed: linear_network_error\nlinear_calls=1',
  })
  const ui = await mount($, 'terminal')
  expect(await ui.find({ type: 'Text', text: /linear_network_error/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /网络波动/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /退出码 3（退出码 3）/ })).toBeUndefined()
  await ui.unmount()
})

const chainFixture = async (
  $: any,
  on: any,
  opts: { issues?: unknown[]; blockedBy?: Record<string, string[]> },
) => {
  __resetSwarmBoardCachesForTests()
  let refreshed: () => void = () => {}
  mock.clock(on)
  on('ui.open', () => ({ value: undefined }))
  on('ui.status', () => { refreshed(); return { value: undefined } })
  on('fs.read', (_: unknown, e: { path: string }) => {
    if (e.path.endsWith('/orca-factory-linear-snapshot.json')) {
      return { value: JSON.stringify({
        generatedAt: Math.floor(Date.now() / 1000),
        issues: opts.issues ?? [],
        comments: {},
        blockedBy: opts.blockedBy ?? {},
      }) }
    }
    return { value: 'AGENT_GH_REPO=owner/test-repo\nAGENT_LINEAR_TEAM=ZII\nAGENT_LINEAR_PROJECT=Factory\nAGENT_MAX_IN_FLIGHT=10' }
  })
  on('process.run', (_: unknown, e: { argv: string[] }) => {
    if (e.argv[0] === 'sh' && e.argv[2]?.includes('AGENT_SNAPSHOT_FILE')) {
      return { value: { stdout: '/tmp/orca-factory-linear-snapshot.json', stderr: '', exitCode: 0 } }
    }
    return { value: { stdout: JSON.stringify({ ok: true, result: {} }), stderr: '', exitCode: 0 } }
  })
  const ready = new Promise<void>(resolve => { refreshed = resolve })
  await $.command.run({ command: 'swarm', args: '' })
  await ready
}
const queueIssue = (id: string) => ({
  identifier: id, title: 'q-' + id, state: { type: 'unstarted' },
  labels: [{ name: 'ready-for-agent' }], updatedAt: '2026-10-04T00:00:00Z', priority: 2,
})
const extIssue = (id: string, state: string) => ({
  identifier: id, title: 'x-' + id, state: { type: state },
  labels: [], updatedAt: '2026-10-04T00:00:00Z',
})
const mountWide = ($: any, bodyColumns = 108) =>
  $.ui.mount({ plugin: 'swarm-board', surface: 'terminal', component: 'Pane', requestId: 'swarm-board', props: { title: 'Swarm', bodyColumns } })

test('queue stats line counts tickets per group', async ($, on) => {
  await chainFixture($, on, {
    issues: [queueIssue('ZII-855'), queueIssue('ZII-860')],
    blockedBy: { 'ZII-855': ['ZII-853'] },
  })
  const ui = await mount($, 'terminal')
  // #then — one blocked (ZII-855), one unknown (ZII-860, no precheck); none claimable or warn.
  expect(await ui.find({ type: 'Text', text: /待领 2 · 🟢 可放行 0 · ⛓ 等依赖 1 · ⚠ 异常 0 · 未知 1/ })).toBeDefined()
  await ui.unmount()
})

test('chain line shows external completed, started and queued badges', async ($, on) => {
  await chainFixture($, on, {
    issues: [extIssue('ZII-853', 'completed'), extIssue('ZII-854', 'started'), queueIssue('ZII-855'), queueIssue('ZII-856')],
    blockedBy: { 'ZII-855': ['ZII-853'], 'ZII-856': ['ZII-854'] },
  })
  const ui = await mountWide($)
  expect(await ui.find({ type: 'Text', text: /链：/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /853✓/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /854🔨/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /855📥/ })).toBeDefined()
  await ui.unmount()
})

test('blocked card lists blockers with short id and badge', async ($, on) => {
  await chainFixture($, on, {
    issues: [extIssue('ZII-855', 'started'), queueIssue('ZII-857')],
    blockedBy: { 'ZII-857': ['ZII-855'] },
  })
  const ui = await mount($, 'terminal')
  expect(await ui.find({ type: 'Text', text: /等 855🔨/ })).toBeDefined()
  await ui.unmount()
})

test('warn group sits above the other groups when any ticket is warn', async ($, on) => {
  await queueFixture($, on, 2, { exitCode: 3, runNumber: 220,
    stdout: '', stderr: '不确定: linear rate limited\nlinear_calls=1' })
  const ui = await mount($, 'terminal')
  // #then — the warn heading appears, and the ⚠ card badge for one queue ticket precedes any 🟢/⛓ heading.
  const warnHead = await ui.find({ type: 'Text', text: /⚠ 异常 \(2\)/ })
  expect(warnHead).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /⚠ ZII-1/ })).toBeDefined()
})

test('each group folds into +N when it holds more than five tickets', async ($, on) => {
  await chainFixture($, on, {
    issues: Array.from({ length: 7 }, (_, i) => queueIssue('ZII-' + (900 + i))),
    blockedBy: {}, // every ticket is unknown
  })
  const ui = await mount($, 'terminal')
  // #then — stats show seven in unknown, and only five cards render under the group.
  expect(await ui.find({ type: 'Text', text: /未知 7/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /^\+2$/ })).toBeDefined()
})

test('narrow lane skips the dependency chain line', async ($, on) => {
  await chainFixture($, on, {
    issues: [extIssue('ZII-853', 'completed'), queueIssue('ZII-855')],
    blockedBy: { 'ZII-855': ['ZII-853'] },
  })
  const ui = await $.ui.mount({ plugin: 'swarm-board', surface: 'terminal', component: 'Pane', requestId: 'swarm-board', props: { title: 'Swarm', bodyColumns: 60 } })
  expect(await ui.find({ type: 'Text', text: /链：/ })).toBeUndefined()
  await ui.unmount()
})

const closeFixture = ($: any, on: any) => {
  __resetSwarmBoardCachesForTests()
  let fire60s = () => {}, done = () => {}, runs = 0
  on('clock.every', (_: unknown, e: { ms: number }) => e.ms === 60_000
    ? new Promise<{ value: undefined }>(r => { fire60s = () => r({ value: undefined }) })
    : new Promise<never>(() => {}))
  on('ui.open', () => ({ value: undefined })); on('ui.close', () => ({ value: undefined }))
  on('ui.status', () => { done(); return { value: undefined } })
  on('fs.read', () => ({ value: 'AGENT_GH_REPO=owner/test-repo\nAGENT_LINEAR_TEAM=ZII\nAGENT_LINEAR_PROJECT=Factory' }))
  on('process.run', (_: unknown, e: { argv: string[] }) => {
    if (e.argv[0] === 'orca' || e.argv[0] === 'gh') runs++
    return { value: { stdout: '{"ok":true,"result":{}}', stderr: '', exitCode: 0 } }
  })
  const open = async () => { const d = new Promise<void>(r => { done = r }); await $.command.run({ command: 'swarm', args: '' }); await d }
  return { open, close: () => $.command.run({ command: SIMULATE_CLOSE_COMMAND, args: '' }), runs: () => runs, tick60s: () => fire60s() }
}

test('closing the swarm pane halts scheduled refreshes', async ($, on) => {
  const f = closeFixture($, on); await f.open(); const before = f.runs()
  expect(before).toBeGreaterThan(0); await f.close(); f.tick60s()
  await new Promise(r => setTimeout(r, 30)); expect(f.runs()).toBe(before)
})

test('reopening the swarm pane triggers an immediate refresh', async ($, on) => {
  const f = closeFixture($, on); await f.open(); const before = f.runs()
  await f.close(); await f.open(); expect(f.runs()).toBeGreaterThan(before)
})

const driverFixture = async ($: any, on: any, stdout: string, exitCode = 0, missing = false) => {
  let refreshed = () => {}
  mock.clock(on)
  on('ui.open', () => ({ value: undefined }))
  on('ui.status', () => { refreshed(); return { value: undefined } })
  on('fs.read', (_: unknown, e: { path: string }) => {
    if (e.path.endsWith('/orca-factory-linear-calls')) throw new Error('ENOENT')
    return { value: 'AGENT_GH_REPO=owner/test-repo\nAGENT_LINEAR_TEAM=ZII\nAGENT_LINEAR_PROJECT=Factory' }
  })
  on('process.run', (_: unknown, e: { argv: string[]; init: { timeoutMs: number } }) => {
    if (e.argv[0] === 'orca-factory-driver') {
      expect(e.argv).toEqual(['orca-factory-driver', 'status', '--json'])
      expect(e.init.timeoutMs).toBe(20_000)
      if (missing) throw new Error('command not found')
      return { value: { stdout, stderr: '', exitCode } }
    }
    return { value: { stdout: '{"ok":true,"result":{}}', stderr: '', exitCode: 0 } }
  })
  await $.command.run({ command: RESET_LINEAR_THROTTLE_COMMAND, args: '' })
  const ready = new Promise<void>(resolve => { refreshed = resolve })
  await $.command.run({ command: 'swarm', args: '' })
  await ready
}

test('driver global pause and hourly Linear usage appear on the board', async ($, on) => {
  await driverFixture($, on, '{"state":"ok","globalPause":{"active":true,"durationSeconds":180},"linear":{"requests":12,"complexity":34,"limits":[{"repo":"/repo","requests":{"remaining":88,"reset":"2026-10-05T09:00:00Z"},"complexity":{"remaining":966,"reset":"2026-10-05T09:30:00Z"}}]}}')
  const ui = await mount($, 'terminal')
  expect(await ui.find({ type: 'Text', text: /⏸ 全局暂停 180 秒/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /驱动器 Linear 本小时 请求 12 · 复杂度 34/ })).toBeDefined()
  expect(await ui.find({ type: 'Text', text: /\/repo 请求剩余 88 · 重置 2026-10-05T09:00:00Z · 复杂度剩余 966 · 重置 2026-10-05T09:30:00Z/ })).toBeDefined()
  await ui.unmount()
})

for (const failure of ['missing', 'nonzero', 'invalid', 'wrong shape']) {
  test('driver ' + failure + ' preserves the original board output', async ($, on) => {
    await driverFixture($, on, failure === 'wrong shape' ? '{}' : 'invalid', failure === 'nonzero' ? 1 : 0, failure === 'missing')
    const ui = await mount($, 'terminal')
    expect(await ui.find({ type: 'Text', text: /🟢 需要人 0/ })).toBeDefined()
    expect(await ui.find({ type: 'Text', text: /Linear 用量未知/ })).toBeDefined()
    expect(await ui.find({ type: 'Text', text: /驱动器 Linear/ })).toBeUndefined()
    expect(await ui.find({ type: 'Text', text: /全局暂停/ })).toBeUndefined()
    await ui.unmount()
  })
}

for (const state of ['unknown', 'stale']) {
  test('driver ' + state + ' is explicitly labelled', async ($, on) => {
    await driverFixture($, on, JSON.stringify({ state, globalPause: { active: true, durationSeconds: 180 }, linear: { requests: 12, complexity: 34, limits: [] } }))
    const ui = await mount($, 'terminal')
    expect(await ui.find({ type: 'Text', text: state === 'unknown' ? /驱动器 Linear 未知/ : /驱动器 Linear 状态过期/ })).toBeDefined()
    expect(await ui.find({ type: 'Text', text: /全局暂停/ })).toBeUndefined()
    await ui.unmount()
  })
}
