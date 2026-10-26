import { atom, read, update } from 'claude-code'
import type { ElementTable, EngineInterface, Register } from 'claude-code'

import type { DriverConfig, EnvEntry, Page, Repo, SchedulerSetting, Snapshot } from '../types'
import { ROLE_LABELS, SETTINGS, SETTING_GROUPS, SOURCE_LABELS, parseConfig, routingSummary } from './config'

const PANE = 'ziikoo-driver'
const TITLE = 'Driver 配置'
const ARGV = ['orca-factory-driver', 'config', '--json']
const STDERR_LINES = 6
const TOOL_KEYS = new Set(['go', 'gh', 'git', 'jq', 'claude', 'codex'])

const PAGES: { id: Page; label: string; hotkey: string }[] = [
  { id: 'overview', label: '概览', hotkey: '1' },
  { id: 'models', label: '模型', hotkey: '2' },
  { id: 'scheduler', label: '调度', hotkey: '3' },
  { id: 'environment', label: '环境', hotkey: '4' },
]

const snapshot = atom({ plugin: 'ziikoo-driver', key: 'snapshot' } as const, { kind: 'loading' }, {
  shape: 'snapshot-2',
})
const page = atom({ plugin: 'ziikoo-driver', key: 'page' } as const, 'overview', { shape: 'page-1' })
const repo = atom({ plugin: 'ziikoo-driver', key: 'repo' } as const, '', { shape: 'repo-1' })

type Engine = Pick<EngineInterface, 'state' | 'process' | 'clock'>
type Kit = Pick<ElementTable, 'Box' | 'Text' | 'Button'>

const message = (error: unknown) => (error instanceof Error ? error.message : String(error))

const parsed = (stdout: string): DriverConfig | string => {
  try {
    return parseConfig(stdout)
  } catch (error) {
    return message(error)
  }
}

const take = async ($: Engine): Promise<Snapshot> => {
  const run = await $.process.run(ARGV).catch((error: unknown) => message(error))
  if (typeof run === 'string') return { kind: 'failed', exitCode: null, stderr: run }
  if (run.exitCode !== 0) return { kind: 'failed', exitCode: run.exitCode, stderr: run.stderr }
  const config = parsed(run.stdout)
  if (typeof config === 'string') return { kind: 'failed', exitCode: 0, stderr: config }
  return { kind: 'ready', config, fetchedAt: await $.clock.now() }
}

const fetchConfig = async ($: Engine): Promise<void> => {
  const next = await take($)
  await update($, snapshot, () => next)
}

const refresh = async ($: Engine): Promise<void> => {
  await update($, snapshot, () => ({ kind: 'loading' }))
  await fetchConfig($)
}

const columns = (cols: number, fixed: number, share: number, max: number) => {
  const rest = Math.max(0, cols - fixed)
  const flexible = Math.max(6, Math.min(max, Math.floor(rest * share)))
  return { flexible, rest: Math.max(6, rest - flexible) }
}

const cell = (ui: Kit, width: number, value: string, dimColor = false, color?: string) => (
  <ui.Box width={width} flexShrink={0}>
    <ui.Text wrap="truncate" dimColor={dimColor} color={color}>
      {value}
    </ui.Text>
  </ui.Box>
)

const field = (ui: Kit, label: string, value: string, color?: string) => (
  <ui.Box flexDirection="row">
    {cell(ui, 12, label, true)}
    <ui.Text wrap="truncate" color={color}>
      {value}
    </ui.Text>
  </ui.Box>
)

const clockTime = (ms: number) => {
  const at = new Date(ms)
  return [at.getHours(), at.getMinutes(), at.getSeconds()].map(n => String(n).padStart(2, '0')).join(':')
}

const overviewPage = (ui: Kit, config: DriverConfig, fetchedAt: number) => {
  const { overview, problems } = config
  return (
    <ui.Box flexDirection="column">
      {field(ui, '构建', overview.build.slice(0, 7))}
      {field(ui, '服务', overview.service, overview.service === 'active' ? 'success' : 'warning')}
      {field(ui, '维护模式', overview.maintenance, overview.maintenance === 'running' ? undefined : 'warning')}
      {field(ui, '状态快照', overview.snapshot)}
      {field(ui, '配额暂停', overview.globalPause ? `暂停中 ${overview.pauseSeconds} 秒` : '未暂停', overview.globalPause ? 'warning' : undefined)}
      {field(ui, '纳管仓库', `${config.repos.length} 个`)}
      {field(ui, '工厂目录', overview.factoryRoot || '未配置')}
      {field(ui, '取数时间', clockTime(fetchedAt))}
      <ui.Text bold>问题</ui.Text>
      {problems.length === 0 && <ui.Text dimColor>无</ui.Text>}
      {problems.map(problem => (
        <ui.Text color="warning">{problem}</ui.Text>
      ))}
    </ui.Box>
  )
}

const modelsPage = (ui: Kit, current: Repo | undefined, cols: number) => {
  if (!current) return <ui.Text dimColor>没有纳管仓库</ui.Text>
  const { flexible: model, rest: key } = columns(cols, 13 + 8 + 7, 0.65, 24)
  const row = (role: string, agent: string, name: string, effort: string, source: string, dim = false) => (
    <ui.Box flexDirection="row">
      {cell(ui, 13, role, dim)}
      {cell(ui, 8, agent, dim)}
      {cell(ui, model, name, dim)}
      {cell(ui, 7, effort, dim)}
      {cell(ui, key, source, dim)}
    </ui.Box>
  )
  return (
    <ui.Box flexDirection="column">
      {row('角色', 'Agent', '模型', '强度', '来源键', true)}
      {current.roles.map(one =>
        row(ROLE_LABELS[one.role], one.agent || '-', one.model || '默认', one.effort || '-', one.key || '内置'),
      )}
      <ui.Text wrap="truncate">{routingSummary(current.routing)}</ui.Text>
    </ui.Box>
  )
}

const settingRow = (ui: Kit, one: SchedulerSetting, keyWidth: number) => {
  const view = SETTINGS[one.key]
  const changed = one.source === 'driver.toml'
  return (
    <ui.Box key={`setting-${one.key}`} flexDirection="row" marginLeft={2}>
      {cell(ui, 18, view.label)}
      {cell(ui, 14, view.show(one.value))}
      {cell(ui, 13, SOURCE_LABELS[one.source], !changed, changed ? 'suggestion' : undefined)}
      {cell(ui, keyWidth, one.key, true)}
    </ui.Box>
  )
}

const schedulerPage = (ui: Kit, settings: SchedulerSetting[], cols: number) => {
  const keyWidth = Math.max(6, cols - 2 - 18 - 14 - 13)
  return (
    <ui.Box flexDirection="column">
      {SETTING_GROUPS.map(group => (
        <ui.Box flexDirection="column">
          <ui.Text bold>{group}</ui.Text>
          {settings.filter(one => SETTINGS[one.key].group === group).map(one => settingRow(ui, one, keyWidth))}
        </ui.Box>
      ))}
    </ui.Box>
  )
}

const envValue = (entry: EnvEntry) => {
  if (entry.secret) return entry.set ? '已设置' : '未设置'
  if (!entry.set) return TOOL_KEYS.has(entry.key) ? '未找到' : '未设置'
  return entry.value ?? '已设置'
}

const envGroup = (entry: EnvEntry) => {
  if (entry.source === 'driver.env') return 'driver.env'
  return TOOL_KEYS.has(entry.key) ? '工具路径' : '服务环境与代理'
}

const environmentPage = (ui: Kit, entries: EnvEntry[], cols: number) => {
  const groups = ['driver.env', '服务环境与代理', '工具路径']
  const width = Math.min(30, Math.max(10, cols - 20))
  return (
    <ui.Box flexDirection="column">
      {groups.map(group => (
        <ui.Box flexDirection="column" marginBottom={1}>
          <ui.Text bold>{group}</ui.Text>
          {entries
            .filter(entry => envGroup(entry) === group)
            .map(entry => (
              <ui.Box key={`env-${entry.key}`} flexDirection="row">
                {cell(ui, width, entry.key)}
                <ui.Text wrap="truncate" dimColor={!entry.set}>
                  {envValue(entry)}
                </ui.Text>
              </ui.Box>
            ))}
        </ui.Box>
      ))}
    </ui.Box>
  )
}

const failureTitle = (exitCode: number | null) => {
  if (exitCode === null) return '无法运行 orca-factory-driver'
  return exitCode === 0 ? '输出无法解析' : `取数失败（退出码 ${exitCode}）`
}

const failedView = (ui: Kit, state: Extract<Snapshot, { kind: 'failed' }>) => (
  <ui.Box flexDirection="column">
    <ui.Text color="error">{failureTitle(state.exitCode)}</ui.Text>
    {state.stderr
      .split('\n')
      .filter(line => line.trim() !== '')
      .slice(0, STDERR_LINES)
      .map(line => (
        <ui.Text dimColor wrap="truncate">
          {line}
        </ui.Text>
      ))}
  </ui.Box>
)

export const register: Register = on => {
  on('session.start', async ($, e, next) => {
    await $.command.register({
      name: 'ziikoo-driver',
      description: '打开 Driver 配置面板（概览、模型、调度、环境）',
      immediate: true,
    })
    return next(e)
  })

  on('command.run', { command: 'ziikoo-driver' }, async $ => {
    await $.ui.open({ id: PANE, title: TITLE, focus: true })
    await fetchConfig($)
    return { text: '已打开 Driver 配置面板。' }
  })

  on('ui.render', { component: 'Pane', requestId: PANE }, async ($, e) => {
    const resolved = $.ui.resolve(e)
    const ui: Kit = resolved
    const state = await read($, snapshot)
    const current = await read($, page)
    const chosen = await read($, repo)
    const cols = e.props.bodyColumns
    const config = state.kind === 'ready' ? state.config : undefined
    const selected = config?.repos.find(one => one.name === chosen) ?? config?.repos[0]

    return (
      <ui.Box flexDirection="column" width={cols}>
        <ui.Box flexDirection="row" justifyContent="space-between">
          <ui.Box flexDirection="row" gap={1} flexShrink={0}>
            {PAGES.map(one => {
              const press = () => update($, page, () => one.id)
              return one.id === current ? (
                <ui.Button key={`page-${one.id}`} label={one.label} hotkey={one.hotkey} variant="primary" onPress={press} />
              ) : (
                <ui.Button key={`page-${one.id}`} label={one.label} hotkey={one.hotkey} plain onPress={press} />
              )
            })}
          </ui.Box>
          <ui.Box flexDirection="row" gap={1} marginLeft={1}>
            {config && <ui.Text dimColor wrap="truncate">{`${config.overview.build.slice(0, 7)} · ${config.overview.service}`}</ui.Text>}
            <ui.Button key="refresh" label="↻ 刷新" hotkey="r" onPress={() => refresh($)} />
          </ui.Box>
        </ui.Box>
        {config && current === 'models' && selected && (
          <ui.Box flexDirection="row" gap={1}>
            <ui.Text dimColor>仓库</ui.Text>
            {'Select' in resolved ? (
              <resolved.Select
                key="repo"
                value={selected.name}
                options={config.repos.map(one => ({ value: one.name, label: one.name }))}
                onSelect={value => update($, repo, () => value)}
              />
            ) : (
              <ui.Text>{selected.name}</ui.Text>
            )}
          </ui.Box>
        )}
        <ui.Box marginTop={1} flexDirection="column">
          {state.kind === 'loading' && <ui.Text dimColor>正在读取 orca-factory-driver config --json …</ui.Text>}
          {state.kind === 'failed' && failedView(ui, state)}
          {state.kind === 'ready' && current === 'overview' && overviewPage(ui, state.config, state.fetchedAt)}
          {state.kind === 'ready' && current === 'models' && modelsPage(ui, selected, cols)}
          {state.kind === 'ready' && current === 'scheduler' && schedulerPage(ui, state.config.scheduler, cols)}
          {state.kind === 'ready' && current === 'environment' && environmentPage(ui, state.config.environment, cols)}
        </ui.Box>
        <ui.Text dimColor wrap="truncate">1–4 切页 · r 刷新 · Tab 切换按钮 · Esc 回到输入框</ui.Text>
      </ui.Box>
    )
  })
}
