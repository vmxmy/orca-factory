import { atom, read, update } from 'claude-code'
import type { ElementTable, EngineInterface, Register, Timer } from 'claude-code'

import type { DriverConfig, EnvEntry, Fetched, Group, Health, Page, Repo, Runtime, RuntimeTicket, SchedulerSetting, Snapshot } from '../types'
import { ROLE_LABELS, SETTINGS, SETTING_GROUPS, SOURCE_LABELS, deploys, parseConfig, routingSummary } from './config'
import { parseHealth, parseRuntime } from './runtime'

const PANE = 'ziikoo-driver'
const TITLE = 'Driver'
const ARGV = ['orca-factory-driver', 'config', '--json']
const RUNTIME_ARGV = ['orca-factory-driver', 'status', '--json']
const HEALTH_ARGV = ['orca-factory-driver', 'doctor', '--json']
const POLL_MS = 10_000
const WIDE_COLUMNS = 80
const STDERR_LINES = 6
const TOOL_KEYS = new Set(['go', 'gh', 'git', 'jq', 'claude', 'codex'])

const GROUPS: { id: Group; label: string; pages: { id: Page; label: string; hotkey: string }[] }[] = [
  {
    id: 'run',
    label: '运行',
    pages: [
      { id: 'pipeline', label: '流水线', hotkey: '1' },
      { id: 'health', label: '健康', hotkey: '2' },
    ],
  },
  {
    id: 'config',
    label: '配置',
    pages: [
      { id: 'models', label: '模型', hotkey: '3' },
      { id: 'scheduler', label: '调度', hotkey: '4' },
      { id: 'environment', label: '环境', hotkey: '5' },
    ],
  },
]

const groupOf = (current: Page): Group => {
  const found = GROUPS.find(one => one.pages.some(tab => tab.id === current))
  if (!found) throw new Error(`页 ${current} 不在任何页签组里`)
  return found.id
}

const snapshot = atom({ plugin: 'ziikoo-driver', key: 'snapshot' } as const, { kind: 'loading' }, {
  shape: 'snapshot-3',
})
const page = atom({ plugin: 'ziikoo-driver', key: 'page' } as const, 'pipeline', { shape: 'page-2' })
const repo = atom({ plugin: 'ziikoo-driver', key: 'repo' } as const, '', { shape: 'repo-1' })
const runtime = atom({ plugin: 'ziikoo-driver', key: 'runtime' } as const, { kind: 'loading' }, { shape: 'runtime-1' })
const health = atom({ plugin: 'ziikoo-driver', key: 'health' } as const, { kind: 'loading' }, { shape: 'health-1' })

type Engine = Pick<EngineInterface, 'state' | 'process' | 'clock'>
type Poller = Engine & Pick<EngineInterface, 'ui'>
type Kit = Pick<ElementTable, 'Box' | 'Text' | 'Button'>

const message = (error: unknown) => (error instanceof Error ? error.message : String(error))

const runJson = async <T,>(
  $: Engine,
  argv: string[],
  parse: (stdout: string) => T,
  accepts: (exitCode: number) => boolean,
): Promise<Fetched<T>> => {
  const run = await $.process.run(argv).catch((error: unknown) => message(error))
  if (typeof run === 'string') return { kind: 'failed', exitCode: null, stderr: run }
  if (!accepts(run.exitCode)) return { kind: 'failed', exitCode: run.exitCode, stderr: run.stderr }
  try {
    return { kind: 'ready', value: parse(run.stdout), fetchedAt: await $.clock.now() }
  } catch (error) {
    return { kind: 'failed', exitCode: 0, stderr: message(error) }
  }
}

const take = async ($: Engine): Promise<Snapshot> => {
  const next = await runJson($, ARGV, parseConfig, code => code === 0)
  return next.kind === 'ready' ? { kind: 'ready', config: next.value, fetchedAt: next.fetchedAt } : next
}

const fetchConfig = async ($: Engine): Promise<void> => {
  const next = await take($)
  await update($, snapshot, () => next)
}

const refresh = async ($: Engine): Promise<void> => {
  await update($, snapshot, () => ({ kind: 'loading' }))
  await fetchConfig($)
}

const fetchRuntime = async ($: Engine): Promise<void> => {
  const next = await runJson($, RUNTIME_ARGV, parseRuntime, code => code === 0)
  await update($, runtime, () => next)
}

// doctor exits 1 when any check fails; its report is still the answer.
const fetchHealth = async ($: Engine): Promise<void> => {
  const next = await runJson($, HEALTH_ARGV, parseHealth, code => code === 0 || code === 1)
  await update($, health, () => next)
}

const openPipeline = async ($: Engine): Promise<void> => {
  await Promise.all([fetchRuntime($), fetchHealth($)])
}

const fetchPage = async ($: Engine, current: Page): Promise<void> => {
  if (groupOf(current) === 'run') await openPipeline($)
}

let poll: Timer | undefined

const pollRuntime = async ($: Poller): Promise<void> => {
  if (!(await $.ui.panes()).some(one => one.id === PANE)) {
    poll?.cancel()
    poll = undefined
    return
  }
  if (groupOf(await read($, page)) === 'run') await fetchRuntime($)
}

const ensurePolling = ($: Poller) => {
  poll ??= $.clock.every(POLL_MS, () => void pollRuntime($))
}

const choose = async ($: Poller, next: Page): Promise<void> => {
  await update($, page, () => next)
  ensurePolling($)
  await fetchPage($, next)
}

const refreshPage = async ($: Engine, current: Page): Promise<void> => {
  await Promise.all([refresh($), fetchPage($, current)])
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

const CHECK_LABELS: Record<string, string> = {
  version: '运行版本',
  service_path: '服务 PATH',
  maintenance: '维护状态',
  checkouts: '项目检出',
  sync_prs: 'core 同步',
  ledger: '账本',
  launches: '重复启动',
  credentials: '凭证',
}

const CHECK_STATUS: Record<string, { text: string; color?: string }> = {
  ok: { text: '通过' },
  warn: { text: '注意', color: 'warning' },
  fail: { text: '失败', color: 'error' },
  unknown: { text: '未知', color: 'warning' },
}

const CHECK_ORDER = Object.keys(CHECK_LABELS)

const checkRank = (id: string) => {
  const at = CHECK_ORDER.indexOf(id)
  return at === -1 ? CHECK_ORDER.length : at
}

const doctorChecks = (ui: Kit, state: Fetched<Health>) => {
  if (state.kind === 'loading') return <ui.Text dimColor>正在运行 orca-factory-driver doctor --json …</ui.Text>
  if (state.kind === 'failed') return failedView(ui, state)
  return [...state.value.checks].sort((a, b) => checkRank(a.id) - checkRank(b.id)).map(one => {
    const status = CHECK_STATUS[one.status] ?? { text: one.status, color: 'warning' }
    return (
      <ui.Box key={`check-${one.id}`} flexDirection="row">
        {cell(ui, 12, CHECK_LABELS[one.id] ?? one.id, true)}
        {cell(ui, 6, status.text, status.color === undefined, status.color)}
        <ui.Text wrap="truncate">{one.reason}</ui.Text>
      </ui.Box>
    )
  })
}

const healthPage = (ui: Kit, config: DriverConfig, checks: Fetched<Health>) => {
  const { overview, problems } = config
  return (
    <ui.Box flexDirection="column">
      {field(ui, '构建', overview.build.slice(0, 7))}
      {field(ui, '服务', overview.service, overview.service === 'active' ? 'success' : 'warning')}
      {field(ui, '维护模式', overview.maintenance, overview.maintenance === 'running' ? undefined : 'warning')}
      {field(ui, '状态快照', overview.snapshot)}
      <ui.Text bold>doctor 检查</ui.Text>
      {doctorChecks(ui, checks)}
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

const environmentPage = (ui: Kit, config: DriverConfig, cols: number) => {
  const groups = ['driver.env', '服务环境与代理', '工具路径']
  const width = Math.min(30, Math.max(10, cols - 20))
  const entries = config.environment
  return (
    <ui.Box flexDirection="column">
      <ui.Box marginBottom={1}>{field(ui, '工厂目录', config.overview.factoryRoot || '未配置')}</ui.Box>
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

const TRACK = ['领', '施工', '门禁', '审查', '合并', '部署']
const NODE_WIDTH = 6

type Tone = 'current' | 'attention' | 'failed'
type Position = { at: number | null; tone: Tone; label: string }

const TONE_COLOR: Record<Tone, string> = { current: 'suggestion', attention: 'warning', failed: 'error' }

const PHASE_AT: Record<string, number> = {
  待领: 0,
  'worker 运行中': 1,
  '写 correction': 1,
  分支检查: 2,
  gate: 2,
  审查: 3,
  '开 PR': 4,
  排队合并: 4,
  已合并: 4,
}

const DEPLOY_LABELS: Record<string, string> = { pending: '等部署', 'deploy-intent': '部署中', 'smoke-intent': 'smoke', paused: '部署暂停' }

const position = (t: RuntimeTicket): Position => {
  const d = t.deployment
  if (d && d.state !== 'passed') return { at: TRACK.length - 1, tone: d.state === 'paused' ? 'failed' : 'current', label: DEPLOY_LABELS[d.state] ?? d.state }
  if (t.stage === 'candidate') return { at: 0, tone: 'current', label: '待领' }
  if (t.stage === 'human') return { at: null, tone: 'attention', label: '转人' }
  if (t.owner === '处置Agent') return { at: null, tone: 'attention', label: '处置 Agent' }
  const at = PHASE_AT[t.phase]
  if (at === undefined) return { at: null, tone: 'attention', label: t.phase || '未知阶段' }
  return { at, tone: t.phase === '写 correction' ? 'attention' : 'current', label: t.phase }
}

const duration = (ms: number) => {
  const seconds = Math.max(0, Math.floor(ms / 1000))
  if (seconds < 60) return `${seconds} 秒`
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `${minutes} 分钟`
  const hours = Math.floor(minutes / 60)
  return minutes % 60 === 0 ? `${hours} 小时` : `${hours} 小时 ${minutes % 60} 分`
}

const reasonOf = (t: RuntimeTicket): { text: string; color?: string } | undefined => {
  if (t.blockedReason) return { text: t.blockedReason, color: 'warning' }
  const d = t.deployment
  if (!d || d.state === 'passed') return undefined
  const text = [d.workflow || '部署', d.state, d.sha.slice(0, 7), d.reason].filter(Boolean).join(' · ')
  return d.state === 'paused' ? { text, color: 'error' } : { text }
}

const trackView = (ui: Kit, pos: Position, nodes: number) => (
  <ui.Box flexDirection="row" width={NODE_WIDTH * TRACK.length} flexShrink={0}>
    {Array.from({ length: nodes }, (_, i) => {
      const done = pos.at !== null && i < pos.at
      const here = pos.at === i
      return (
        <ui.Box flexDirection="row">
          <ui.Text color={done ? 'success' : here ? TONE_COLOR[pos.tone] : undefined} dimColor={!done && !here}>
            {done ? '●' : here ? '◉' : '○'}
          </ui.Text>
          {i < nodes - 1 && (
            <ui.Text color={done ? 'success' : undefined} dimColor={!done}>
              {done ? '━━━━━' : '─────'}
            </ui.Text>
          )}
        </ui.Box>
      )
    })}
  </ui.Box>
)

const ticketView = (ui: Kit, t: RuntimeTicket, nodes: number, now: number, wide: boolean) => {
  const pos = position(t)
  const reason = reasonOf(t)
  return (
    <ui.Box flexDirection="column" marginLeft={2}>
      <ui.Box key={`pipeline-${t.identifier}`} flexDirection="row">
        {cell(ui, 10, t.identifier)}
        {wide && trackView(ui, pos, nodes)}
        {cell(ui, 16, pos.label, false, pos.tone === 'current' ? undefined : TONE_COLOR[pos.tone])}
        {cell(ui, 12, t.since === undefined ? '' : duration(now - t.since), true)}
      </ui.Box>
      {t.title && (
        <ui.Text dimColor wrap="truncate">
          {t.title}
        </ui.Text>
      )}
      {reason && (
        <ui.Text color={reason.color} dimColor={reason.color === undefined} wrap="truncate">
          {`↳ ${reason.text}`}
        </ui.Text>
      )}
    </ui.Box>
  )
}

type Badge = { text: string; color?: string }

const badges = (repo: Repo, state: Fetched<Health>): Badge[] => {
  if (state.kind === 'loading') return [{ text: '检查中…' }]
  if (state.kind === 'failed') return [{ text: 'doctor 读取失败', color: 'warning' }]
  const { checkouts, syncPrs } = state.value
  const checkout = checkouts?.find(one => one.path === repo.path)
  const out: Badge[] = []
  if (!checkout) out.push({ text: '检出未知', color: 'warning' })
  else {
    out.push(checkout.branch === 'main' ? { text: 'main ✓' } : { text: `在 ${checkout.branch}`, color: 'warning' })
    if (checkout.behind > 0) out.push({ text: `落后 ${checkout.behind}`, color: 'warning' })
    if (!checkout.clean) out.push({ text: '⚠ 未提交改动', color: 'warning' })
  }
  if (syncPrs === null) {
    out.push({ text: 'core 未知', color: 'warning' })
    return out
  }
  const pr = syncPrs.find(one => one.project === repo.name)
  out.push(pr ? { text: `core 待同步 #${pr.number}`, color: 'warning' } : { text: 'core ✓' })
  return out
}

const projectHeader = (ui: Kit, repo: Repo, state: Fetched<Health>) => (
  <ui.Box flexDirection="row" justifyContent="space-between">
    <ui.Box flexDirection="row" gap={1} flexShrink={0}>
      <ui.Text bold>{repo.name}</ui.Text>
      <ui.Text dimColor>{repo.prefix}</ui.Text>
    </ui.Box>
    <ui.Box flexDirection="row" gap={2} marginLeft={1}>
      {badges(repo, state).map(one => (
        <ui.Text color={one.color} dimColor={one.color === undefined}>
          {one.text}
        </ui.Text>
      ))}
    </ui.Box>
  </ui.Box>
)

const freshness = (state: Runtime, fetchedAt: number) => {
  if (state.state === 'unknown') return '没有可用的快照'
  const age = `快照 ${duration(fetchedAt - state.generatedAt)}前`
  return state.state === 'ok' ? `${age} · 每 ${POLL_MS / 1000} 秒刷新` : `${age} · 已过期`
}

const pipelinePage = (ui: Kit, config: DriverConfig, state: Fetched<Runtime>, checks: Fetched<Health>, cols: number) => {
  if (state.kind === 'loading') return <ui.Text dimColor>正在读取 orca-factory-driver status --json …</ui.Text>
  if (state.kind === 'failed') return failedView(ui, state)
  const { tickets } = state.value
  const wide = cols >= WIDE_COLUMNS
  const inFlight = tickets.filter(t => t.stage === undefined).length
  const barriers = tickets.filter(t => t.deployment && t.deployment.state !== 'passed').length
  const waiting = tickets.filter(t => t.stage === 'candidate').length
  return (
    <ui.Box flexDirection="column">
      <ui.Text>{`在途 ${inFlight}   部署屏障 ${barriers}   待领 ${waiting}`}</ui.Text>
      {wide && (
        <ui.Box flexDirection="row" marginLeft={2} marginTop={1}>
          {cell(ui, 10, '')}
          {TRACK.map(label => cell(ui, NODE_WIDTH, label, true))}
        </ui.Box>
      )}
      {config.repos.map(one => {
        const own = tickets.filter(t => t.repo === one.path)
        const nodes = deploys(one.routing) ? TRACK.length : TRACK.length - 1
        return (
          <ui.Box flexDirection="column" marginTop={1}>
            {projectHeader(ui, one, checks)}
            {own.length === 0 ? (
              <ui.Box marginLeft={2}>
                <ui.Text dimColor>空闲</ui.Text>
              </ui.Box>
            ) : (
              own.map(t => ticketView(ui, t, nodes, state.fetchedAt, wide))
            )}
          </ui.Box>
        )
      })}
    </ui.Box>
  )
}

const cornerNote = (ui: Kit, group: Group, config: Snapshot, state: Fetched<Runtime>) => {
  if (group === 'run' && state.kind === 'ready') {
    const fresh = state.value.state === 'ok'
    return (
      <ui.Text wrap="truncate" color={fresh ? undefined : 'warning'} dimColor={fresh}>
        {freshness(state.value, state.fetchedAt)}
      </ui.Text>
    )
  }
  if (group === 'config' && config.kind === 'ready') {
    return (
      <ui.Text wrap="truncate" dimColor>
        {`取数于 ${clockTime(config.fetchedAt)}`}
      </ui.Text>
    )
  }
  return undefined
}

const failureTitle = (exitCode: number | null) => {
  if (exitCode === null) return '无法运行 orca-factory-driver'
  return exitCode === 0 ? '输出无法解析' : `取数失败（退出码 ${exitCode}）`
}

const failedView = (ui: Kit, state: { exitCode: number | null; stderr: string }) => (
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
      description: '打开 Driver 面板（运行：流水线、健康；配置：模型、调度、环境）',
      immediate: true,
    })
    return next(e)
  })

  on('command.run', { command: 'ziikoo-driver' }, async $ => {
    await $.ui.open({ id: PANE, title: TITLE, focus: true })
    ensurePolling($)
    await fetchConfig($)
    await fetchPage($, await read($, page))
    return { text: '已打开 Driver 面板。' }
  })

  on('ui.render', { component: 'Pane', requestId: PANE }, async ($, e) => {
    const resolved = $.ui.resolve(e)
    const ui: Kit = resolved
    const state = await read($, snapshot)
    const current = await read($, page)
    const chosen = await read($, repo)
    const runtimeState = await read($, runtime)
    const healthState = await read($, health)
    const cols = e.props.bodyColumns
    const config = state.kind === 'ready' ? state.config : undefined
    const selected = config?.repos.find(one => one.name === chosen) ?? config?.repos[0]
    const group = groupOf(current)

    return (
      <ui.Box flexDirection="column" width={cols}>
        <ui.Box flexDirection="row" justifyContent="space-between">
          <ui.Box flexDirection="row" gap={3} flexShrink={0}>
            {GROUPS.map(tabs => (
              <ui.Box flexDirection="row" gap={1}>
                <ui.Text bold={tabs.id === group} dimColor={tabs.id !== group}>{`${tabs.label} ▸`}</ui.Text>
                {tabs.pages.map(one => {
                  const press = () => choose($, one.id)
                  return one.id === current ? (
                    <ui.Button key={`page-${one.id}`} label={one.label} hotkey={one.hotkey} variant="primary" onPress={press} />
                  ) : (
                    <ui.Button key={`page-${one.id}`} label={one.label} hotkey={one.hotkey} plain onPress={press} />
                  )
                })}
              </ui.Box>
            ))}
          </ui.Box>
          <ui.Box flexDirection="row" gap={1} marginLeft={1}>
            {cornerNote(ui, group, state, runtimeState)}
            <ui.Button key="refresh" label="↻ 刷新" hotkey="r" onPress={() => refreshPage($, current)} />
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
          {state.kind === 'ready' && current === 'models' && modelsPage(ui, selected, cols)}
          {state.kind === 'ready' && current === 'scheduler' && schedulerPage(ui, state.config.scheduler, cols)}
          {state.kind === 'ready' && current === 'pipeline' && pipelinePage(ui, state.config, runtimeState, healthState, cols)}
          {state.kind === 'ready' && current === 'health' && healthPage(ui, state.config, healthState)}
          {state.kind === 'ready' && current === 'environment' && environmentPage(ui, state.config, cols)}
        </ui.Box>
        <ui.Text dimColor wrap="truncate">1–5 切页 · r 刷新 · Tab 切换按钮 · Esc 回到输入框</ui.Text>
      </ui.Box>
    )
  })
}
