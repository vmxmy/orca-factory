import { expect, mock, test } from 'claude-code/testing'
import type { FoundElement } from 'claude-code/testing'
import type { On, ProcessRunResult } from 'claude-code'

import { SAMPLE_CONFIG } from './sample-config'

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

const engine = (on: On, result: ProcessRunResult): string[][] => {
  const calls: string[][] = []
  mock.clock(on)
  on('command.register', () => ({ value: { command: 'ziikoo-driver' } }))
  on('ui.open', () => ({ value: { isPlaced: true } }))
  on('process.run', (_, e) => {
    calls.push([...e.argv])
    return { value: result }
  })
  return calls
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
  const calls = engine(on, ran({ stdout: sampleStdout }))
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

test('调度页 default 来源的行是灰色', async ($, on) => {
  engine(on, ran({ stdout: sampleStdout }))
  await $.command.run(runCommand)
  const ui = await $.ui.mount(PANE)
  await ui.press({ key: 'page-scheduler' })

  const rows = await Promise.all(SAMPLE_CONFIG.scheduler.map(one => exactText(ui, one.key)))
  expect(rows.map(found => found.map(one => one.props.dimColor))).toEqual([
    [false], [false], [true], [true], [false], [true], [false],
    [false], [true], [false], [true], [true], [false], [true],
  ])
  expect((await exactText(ui, '默认')).map(one => one.props.dimColor)).toEqual([true, true, true, true, true, true, true])
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
