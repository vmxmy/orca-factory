import { expect, mock, test } from 'claude-code/testing'
import type { FoundElement } from 'claude-code/testing'
import type { On, ProcessRunResult } from 'claude-code'

import { SAMPLE_CONFIG } from './sample-config'
import { SAMPLE_LOGS } from './sample-logs'
import { NOW, SAMPLE_DOCTOR, SAMPLE_STATUS } from './sample-runtime'

const PANE = {
  plugin: 'ziikoo-driver',
  component: 'Pane',
  requestId: 'ziikoo-driver',
  surface: 'terminal',
  props: {
    title: 'Driver',
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

type Answer = ProcessRunResult | ((nth: number) => ProcessRunResult)

// The test host cannot answer a plugin's $.ui.focus, neither from a test hook nor from an inline plugin beneath,
// so every move fails and the panel's debug line for it names the key it tried.
const FOCUS_LOG = /^focus (\S+) not moved: /

const engine = (on: On, result: ProcessRunResult, bySubcommand: Record<string, Answer> = {}, lateMs: Record<string, number> = {}) => {
  const calls: string[][] = []
  const focused: string[] = []
  const clock = mock.clock(on, { now: NOW })
  on('command.register', () => ({ value: { command: 'ziikoo-driver' } }))
  on('ui.open', () => ({ value: { isPlaced: true } }))
  on('ui.log', (_, e) => {
    const key = FOCUS_LOG.exec(e.text)?.[1]
    if (key !== undefined) focused.push(key)
    return { value: undefined }
  })
  on('ui.panes', () => ({
    value: [{ id: 'ziikoo-driver', title: 'Driver', isShown: true, isFocused: true, isPlaced: true, plugin: 'ziikoo-driver' }],
  }))
  // bash runs the logs pipeline, whose argv[1] is -c.
  const subOf = (argv: readonly string[]) => (argv[0] === 'bash' ? 'bash' : (argv[1] ?? ''))
  on('process.run', async (_, e) => {
    calls.push([...e.argv])
    const sub = subOf(e.argv)
    const late = lateMs[sub]
    if (late !== undefined) await clock.sleep(late)
    const answer = bySubcommand[sub] ?? result
    return { value: typeof answer === 'function' ? answer(calls.filter(argv => subOf(argv) === sub).length) : answer }
  })
  return { calls, clock, focused }
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

const subcommands = (calls: string[][]) => calls.map(argv => argv[1])

test('模型页列出施工档位与样本模型名，可切换仓库', async ($, on) => {
  const { calls } = engine(on, ran({ stdout: sampleStdout }))
  await $.command.run(runCommand)
  expect(subcommands(calls).sort()).toEqual(['config', 'doctor', 'status'])

  const ui = await $.ui.mount(PANE)
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
  expect((await exactText(ui, 'r 重试')).map(one => one.props.dimColor)).toEqual([true])
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

const runtimeEngine = (on: On, status: unknown = SAMPLE_STATUS, doctor: unknown = SAMPLE_DOCTOR) =>
  engine(on, ran({ stdout: sampleStdout }), {
    status: ran({ stdout: JSON.stringify(status) }),
    doctor: ran({ exitCode: 1, stdout: JSON.stringify(doctor) }),
  })

test('流水线页按项目画阶段轨道，没配部署的仓库没有部署点', async ($, on) => {
  const { calls } = runtimeEngine(on)
  await $.command.run(runCommand)
  const ui = await $.ui.mount(PANE)
  expect(subcommands(calls).sort()).toEqual(['config', 'doctor', 'status'])

  const row = async (id: string) => (await ui.find({ key: `pipeline-${id}` }))?.text
  expect(await row('OF-1')).toBe('OF-1●━━━━━●━━━━━◉─────○─────○─────○gate4 分钟')
  expect(await row('OF-3')).toBe('OF-3●━━━━━●━━━━━●━━━━━●━━━━━●━━━━━◉smoke2 小时 5 分')
  expect(await row('ZW-7')).toBe('ZW-7●━━━━━◉─────○─────○─────○worker 运行中30 秒')
  expect((await exactText(ui, '◉')).map(one => one.props.color)).toEqual(['suggestion', 'warning', 'suggestion', 'suggestion', 'suggestion'])
  expect((await exactText(ui, '写 correction')).map(one => one.props.color)).toEqual(['warning'])

  expect((await ui.find({ type: 'Text', text: /^在途/ }))?.text).toBe('在途 4   部署屏障 1   待领 1')
  expect((await exactText(ui, '快照 8 秒前')).map(one => one.props.dimColor)).toEqual([true])
  expect(await exactText(ui, '流水线冒烟（OF）')).toHaveLength(1)
  expect(await exactText(ui, '↳ 部署 · smoke-intent · abc1234')).toHaveLength(1)
  expect((await exactText(ui, '↳ blocked by ZW-7')).map(one => one.props.color)).toEqual(['warning'])
  expect(await exactText(ui, '空闲')).toHaveLength(1)
  await ui.unmount()
})

test('流水线页每个项目一个区块，需要处理的徽标画成标签', async ($, on) => {
  runtimeEngine(on)
  await $.command.run(runCommand)
  const ui = await $.ui.mount(PANE)

  expect((await exactText(ui, 'orca-factory')).map(one => one.props.bold)).toEqual([true])
  expect((await exactText(ui, 'ziikoo-web')).map(one => one.props.bold)).toEqual([true])
  const chip = (one: FoundElement) => [one.props.backgroundColor, one.props.color]
  expect((await exactText(ui, ' 在 feat/login ')).map(chip)).toEqual([['warning', 'inverseText']])
  expect((await exactText(ui, ' core 待同步 #12 ')).map(chip)).toEqual([['warning', 'inverseText']])
  expect((await exactText(ui, ' 落后 2 ')).map(chip)).toEqual([['warning', 'inverseText']])
  expect((await exactText(ui, ' 未提交改动 ')).map(chip)).toEqual([['warning', 'inverseText']])
  expect((await exactText(ui, ' main ✓ ')).map(one => [one.props.backgroundColor, one.props.dimColor])).toEqual([
    [undefined, true],
    [undefined, true],
  ])
  await ui.unmount()
})

test('轨道每段宽度随面板伸缩，放不下时退回纯文字', async ($, on) => {
  runtimeEngine(on)
  await $.command.run(runCommand)
  const at = async (bodyColumns: number) => {
    const ui = await $.ui.mount({ ...PANE, props: { ...PANE.props, bodyColumns } })
    const seen = {
      row: (await ui.find({ key: 'pipeline-OF-1' }))?.text,
      full: (await exactText(ui, '施工')).length,
      short: (await exactText(ui, '施')).length,
      here: (await exactText(ui, '◉')).length,
    }
    await ui.unmount()
    return seen
  }

  expect(await at(100)).toEqual({ row: 'OF-1●━━━━━●━━━━━◉─────○─────○─────○gate4 分钟', full: 1, short: 0, here: 5 })
  expect(await at(57)).toEqual({ row: 'OF-1●━━●━━◉──○──○──○gate4 分钟', full: 0, short: 1, here: 5 })
  expect(await at(52)).toEqual({ row: 'OF-1●━●━◉─○─○─○gate4 分钟', full: 0, short: 1, here: 5 })
  expect(await at(40)).toEqual({ row: 'OF-1gate4 分钟', full: 0, short: 0, here: 0 })
})

test('头部第二行显示服务状态与构建短 sha', async ($, on) => {
  runtimeEngine(on)
  await $.command.run(runCommand)
  const ui = await $.ui.mount(PANE)
  await ui.press({ key: 'page-models' })

  expect((await exactText(ui, '●')).map(one => one.props.color)).toEqual(['success'])
  expect(await exactText(ui, 'active')).toHaveLength(1)
  expect(await exactText(ui, 'orca-factory-driver')).toHaveLength(0)
  expect(await exactText(ui, '· 006396a')).toHaveLength(1)
  await ui.unmount()
})

test('读取进行中时头部画加载动画，每 100 毫秒换帧，读完后消失', async ($, on) => {
  const { clock } = engine(
    on,
    ran({ stdout: sampleStdout }),
    { status: ran({ stdout: JSON.stringify(SAMPLE_STATUS) }), doctor: ran({ exitCode: 1, stdout: JSON.stringify(SAMPLE_DOCTOR) }) },
    { doctor: 1000 },
  )
  const blits: unknown[] = []
  on('ui.blit', (_, e) => {
    blits.push(e)
    return { value: {} }
  })
  const opened = $.command.run(runCommand)
  await clock.settle()
  const ui = await $.ui.mount(PANE)
  expect((await ui.findAll({ key: 'spin' })).map(one => one.type)).toEqual(['Raster'])

  await clock.advance(100)
  expect(blits).toEqual([{ requestId: 'ziikoo-driver', key: 'spin', cells: 'GSgAAAAAAAEAAAAB' }])

  await clock.advance(1000)
  await opened
  expect(await ui.findAll({ key: 'spin' })).toHaveLength(0)
  expect((await ui.find({ key: 'refresh' }))?.type).toBe('Button')
  await ui.unmount()
})

test('加载动画画不上去时计时器自己停下', async ($, on) => {
  const { clock } = engine(
    on,
    ran({ stdout: sampleStdout }),
    { status: ran({ stdout: JSON.stringify(SAMPLE_STATUS) }), doctor: ran({ exitCode: 1, stdout: JSON.stringify(SAMPLE_DOCTOR) }) },
    { doctor: 1000 },
  )
  let blits = 0
  on('ui.blit', () => {
    blits += 1
    return { value: { deny: 'not mounted' } }
  })
  const opened = $.command.run(runCommand)
  await clock.settle()
  await clock.advance(500)
  expect(blits).toBe(1)

  await clock.advance(500)
  await opened
})

test('趋势行按分钟取最大在途数，画近 30 分钟', async ($, on) => {
  const inFlight = [1, 4, 2]
  const { clock } = engine(on, ran({ stdout: sampleStdout }), {
    status: nth => ran({ stdout: JSON.stringify({ ...SAMPLE_STATUS, tickets: SAMPLE_STATUS.tickets.slice(0, inFlight[nth - 1]) }) }),
    doctor: ran({ exitCode: 1, stdout: JSON.stringify(SAMPLE_DOCTOR) }),
  })
  await $.command.run(runCommand)
  const ui = await $.ui.mount(PANE)
  expect((await ui.find({ key: 'trend' }))?.text).toBe(`趋势${'·'.repeat(29)}█近 30 分钟`)
  expect((await exactText(ui, '·'.repeat(29))).map(one => one.props.dimColor)).toEqual([true])
  expect((await exactText(ui, '█')).map(one => one.props.color)).toEqual(['suggestion'])

  for (const _ of inFlight.slice(1)) {
    await ui.press({ key: 'page-models' })
    await clock.advance(61_000)
    await ui.press({ key: 'page-pipeline' })
  }
  expect((await ui.find({ type: 'Text', text: /^在途/ }))?.text).toBe('在途 2   部署屏障 0   待领 0')
  expect((await ui.find({ key: 'trend' }))?.text).toBe(`趋势${'·'.repeat(27)}▃█▅近 30 分钟`)
  await ui.unmount()
})

test('快照过期时警告；流水线页每 10 秒重新读取 status，离开后停止', async ($, on) => {
  const { calls, clock } = runtimeEngine(on, { ...SAMPLE_STATUS, state: 'stale', generatedAt: '2026-10-27T11:54:00Z' })
  await $.command.run(runCommand)
  const ui = await $.ui.mount(PANE)
  expect((await exactText(ui, '快照 6 分钟前 · 已过期')).map(one => one.props.color)).toEqual(['warning'])

  const statusReads = () => subcommands(calls).filter(one => one === 'status').length
  expect(statusReads()).toBe(1)
  await clock.advance(10_000)
  await clock.settle()
  expect(statusReads()).toBe(2)

  await ui.press({ key: 'page-models' })
  await clock.advance(10_000)
  await clock.settle()
  expect(statusReads()).toBe(2)
  await ui.unmount()
})

test('打开面板默认在流水线页，页签分运行与配置两组', async ($, on) => {
  const { calls } = runtimeEngine(on)
  await $.command.run(runCommand)
  expect(subcommands(calls).sort()).toEqual(['config', 'doctor', 'status'])

  const ui = await $.ui.mount(PANE)
  expect((await ui.find({ key: 'pipeline-OF-1' }))?.text).toBe('OF-1●━━━━━●━━━━━◉─────○─────○─────○gate4 分钟')
  expect((await exactText(ui, '运行')).map(one => [one.props.bold, one.props.dimColor])).toEqual([[true, false]])
  expect((await exactText(ui, '配置')).map(one => [one.props.bold, one.props.dimColor])).toEqual([[false, true]])

  await ui.press({ key: 'page-models' })
  expect((await exactText(ui, '运行')).map(one => [one.props.bold, one.props.dimColor])).toEqual([[false, true]])
  expect((await exactText(ui, '配置')).map(one => [one.props.bold, one.props.dimColor])).toEqual([[true, false]])
  await ui.unmount()
})

test('健康页逐项列出 doctor 检查，不再显示工厂目录与纳管仓库', async ($, on) => {
  runtimeEngine(on)
  await $.command.run(runCommand)
  const ui = await $.ui.mount(PANE)
  await ui.press({ key: 'page-health' })

  expect(await exactText(ui, '006396a')).toHaveLength(1)
  expect((await exactText(ui, 'active')).map(one => one.props.color)).toEqual([undefined, 'success'])
  expect(await exactText(ui, 'doctor 检查')).toHaveLength(1)
  expect((await ui.find({ key: 'check-version' }))?.text).toBe('运行版本 通过 运行版本与 factory origin/main 一致')
  expect((await ui.find({ key: 'check-checkouts' }))?.text).toBe('项目检出 注意 ziikoo-web 检出在 feat/login，不在 main；ansible 有未提交改动')
  expect((await ui.find({ key: 'check-sync_prs' }))?.text).toBe('core 同步 注意 ansible #12 待合并')
  expect((await exactText(ui, ' 注意 ')).map(one => [one.props.backgroundColor, one.props.color])).toEqual([
    ['warning', 'inverseText'],
    ['warning', 'inverseText'],
  ])
  expect((await exactText(ui, ' 通过 ')).map(one => one.props.dimColor)).toEqual([true])
  expect(await exactText(ui, 'driver.toml 的 scheduler.resume_fraction 不被识别，不生效')).toHaveLength(1)
  expect(await exactText(ui, '工厂目录')).toHaveLength(0)
  expect(await exactText(ui, '纳管仓库')).toHaveLength(0)
  await ui.unmount()
})

test('项目发现失败时健康页照常列检查，徽标显示检出与 core 未知', async ($, on) => {
  const unprobed = { status: 'unknown', reason: '项目发现失败，无法列出已接管项目' }
  runtimeEngine(on, SAMPLE_STATUS, {
    checks: { checkouts: unprobed, ledger: unprobed, sync_prs: unprobed, version: SAMPLE_DOCTOR.checks.version },
    errors: ['checkouts: 项目发现失败，无法列出已接管项目'],
  })
  await $.command.run(runCommand)
  const ui = await $.ui.mount(PANE)

  expect((await exactText(ui, ' 检出未知 ')).map(one => one.props.backgroundColor)).toEqual(['warning', 'warning', 'warning'])
  expect((await exactText(ui, ' core 未知 ')).map(one => one.props.backgroundColor)).toEqual(['warning', 'warning', 'warning'])

  await ui.press({ key: 'page-health' })
  expect((await ui.find({ key: 'check-checkouts' }))?.text).toBe('项目检出 未知 项目发现失败，无法列出已接管项目')
  expect((await ui.find({ key: 'check-version' }))?.text).toBe('运行版本 通过 运行版本与 factory origin/main 一致')
  expect((await exactText(ui, ' 未知 ')).map(one => one.props.backgroundColor)).toEqual(['warning', 'warning', 'warning'])
  await ui.unmount()
})

test('健康页也每 10 秒重新读取 status', async ($, on) => {
  const { calls, clock } = runtimeEngine(on)
  await $.command.run(runCommand)
  const ui = await $.ui.mount(PANE)
  await ui.press({ key: 'page-health' })
  expect(await exactText(ui, '快照 8 秒前')).toHaveLength(1)

  const statusReads = () => subcommands(calls).filter(one => one === 'status').length
  const before = statusReads()
  await clock.advance(10_000)
  await clock.settle()
  expect(statusReads()).toBe(before + 1)
  await ui.unmount()
})

test('记住的页是健康页时，打开面板读取 config、status、doctor', async ($, on) => {
  on('state.get', { plugin: 'ziikoo-driver', key: 'page' }, () => ({
    value: { value: { shape: 'page-2', value: 'health' }, version: 1 },
  }))
  const { calls } = runtimeEngine(on)
  await $.command.run(runCommand)
  expect(subcommands(calls).sort()).toEqual(['config', 'doctor', 'status'])
})

test('健康页的检查按文档顺序排列，不按键名', async ($, on) => {
  runtimeEngine(on)
  await $.command.run(runCommand)
  const ui = await $.ui.mount(PANE)
  await ui.press({ key: 'page-health' })

  const labels = new Set(['运行版本', '项目检出', 'core 同步'])
  const shown = (await ui.findAll({ type: 'Text' })).map(one => one.text).filter(text => labels.has(text))
  expect(shown).toEqual(['运行版本', '项目检出', 'core 同步'])
  await ui.unmount()
})

test('环境页最上面是工厂目录', async ($, on) => {
  engine(on, ran({ stdout: sampleStdout }))
  await $.command.run(runCommand)
  const ui = await $.ui.mount(PANE)
  await ui.press({ key: 'page-environment' })

  expect(await exactText(ui, '工厂目录')).toHaveLength(1)
  expect(await exactText(ui, '/home/laoxu/github/orca-factory')).toHaveLength(1)
  await ui.unmount()
})

test('右上角在配置组显示 config 取数时间，在运行组显示快照新鲜度', async ($, on) => {
  runtimeEngine(on)
  await $.command.run(runCommand)
  const ui = await $.ui.mount(PANE)
  expect(await ui.findAll({ type: 'Text', text: /^取数于/ })).toHaveLength(0)

  await ui.press({ key: 'page-scheduler' })
  const fetched = await ui.findAll({ type: 'Text', text: /^取数于/ })
  expect(fetched.map(one => one.props.dimColor)).toEqual([true])
  expect(fetched[0]?.text).toMatch(/^取数于 \d{2}:\d{2}:\d{2}$/)
  expect(await exactText(ui, '快照 8 秒前')).toHaveLength(0)
  await ui.unmount()
})

test('旧版本存下的概览页按初始值读，打开后在流水线页', async ($, on) => {
  on('state.get', { plugin: 'ziikoo-driver', key: 'page' }, () => ({
    value: { value: { shape: 'page-1', value: 'overview' }, version: 1 },
  }))
  runtimeEngine(on)
  await $.command.run(runCommand)
  const ui = await $.ui.mount(PANE)

  expect((await ui.find({ key: 'pipeline-OF-1' }))?.text).toBe('OF-1●━━━━━●━━━━━◉─────○─────○─────○gate4 分钟')
  await ui.unmount()
})

test('徽标放得下时画在框线上，放不下时退回框内第一行', async ($, on) => {
  runtimeEngine(on)
  await $.command.run(runCommand)
  const at = async (bodyColumns: number) => {
    const ui = await $.ui.mount({ ...PANE, props: { ...PANE.props, bodyColumns } })
    const edge = async (name: string) => {
      const found = await ui.find({ key: `edge-${name}` })
      return found && { text: found.text, position: found.props.position, top: found.props.top, right: found.props.right }
    }
    const seen = {
      edge: { factory: await edge('orca-factory'), ansible: await edge('ansible') },
      inside: (await ui.find({ key: 'badges-ansible' }))?.text,
    }
    await ui.unmount()
    return seen
  }

  const factory = { text: ' main ✓  core ✓ ', position: 'absolute', top: 0, right: 2 }
  expect(await at(100)).toEqual({
    edge: { factory, ansible: { text: ' main ✓  落后 2   未提交改动   core 待同步 #12 ', position: 'absolute', top: 0, right: 2 } },
    inside: ' main ✓  落后 2   未提交改动   core 待同步 #12 ',
  })
  expect(await at(52)).toEqual({
    edge: { factory, ansible: undefined },
    inside: ' main ✓  落后 2   未提交改动   core 待同步 #12 ',
  })
})

test('读取中的徽标是框线上的灰字「检查中…」', async ($, on) => {
  const { clock } = engine(
    on,
    ran({ stdout: sampleStdout }),
    { status: ran({ stdout: JSON.stringify(SAMPLE_STATUS) }), doctor: ran({ exitCode: 1, stdout: JSON.stringify(SAMPLE_DOCTOR) }) },
    { doctor: 1000 },
  )
  on('ui.blit', () => ({ value: {} }))
  const opened = $.command.run(runCommand)
  await clock.settle()
  const ui = await $.ui.mount(PANE)
  expect((await ui.find({ key: 'edge-orca-factory' }))?.text).toBe(' 检查中… ')
  expect((await exactText(ui, ' 检查中… ')).map(one => one.props.dimColor)).toEqual([true, true, true])

  await clock.advance(1000)
  await opened
  expect(await exactText(ui, ' 检查中… ')).toHaveLength(0)
  expect((await ui.find({ key: 'edge-orca-factory' }))?.text).toBe(' main ✓  core ✓ ')
  await ui.unmount()
})

test('调度页窄到原键名不足 14 列时整格不画', async ($, on) => {
  engine(on, ran({ stdout: sampleStdout }))
  await $.command.run(runCommand)
  const row = async (bodyColumns: number) => {
    const ui = await $.ui.mount({ ...PANE, props: { ...PANE.props, bodyColumns } })
    await ui.press({ key: 'page-scheduler' })
    const text = (await ui.find({ key: 'setting-max_in_flight' }))?.text
    await ui.unmount()
    return text
  }

  expect(await row(52)).toBe('同时运行的票最多 4 张driver.toml')
  expect(await row(100)).toBe('同时运行的票最多 4 张driver.tomlmax_in_flight')
})

test('上次中断留下的读取种类在同类读取完成后清除', async ($, on) => {
  const BUSY = { plugin: 'ziikoo-driver', key: 'busy' } as const
  let stored: { value: unknown; version: number } = { value: { shape: 'busy-1', value: ['doctor'] }, version: 1 }
  on('state.get', BUSY, () => ({ value: stored }))
  on('state.set', BUSY, (_, e) => {
    if (e.ifVersion !== undefined && e.ifVersion !== stored.version) return { value: { isSet: false, version: stored.version } }
    stored = { value: e.value, version: stored.version + 1 }
    return { value: { isSet: true, version: stored.version } }
  })
  runtimeEngine(on)
  const ui = await $.ui.mount(PANE)
  expect((await ui.findAll({ key: 'spin' })).map(one => one.type)).toEqual(['Raster'])

  await $.command.run(runCommand)
  expect(await ui.findAll({ key: 'spin' })).toHaveLength(0)
  expect((await ui.find({ key: 'refresh' }))?.type).toBe('Button')
  await ui.unmount()
})

const logsEngine = (on: On, status: unknown = SAMPLE_STATUS) =>
  engine(on, ran({ stdout: sampleStdout }), {
    status: ran({ stdout: JSON.stringify(status) }),
    doctor: ran({ exitCode: 1, stdout: JSON.stringify(SAMPLE_DOCTOR) }),
    bash: ran({ stdout: SAMPLE_LOGS }),
  })

type Drawing = { findAll: (q: { type: string }) => Promise<FoundElement[]> }

const timelineRows = async (ui: Drawing) =>
  (await ui.findAll({ type: 'Box' })).filter(one => one.key?.startsWith('timeline-')).map(one => one.text)

const ticketRows = async (ui: Drawing) =>
  (await ui.findAll({ type: 'Box' })).filter(one => one.key?.startsWith('pipeline-')).map(one => one.key)

const HELP = {
  list: '↑↓ 选择 · ⏎ 详情 · f 筛选 · r 刷新 · 1–5 切页 · Esc 回到输入框',
  detail: 'q 返回 · r 刷新 · 1–5 切页 · Esc 回到输入框',
  environment: 'f 筛选 · r 刷新 · 1–5 切页 · Esc 回到输入框',
  other: 'r 刷新 · 1–5 切页 · Tab/↑↓ 移动 · Esc 回到输入框',
}

test('按下票号进入详情，时间线按执行器日志列出中文事件', async ($, on) => {
  const { calls } = logsEngine(on)
  await $.command.run(runCommand)
  const ui = await $.ui.mount(PANE)
  expect(await ticketRows(ui)).toEqual(['pipeline-OF-1', 'pipeline-OF-2', 'pipeline-OF-3', 'pipeline-ZW-7', 'pipeline-ZW-8'])

  await ui.press({ key: 'ticket-OF-1' })
  expect(await ticketRows(ui)).toEqual([])
  expect((await ui.find({ key: 'detail-back' }))?.props.hotkey).toBe('q')
  expect(await exactText(ui, 'OF-1')).toHaveLength(1)
  expect(await exactText(ui, ' 流水线冒烟（OF）')).toHaveLength(1)
  expect((await exactText(ui, 'gate')).map(one => one.props.color)).toEqual(['suggestion'])
  expect(await exactText(ui, '4 分钟')).toHaveLength(1)
  expect(await exactText(ui, '执行器')).toHaveLength(1)
  expect(await exactText(ui, 'orca-factory')).toHaveLength(1)

  const rows = await timelineRows(ui)
  expect(rows.map(row => row.slice(8))).toEqual([
    '执行器启动',
    '认领',
    'worker 失败',
    'correction 已写好',
    'worker 完成',
    '分支检查通过',
    'gate 通过',
    '审查通过',
    '开 PR',
    '进入排队合并',
    '执行器退出',
  ])
  for (const row of rows) expect(row.slice(0, 8)).toMatch(/^\d{2}:\d{2}:\d{2}$/)
  expect((await exactText(ui, 'worker 失败')).map(one => one.props.color)).toEqual(['warning'])
  expect((await exactText(ui, '认领')).map(one => one.props.color)).toEqual([undefined])

  const logs = calls.filter(argv => argv[0] === 'bash')
  expect(logs).toHaveLength(1)
  expect(logs[0]?.at(-1)).toBe('OF-1')
  expect(logs[0]?.[2]).toContain('orca-factory-driver logs "$1" --json')
  expect(logs[0]?.[2]).not.toContain('OF-1')
  await ui.unmount()
})

test('详情里按返回或 q 回到列表，切页再回来也是列表', async ($, on) => {
  logsEngine(on)
  await $.command.run(runCommand)
  const ui = await $.ui.mount(PANE)

  await ui.press({ key: 'ticket-OF-2' })
  expect(await ticketRows(ui)).toEqual([])
  await ui.press({ key: 'detail-back' })
  expect((await ticketRows(ui)).length).toBe(5)
  expect(await ui.findAll({ key: 'detail-back' })).toHaveLength(0)

  await ui.press({ key: 'ticket-ZW-8' })
  expect((await ui.find({ key: 'detail-back' }))?.type).toBe('Button')
  await ui.press({ key: 'page-models' })
  await ui.press({ key: 'page-pipeline' })
  expect((await ticketRows(ui)).length).toBe(5)
  expect(await ui.findAll({ key: 'detail-back' })).toHaveLength(0)
  await ui.unmount()
})

test('详情里按 r 重新读取时间线', async ($, on) => {
  const { calls } = logsEngine(on)
  await $.command.run(runCommand)
  const ui = await $.ui.mount(PANE)
  await ui.press({ key: 'ticket-OF-1' })
  await ui.press({ key: 'refresh' })
  expect(calls.filter(argv => argv[0] === 'bash').map(argv => argv.at(-1))).toEqual(['OF-1', 'OF-1'])
  await ui.unmount()
})

test('不在在途列表的票照常显示时间线；超过 15 条时折叠更早的', async ($, on) => {
  const lines = SAMPLE_LOGS.trim().split('\n')
  const many = Array.from({ length: 20 }, (_, i) =>
    JSON.stringify({ time: `2026-10-27T20:${String(10 + i).padStart(2, '0')}:00+08:00`, msg: 'transition', event: 'tick', from: 'gate', to: 'gate' }),
  )
  engine(on, ran({ stdout: sampleStdout }), {
    status: nth => ran({ stdout: JSON.stringify(nth === 1 ? SAMPLE_STATUS : { ...SAMPLE_STATUS, tickets: [] }) }),
    doctor: ran({ exitCode: 1, stdout: JSON.stringify(SAMPLE_DOCTOR) }),
    bash: ran({ stdout: [...many, ...lines.slice(0, 2)].join('\n') }),
  })
  await $.command.run(runCommand)
  const ui = await $.ui.mount(PANE)
  await ui.press({ key: 'ticket-OF-1' })
  await ui.press({ key: 'refresh' })

  expect((await exactText(ui, '已不在在途列表')).map(one => one.props.dimColor)).toEqual([true])
  expect((await exactText(ui, '更早 6 条')).map(one => one.props.dimColor)).toEqual([true])
  const rows = await timelineRows(ui)
  expect(rows).toHaveLength(15)
  expect(rows.at(-1)?.slice(8)).toBe('执行器启动')
  expect(rows[0]?.slice(8)).toBe('定时检查')
  await ui.unmount()
})

test('没有执行器日志时说明；grep 无匹配的退出码 1 不算失败', async ($, on) => {
  engine(on, ran({ stdout: sampleStdout }), {
    status: ran({ stdout: JSON.stringify(SAMPLE_STATUS) }),
    doctor: ran({ exitCode: 1, stdout: JSON.stringify(SAMPLE_DOCTOR) }),
    bash: ran({ exitCode: 1 }),
  })
  await $.command.run(runCommand)
  const ui = await $.ui.mount(PANE)
  await ui.press({ key: 'ticket-OF-1' })
  expect((await exactText(ui, '没有执行器日志')).map(one => one.props.dimColor)).toEqual([true])
  expect(await ui.findAll({ type: 'Text', text: /取数失败/ })).toHaveLength(0)
  await ui.unmount()
})

test('票号格式不对时不运行 bash，详情说明原因', async ($, on) => {
  const bad = { repo: '/home/laoxu/github/orca-factory', identifier: 'OF-1; rm -rf x', owner: '执行器', phase: 'gate' }
  const { calls } = logsEngine(on, { ...SAMPLE_STATUS, tickets: [...SAMPLE_STATUS.tickets, bad] })
  await $.command.run(runCommand)
  const ui = await $.ui.mount(PANE)

  await ui.press({ key: 'ticket-OF-1; rm -rf x' })
  expect(await exactText(ui, '票号格式不对')).toHaveLength(1)
  expect(calls.filter(argv => argv[0] === 'bash')).toHaveLength(0)

  await ui.press({ key: 'detail-back' })
  await ui.press({ key: 'ticket-OF-1' })
  expect(await exactText(ui, '票号格式不对')).toHaveLength(0)
  expect(calls.filter(argv => argv[0] === 'bash')).toHaveLength(1)
  await ui.unmount()
})

test('流水线按票号或标题筛选，没有匹配的项目写无匹配', async ($, on) => {
  logsEngine(on)
  await $.command.run(runCommand)
  const ui = await $.ui.mount(PANE)
  const field = await ui.find({ key: 'filter-input' })
  expect(field?.props.placeholder).toBe('票号或标题')
  expect(field?.props.submitLabel).toBe('确定')
  expect(field?.props.value).toBe('')
  expect(field?.props.autoFocus).toBe(undefined)
  expect((await ui.find({ key: 'filter' }))?.props.label).toBe('筛选')

  await ui.input({ key: 'filter-input', text: 'zw', kind: 'change' })
  expect(await ticketRows(ui)).toEqual(['pipeline-ZW-7', 'pipeline-ZW-8'])
  expect(await exactText(ui, '无匹配')).toHaveLength(2)

  await ui.input({ key: 'filter-input', text: '冒烟' })
  expect((await ui.find({ key: 'filter-input' }))?.props.value).toBe('冒烟')
  expect(await ticketRows(ui)).toEqual(['pipeline-OF-1'])
  expect((await ui.find({ key: 'filter' }))?.props.label).toBe('筛选')

  await ui.input({ key: 'filter-input', text: '' })
  expect((await ticketRows(ui)).length).toBe(5)
  expect(await exactText(ui, '无匹配')).toHaveLength(0)
  await ui.unmount()
})

test('打开面板、从详情返回、筛选提交后焦点落在票上，10 秒轮询不移动焦点', async ($, on) => {
  const { clock, focused } = logsEngine(on)
  await $.command.run(runCommand)
  const ui = await $.ui.mount(PANE)
  expect(focused).toEqual(['ticket-OF-1'])

  await clock.advance(10_000)
  await clock.settle()
  expect(focused).toEqual(['ticket-OF-1'])

  await ui.press({ key: 'ticket-OF-3' })
  await ui.press({ key: 'detail-back' })
  expect(focused).toEqual(['ticket-OF-1', 'ticket-OF-3'])

  await ui.press({ key: 'filter' })
  expect(focused).toEqual(['ticket-OF-1', 'ticket-OF-3', 'filter-input'])
  await ui.input({ key: 'filter-input', text: 'ZW', kind: 'change' })
  expect(focused).toEqual(['ticket-OF-1', 'ticket-OF-3', 'filter-input'])
  await ui.input({ key: 'filter-input', text: 'ZW' })
  expect(focused).toEqual(['ticket-OF-1', 'ticket-OF-3', 'filter-input', 'ticket-ZW-7'])
  await ui.unmount()
})

test('环境页按变量名筛选，没有匹配的分组写无匹配；切页后清除', async ($, on) => {
  const { focused } = logsEngine(on)
  await $.command.run(runCommand)
  const ui = await $.ui.mount(PANE)
  await ui.press({ key: 'page-environment' })
  expect((await ui.find({ key: 'filter-input' }))?.props.placeholder).toBe('变量名')
  expect((await ui.find({ key: 'filter' }))?.props.label).toBe('筛选')

  await ui.press({ key: 'filter' })
  expect(focused).toEqual(['ticket-OF-1', 'filter-input'])
  await ui.input({ key: 'filter-input', text: 'proxy', kind: 'change' })
  expect((await ui.find({ key: 'env-HTTPS_PROXY' }))?.text).toBe('HTTPS_PROXYhttp://127.0.0.1:7890')
  expect(await ui.findAll({ key: 'env-gh' })).toHaveLength(0)
  await ui.input({ key: 'filter-input', text: 'proxy' })
  expect(focused).toEqual(['ticket-OF-1', 'filter-input'])
  expect((await ui.find({ key: 'env-HTTPS_PROXY' }))?.text).toBe('HTTPS_PROXYhttp://127.0.0.1:7890')
  expect(await ui.findAll({ key: 'env-gh' })).toHaveLength(0)
  expect(await exactText(ui, '工具路径')).toHaveLength(1)
  expect(await exactText(ui, '无匹配')).toHaveLength(2)

  await ui.press({ key: 'page-models' })
  await ui.press({ key: 'page-environment' })
  expect((await ui.find({ key: 'env-gh' }))?.text).toBe('gh/usr/bin/gh')
  expect(await exactText(ui, '无匹配')).toHaveLength(0)
  expect((await ui.find({ key: 'filter-input' }))?.props.value).toBe('')
  await ui.unmount()
})

test('帮助条随视图变化', async ($, on) => {
  logsEngine(on)
  await $.command.run(runCommand)
  const ui = await $.ui.mount(PANE)
  const help = async () => (await ui.findAll({ type: 'Text' })).filter(one => Object.values(HELP).includes(one.text)).map(one => one.text)

  expect(await help()).toEqual([HELP.list])
  await ui.press({ key: 'ticket-OF-1' })
  expect(await help()).toEqual([HELP.detail])
  await ui.press({ key: 'page-environment' })
  expect(await help()).toEqual([HELP.environment])
  await ui.press({ key: 'page-scheduler' })
  expect(await help()).toEqual([HELP.other])
  await ui.unmount()
})

test('doctor 还没读完时焦点已落在第一张票上', async ($, on) => {
  const { clock, focused } = engine(
    on,
    ran({ stdout: sampleStdout }),
    { status: ran({ stdout: JSON.stringify(SAMPLE_STATUS) }), doctor: ran({ exitCode: 1, stdout: JSON.stringify(SAMPLE_DOCTOR) }) },
    { doctor: 10_000 },
  )
  on('ui.blit', () => ({ value: {} }))
  const opened = $.command.run(runCommand)
  await clock.settle()
  expect(focused).toEqual(['ticket-OF-1'])

  await clock.advance(10_000)
  await opened
  expect(focused).toEqual(['ticket-OF-1'])
})
