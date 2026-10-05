import { test, expect, mock } from 'claude-code/testing'

import type { Ticket } from '../types'
import { laneStage, ticketPhase } from './register'

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
  expect(await ui.find({ type: 'Text', text: /待领 2 张但 precheck 判定可领 0 张/ })).toBeDefined()
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
test('completed workers without gate or PR await wrap-up', async () => {
  expect(ticketPhase(phaseTicket, { run: { id: 'run-1' }, tasks: [{ status: 'completed' }] })).toBe('📬 待收尾')
  expect(ticketPhase(phaseTicket, { run: { id: 'run-1' }, tasks: [{ status: 'failed' }, { status: 'completed' }], previous: '🔨 实现' })).toBe('📬 待收尾')
  expect(ticketPhase(phaseTicket, { run: { id: 'run-1' }, tasks: [{ status: 'failed' }], previous: '🔨 实现' })).toBe('🔨 实现')
  expect(ticketPhase(phaseTicket, { run: { id: 'run-1' }, tasks: [{ status: 'failed' }] })).toBe('📤 派工')
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
