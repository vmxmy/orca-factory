import { expect, mock, test } from 'claude-code/testing'
import type { FoundElement } from 'claude-code/testing'
import type { On, ProcessRunResult } from 'claude-code'

import { SAMPLE_CONFIG } from './sample-config'
import { NOW, SAMPLE_DOCTOR, SAMPLE_STATUS } from './sample-runtime'

const PANE = {
  plugin: 'ziikoo-driver',
  component: 'Pane',
  requestId: 'ziikoo-driver',
  surface: 'terminal',
  props: {
    title: 'Driver 配置',
    isFocused: true,
    bodyColumns: 100,
    placement: 'dock',
    scroll: { offset: 0, bodyRows: 40 },
    view: {},
  },
} as const

const ran = (result: Partial<ProcessRunResult>): ProcessRunResult => ({
  exitCode: 0,
  stdout: '',
  stderr: '',
  isStdoutTruncated: false,
  isStderrTruncated: false,
  ...result,
})

const engine = (on: On, result: ProcessRunResult, bySubcommand: Record<string, ProcessRunResult> = {}) => {
  const calls: string[][] = []
  const clock = mock.clock(on, { now: NOW })
  on('command.register', () => ({ value: { command: 'ziikoo-driver' } }))
  on('ui.open', () => ({ value: { isPlaced: true } }))
  on('ui.panes', () => ({
    value: [{ id: 'ziikoo-driver', title: 'Driver 配置', isShown: true, isFocused: true, isPlaced: true, plugin: 'ziikoo-driver' }],
  }))
  on('process.run', (_, e) => {
    calls.push([...e.argv])
    return { value: bySubcommand[e.argv[1] ?? ''] ?? result }
  })
  return { calls, clock }
}

const runCommand = {
  command: 'ziikoo-driver',
  args: '',
  origin: { kind: 'composer' },
  presentation: { isFullscreen: true, columns: 120 },
} as const

const sampleStdout = JSON.stringify(SAMPLE_CONFIG)

const exactText = async (ui: { findAll: (q: { type: string }) => Promise<FoundElement[]> }, text: string) =>
  (await ui.findAll({ type: 'Text' })).filter(one => one.text === text)

test('模型页列出施工档位与样本模型名，可切换仓库', async ($, on) => {
  const { calls } = engine(on, ran({ stdout: sampleStdout }))
  await $.command.run(runCommand)
  expect(calls).toEqual([['orca-factory-driver', 'config', '--json']])

  const ui = await $.ui.mount(PANE)
  expect((await ui.find({ type: 'Text', text: /006396a · active/ }))?.text).toBe('006396a · active')
  await ui.press({ key: 'page-models' })

  expect(await exactText(ui, '施工')).toHaveLength(1)
  expect(await exactText(ui, '施工 升档')).toHaveLength(1)
  expect(await exactText(ui, 'gpt-6.1-sol')).toHaveLength(1)
  expect(await exactText(ui, '小票审查')).toHaveLength(0)
  expect((await ui.find({ type: 'Text', text: /打回升档/ }))?.text).toBe('打回升档 开 · 小票审查 ≤2')

  await ui.select({ key: 'repo', value: 'ziikoo-web' })
  expect(await exactText(ui, '小票审查')).toHaveLength(1)
  expect(await exactText(ui, 'claude-haiku-5-5')).toHaveLength(1)
  await ui.unmount()
})

test('调度页按用途分组，值带单位，只高亮 driver.toml 改过的项', async ($, on) => {
  engine(on, ran({ stdout: sampleStdout }))
  await $.command.run(runCommand)
  const ui = await $.ui.mount(PANE)
  await ui.press({ key: 'page-scheduler' })

  for (const group of ['并发与限速', 'GitHub', 'Orca 与执行器', '账本']) {
    expect(await exactText(ui, group)).toHaveLength(1)
  }
  const row = async (key: string) => (await ui.find({ key: `setting-${key}` }))?.text
  expect(await row('max_in_flight')).toBe('同时运行的票最多 4 张driver.tomlmax_in_flight')
  expect(await row('token_bucket.refill_per_second')).toBe('领票补充速度每秒 0.5 次默认token_bucket.refill_per_second')
  expect(await row('github.http_timeout')).toBe('请求超时30 秒默认github.http_timeout')
  expect(await row('orca.worker_start_wait')).toBe('worker 就绪等待1 分钟 30 秒driver.tomlorca.worker_start_wait')
  expect(await row('ledger.flush_interval')).toBe('用量账本写回每 100 毫秒默认ledger.flush_interval')
  expect((await exactText(ui, 'driver.toml')).map(one => one.props.color)).toEqual(['suggestion', 'suggestion', 'suggestion'])
  expect((await exactText(ui, '同时运行的票')).map(one => one.props.dimColor)).toEqual([false])
  expect((await exactText(ui, '默认')).map(one => one.props.dimColor)).toEqual([true, true, true, true, true])
  await ui.unmount()
})

test('环境页 secret 项只显示已设置，样本里没有可泄露的值', async ($, on) => {
  const secrets = SAMPLE_CONFIG.environment.filter(entry => 'secret' in entry)
  expect(secrets).toHaveLength(3)
  expect(secrets.filter(entry => 'value' in entry)).toHaveLength(0)

  engine(on, ran({ stdout: sampleStdout }))
  await $.command.run(runCommand)
  const ui = await $.ui.mount(PANE)
  await ui.press({ key: 'page-environment' })

  expect((await ui.find({ key: 'env-AGENT_FEISHU_APP_SECRET' }))?.text).toBe('AGENT_FEISHU_APP_SECRET已设置')
  expect((await ui.find({ key: 'env-GITHUB_TOKEN' }))?.text).toBe('GITHUB_TOKEN未设置')
  expect((await ui.find({ key: 'env-gh' }))?.text).toBe('gh/usr/bin/gh')
  expect((await ui.find({ key: 'env-HTTPS_PROXY' }))?.text).toBe('HTTPS_PROXYhttp://127.0.0.1:7890')
  await ui.unmount()
})

test('config 非零退出时渲染失败状态与 stderr 摘要', async ($, on) => {
  engine(on, ran({ exitCode: 2, stderr: 'unknown command "config" for "orca-factory-driver"\nRun --help for usage.' }))
  await $.command.run(runCommand)
  const ui = await $.ui.mount(PANE)

  expect((await ui.find({ type: 'Text', text: /取数失败/ }))?.text).toBe('取数失败（退出码 2）')
  expect((await ui.find({ type: 'Text', text: /unknown command/ }))?.text).toBe(
    'unknown command "config" for "orca-factory-driver"',
  )
  await ui.unmount()
})

test('schema 不对时进入失败状态并给出原因', async ($, on) => {
  engine(on, ran({ stdout: JSON.stringify({ ...SAMPLE_CONFIG, schema: 'driver-config-v2' }) }))
  await $.command.run(runCommand)
  const ui = await $.ui.mount(PANE)

  expect((await ui.find({ type: 'Text', text: /schema/ }))?.text).toBe('schema 不是 driver-config-v1：driver-config-v2')
  await ui.unmount()
})

test('未知调度键时进入失败状态', async ($, on) => {
  const scheduler = [...SAMPLE_CONFIG.scheduler, { key: 'scheduler.new_knob', value: '1', source: 'default', set: true }]
  engine(on, ran({ stdout: JSON.stringify({ ...SAMPLE_CONFIG, scheduler }) }))
  await $.command.run(runCommand)
  const ui = await $.ui.mount(PANE)

  expect((await ui.find({ type: 'Text', text: /取值未知/ }))?.text).toBe('scheduler[8].key 取值未知：scheduler.new_knob')
  await ui.unmount()
})

const runtimeEngine = (on: On, status: unknown = SAMPLE_STATUS) =>
  engine(on, ran({ stdout: sampleStdout }), {
    status: ran({ stdout: JSON.stringify(status) }),
    doctor: ran({ exitCode: 1, stdout: JSON.stringify(SAMPLE_DOCTOR) }),
  })

const subcommands = (calls: string[][]) => calls.map(argv => argv[1])

test('流水线页按项目画阶段轨道，没配部署的仓库没有部署点', async ($, on) => {
  const { calls } = runtimeEngine(on)
  await $.command.run(runCommand)
  const ui = await $.ui.mount(PANE)
  await ui.press({ key: 'page-pipeline' })
  expect(subcommands(calls).sort()).toEqual(['config', 'doctor', 'status'])

  const row = async (id: string) => (await ui.find({ key: `pipeline-${id}` }))?.text
  expect(await row('OF-1')).toBe('OF-1●━━━━━●━━━━━◉─────○─────○─────○gate4 分钟')
  expect(await row('OF-3')).toBe('OF-3●━━━━━●━━━━━●━━━━━●━━━━━●━━━━━◉smoke2 小时 5 分')
  expect(await row('ZW-7')).toBe('ZW-7●━━━━━◉─────○─────○─────○worker 运行中30 秒')
  expect((await exactText(ui, '◉')).map(one => one.props.color)).toEqual(['suggestion', 'warning', 'suggestion', 'suggestion', 'suggestion'])
  expect((await exactText(ui, '写 correction')).map(one => one.props.color)).toEqual(['warning'])

  expect((await ui.find({ type: 'Text', text: /^在途/ }))?.text).toBe('在途 4   部署屏障 1   待领 1')
  expect((await exactText(ui, '快照 8 秒前 · 每 10 秒刷新')).map(one => one.props.dimColor)).toEqual([true])
  expect(await exactText(ui, '流水线冒烟（OF）')).toHaveLength(1)
  expect(await exactText(ui, '↳ 部署 · smoke-intent · abc1234')).toHaveLength(1)
  expect((await exactText(ui, '↳ blocked by ZW-7')).map(one => one.props.color)).toEqual(['warning'])
  expect(await exactText(ui, '空闲')).toHaveLength(1)

  expect((await exactText(ui, '在 feat/login')).map(one => one.props.color)).toEqual(['warning'])
  expect((await exactText(ui, 'core 待同步 #12')).map(one => one.props.color)).toEqual(['warning'])
  expect(await exactText(ui, '⚠ 未提交改动')).toHaveLength(1)
  expect(await exactText(ui, 'main ✓')).toHaveLength(2)
  await ui.unmount()
})

test('快照过期时警告；流水线页每 10 秒重新读取 status，离开后停止', async ($, on) => {
  const { calls, clock } = runtimeEngine(on, { ...SAMPLE_STATUS, state: 'stale', generatedAt: '2026-10-27T11:54:00Z' })
  await $.command.run(runCommand)
  const ui = await $.ui.mount(PANE)
  await ui.press({ key: 'page-pipeline' })
  expect((await exactText(ui, '快照 6 分钟前 · 已过期')).map(one => one.props.color)).toEqual(['warning'])

  const statusReads = () => subcommands(calls).filter(one => one === 'status').length
  expect(statusReads()).toBe(1)
  await clock.advance(10_000)
  await clock.settle()
  expect(statusReads()).toBe(2)

  await ui.press({ key: 'page-overview' })
  await clock.advance(10_000)
  await clock.settle()
  expect(statusReads()).toBe(2)
  await ui.unmount()
})

test('窄于 80 列时不画轨道', async ($, on) => {
  runtimeEngine(on)
  await $.command.run(runCommand)
  const ui = await $.ui.mount({ ...PANE, props: { ...PANE.props, bodyColumns: 70 } })
  await ui.press({ key: 'page-pipeline' })
  expect((await ui.find({ key: 'pipeline-OF-1' }))?.text).toBe('OF-1gate4 分钟')
  expect(await exactText(ui, '◉')).toHaveLength(0)
  await ui.unmount()
})
