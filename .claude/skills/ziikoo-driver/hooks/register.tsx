import { atom, read, update } from 'claude-code'
import type { EngineInterface, Register, RenderChildren, Timer } from 'claude-code'

import type {
  Alert,
  DriverConfig,
  EnvEntry,
  FetchKind,
  Fetched,
  Filter,
  Group,
  Health,
  Page,
  Repo,
  Runtime,
  RuntimeTicket,
  SchedulerSetting,
  Snapshot,
  Timeline,
  TrendSample,
} from '../types'
import { ROLE_LABELS, SETTINGS, SETTING_GROUPS, SOURCE_LABELS, deploys, parseConfig, routingSummary } from './config'
import { parseHealth, parseRuntime, parseTimeline } from './runtime'
import {
  NO_MATCH,
  ROW,
  SECTION_EDGE_FRAME,
  SECTION_INSET,
  TONE_COLOR,
  TRACK,
  TREND_MINUTES,
  cell,
  chip,
  chipText,
  clockTime,
  detailView,
  displayWidth,
  duration,
  failedView,
  field,
  filterBar,
  helpText,
  matches,
  position,
  reasonLine,
  section,
  segmentWidth,
  spinnerCells,
  trackLabels,
  trackView,
  trendView,
} from './view'
import type { InputElement, Kit } from './view'

const PANE = 'ziikoo-driver'
const TITLE = 'Driver'
const argvOf = (kind: FetchKind) => ['orca-factory-driver', kind, '--json']
// Most of a long ticket's log is polling, and process.run keeps only the first 4 MiB, so grep keeps the lines the timeline reads.
const LOGS_SCRIPT = 'orca-factory-driver logs "$1" --json | grep -E \'"msg":"(transition|ticket completed|command started|command exited)"\''
const TICKET_ID = /^[A-Z][A-Z0-9]*-\d+$/
const POLL_MS = 10_000
const SPIN_MS = 100
const TREND_MS = TREND_MINUTES * 60_000
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
const health = atom({ plugin: 'ziikoo-driver', key: 'health' } as const, { kind: 'loading' }, { shape: 'health-2' })
const busy = atom({ plugin: 'ziikoo-driver', key: 'busy' } as const, [], { shape: 'busy-1' })
const trend = atom({ plugin: 'ziikoo-driver', key: 'trend' } as const, [], { shape: 'trend-1' })
const detail = atom({ plugin: 'ziikoo-driver', key: 'detail' } as const, '', { shape: 'detail-1' })
const timeline = atom({ plugin: 'ziikoo-driver', key: 'timeline' } as const, { kind: 'loading' }, { shape: 'timeline-1' })
const NO_FILTER: Filter = { text: '', editing: false }
const filter = atom({ plugin: 'ziikoo-driver', key: 'filter' } as const, NO_FILTER, { shape: 'filter-1' })

type Engine = Pick<EngineInterface, 'state' | 'process' | 'clock' | 'ui'>

const message = (error: unknown) => (error instanceof Error ? error.message : String(error))

let spin: Timer | undefined
let frame = 0

// A blit is denied once the pane is closed or a hot reload has replaced this module, so the timer stops itself.
const startSpin = ($: Engine) => {
  frame = 0
  const timer = $.clock.every(SPIN_MS, async () => {
    frame += 1
    const { deny } = await $.ui.blit({ requestId: PANE, key: 'spin', cells: spinnerCells(frame) })
    if (deny === undefined) return
    timer.cancel()
    if (spin === timer) spin = undefined
  })
  spin = timer
}

// busy lives in plugin state and survives a hot reload that may drop a finally; keyed by kind, the next fetch of that kind clears it.
const runJson = async <T,>(
  $: Engine,
  kind: FetchKind,
  argv: string[],
  parse: (stdout: string) => T,
  accepts: (exitCode: number) => boolean,
): Promise<Fetched<T>> => {
  await update($, busy, kinds => (kinds.includes(kind) ? kinds : [...kinds, kind]))
  if (spin === undefined) startSpin($)
  try {
    const run = await $.process.run(argv).catch((error: unknown) => message(error))
    if (typeof run === 'string') return { kind: 'failed', exitCode: null, stderr: run }
    if (!accepts(run.exitCode)) return { kind: 'failed', exitCode: run.exitCode, stderr: run.stderr }
    try {
      return { kind: 'ready', value: parse(run.stdout), fetchedAt: await $.clock.now() }
    } catch (error) {
      return { kind: 'failed', exitCode: 0, stderr: message(error) }
    }
  } finally {
    if ((await update($, busy, kinds => kinds.filter(one => one !== kind))).length === 0) {
      spin?.cancel()
      spin = undefined
    }
  }
}

const take = async ($: Engine): Promise<Snapshot> => {
  const next = await runJson($, 'config', argvOf('config'), parseConfig, code => code === 0)
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

const inFlightOf = (tickets: RuntimeTicket[]) => tickets.filter(t => t.stage === undefined).length

const fetchRuntime = async ($: Engine): Promise<void> => {
  const next = await runJson($, 'status', argvOf('status'), parseRuntime, code => code === 0)
  if (next.kind === 'ready') {
    const sample = { at: next.fetchedAt, inFlight: inFlightOf(next.value.tickets) }
    await update($, trend, samples => [...samples.filter(one => one.at > sample.at - TREND_MS), sample])
  }
  await update($, runtime, () => next)
}

// doctor exits 1 when any check fails; its report is still the answer.
const fetchHealth = async ($: Engine): Promise<void> => {
  const next = await runJson($, 'doctor', argvOf('doctor'), parseHealth, code => code === 0 || code === 1)
  await update($, health, () => next)
}

// grep exits 1 when no line matches, which is a ticket without executor logs.
const fetchTimeline = async ($: Engine, id: string): Promise<void> => {
  if (!TICKET_ID.test(id)) {
    await update($, timeline, (): Fetched<Timeline> => ({ kind: 'failed', exitCode: null, stderr: '票号格式不对' }))
    return
  }
  const argv = ['bash', '-c', LOGS_SCRIPT, '_', id]
  const next = await runJson($, 'logs', argv, stdout => ({ id, entries: parseTimeline(stdout) }), code => code === 0 || code === 1)
  await update($, timeline, () => next)
}

const openDetail = async ($: Engine, id: string): Promise<void> => {
  await update($, detail, () => id)
  await update($, timeline, () => ({ kind: 'loading' }))
  await fetchTimeline($, id)
}

const openPipeline = async ($: Engine): Promise<void> => {
  await Promise.all([fetchRuntime($), fetchHealth($)])
}

const fetchPage = async ($: Engine, current: Page): Promise<void> => {
  if (groupOf(current) !== 'run') return
  const opened = current === 'pipeline' ? await read($, detail) : ''
  await Promise.all([openPipeline($), ...(opened === '' ? [] : [fetchTimeline($, opened)])])
}

let poll: Timer | undefined

const pollRuntime = async ($: Engine): Promise<void> => {
  if (!(await $.ui.panes()).some(one => one.id === PANE)) {
    poll?.cancel()
    poll = undefined
    return
  }
  if (groupOf(await read($, page)) === 'run') await fetchRuntime($)
}

const ensurePolling = ($: Engine) => {
  poll ??= $.clock.every(POLL_MS, () => void pollRuntime($))
}

// autoFocus acts only when the pane takes the keyboard and a press leaves the ring where it was, so the field is focused here.
// A host without a focus ring rejects the move; like a denied move, the field still shows and Tab reaches it.
const editFilter = async ($: Engine): Promise<void> => {
  await update($, filter, wanted => ({ ...wanted, editing: true }))
  await $.ui.focus({ requestId: PANE, key: 'filter-input' }).catch((error: unknown) => ({ deny: message(error) }))
}

const choose = async ($: Engine, next: Page): Promise<void> => {
  await update($, page, () => next)
  await update($, detail, () => '')
  await update($, filter, () => NO_FILTER)
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

const CHECK_LABELS: Record<string, string> = {
  service_path: '服务 PATH',
  version: '运行版本',
  maintenance: '维护状态',
  checkouts: '项目检出',
  sync_prs: 'core 同步',
  ledger: '账本',
  launches: '重复启动',
  credentials: '凭证',
}

const CHECK_STATUS: Record<string, { text: string; alert?: Alert }> = {
  ok: { text: '通过' },
  warn: { text: '注意', alert: 'warning' },
  fail: { text: '失败', alert: 'error' },
  unknown: { text: '未知', alert: 'warning' },
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
    const status = CHECK_STATUS[one.status] ?? { text: one.status, alert: 'warning' }
    return (
      <ui.Box key={`check-${one.id}`} flexDirection="row">
        {cell(ui, 12, CHECK_LABELS[one.id] ?? one.id, true)}
        <ui.Box width={7} flexShrink={0}>
          {chip(ui, status.text, status.alert)}
        </ui.Box>
        <ui.Text wrap="truncate">{one.reason}</ui.Text>
      </ui.Box>
    )
  })
}

const healthPage = (ui: Kit, config: DriverConfig, checks: Fetched<Health>) => {
  const { overview, problems } = config
  return (
    <ui.Box flexDirection="column">
      {section(ui, '运行状态', [
        field(ui, '构建', overview.build.slice(0, 7)),
        field(ui, '服务', overview.service, overview.service === 'active' ? 'success' : 'warning'),
        field(ui, '维护模式', overview.maintenance, overview.maintenance === 'running' ? undefined : 'warning'),
        field(ui, '状态快照', overview.snapshot),
      ])}
      {section(ui, 'doctor 检查', doctorChecks(ui, checks))}
      {section(
        ui,
        '问题',
        problems.length === 0 ? <ui.Text dimColor>无</ui.Text> : problems.map(problem => <ui.Text color="warning">{problem}</ui.Text>),
      )}
    </ui.Box>
  )
}

const modelsPage = (ui: Kit, current: Repo | undefined, cols: number) => {
  if (!current) return <ui.Text dimColor>没有纳管仓库</ui.Text>
  const { flexible: model, rest: key } = columns(cols - SECTION_INSET, 13 + 8 + 7, 0.65, 24)
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
      {section(ui, '角色', [
        row('角色', 'Agent', '模型', '强度', '来源键', true),
        ...current.roles.map(one =>
          row(ROLE_LABELS[one.role], one.agent || '-', one.model || '默认', one.effort || '-', one.key || '内置'),
        ),
      ])}
      <ui.Text wrap="truncate">{routingSummary(current.routing)}</ui.Text>
    </ui.Box>
  )
}

const settingRow = (ui: Kit, one: SchedulerSetting, keyWidth: number | null) => {
  const view = SETTINGS[one.key]
  const changed = one.source === 'driver.toml'
  return (
    <ui.Box key={`setting-${one.key}`} flexDirection="row">
      {cell(ui, 18, view.label)}
      {cell(ui, 14, view.show(one.value))}
      {cell(ui, 13, SOURCE_LABELS[one.source], !changed, changed ? 'suggestion' : undefined)}
      {keyWidth !== null && cell(ui, keyWidth, one.key, true)}
    </ui.Box>
  )
}

// A raw key cut to a few characters says nothing, so a narrow panel drops the column.
const KEY_MIN = 14

const schedulerPage = (ui: Kit, settings: SchedulerSetting[], cols: number) => {
  const room = cols - SECTION_INSET - 18 - 14 - 13
  const keyWidth = room < KEY_MIN ? null : room
  return (
    <ui.Box flexDirection="column">
      {SETTING_GROUPS.map(group =>
        section(
          ui,
          group,
          settings.filter(one => SETTINGS[one.key].group === group).map(one => settingRow(ui, one, keyWidth)),
        ),
      )}
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

const noMatch = (ui: Kit) => <ui.Text dimColor>{NO_MATCH}</ui.Text>

const environmentPage = (ui: Kit, config: DriverConfig, cols: number, wanted: Filter, bar: RenderChildren) => {
  const groups = ['driver.env', '服务环境与代理', '工具路径']
  const width = Math.min(30, Math.max(10, cols - SECTION_INSET - 20))
  const entries = config.environment.filter(entry => matches(wanted, entry.key))
  const rows = (group: string) =>
    entries
      .filter(entry => envGroup(entry) === group)
      .map(entry => (
        <ui.Box key={`env-${entry.key}`} flexDirection="row">
          {cell(ui, width, entry.key)}
          <ui.Text wrap="truncate" dimColor={!entry.set}>
            {envValue(entry)}
          </ui.Text>
        </ui.Box>
      ))
  return (
    <ui.Box flexDirection="column">
      {bar}
      {section(
        ui,
        '工厂目录',
        <ui.Text wrap="truncate" dimColor={!config.overview.factoryRoot}>
          {config.overview.factoryRoot || '未配置'}
        </ui.Text>,
      )}
      {groups.map(group => {
        const shown = rows(group)
        return section(ui, group, shown.length === 0 && wanted.text !== '' ? noMatch(ui) : shown)
      })}
    </ui.Box>
  )
}

const ticketView = (ui: Kit, t: RuntimeTicket, nodes: number, now: number, segment: number | null, open: (id: string) => void) => {
  const pos = position(t)
  return (
    <ui.Box flexDirection="column">
      <ui.Box key={`pipeline-${t.identifier}`} flexDirection="row">
        <ui.Box width={ROW.id} flexShrink={0}>
          <ui.Button key={`ticket-${t.identifier}`} label={t.identifier} plain onPress={() => open(t.identifier)} />
        </ui.Box>
        {segment !== null && trackView(ui, pos, nodes, segment)}
        {cell(ui, ROW.label, pos.label, false, pos.tone === 'current' ? undefined : TONE_COLOR[pos.tone])}
        {cell(ui, ROW.since, t.since === undefined ? '' : duration(now - t.since), true)}
      </ui.Box>
      {t.title && (
        <ui.Text dimColor wrap="truncate">
          {t.title}
        </ui.Text>
      )}
      {reasonLine(ui, t)}
    </ui.Box>
  )
}

type Badge = { text: string; alert?: Alert }

const badges = (repo: Repo, state: Fetched<Health>): Badge[] => {
  if (state.kind === 'loading') return [{ text: '检查中…' }]
  if (state.kind === 'failed') return [{ text: 'doctor 读取失败', alert: 'warning' }]
  const { checkouts, syncPrs } = state.value
  const checkout = checkouts?.find(one => one.path === repo.path)
  const out: Badge[] = []
  if (!checkout) out.push({ text: '检出未知', alert: 'warning' })
  else {
    out.push(checkout.branch === 'main' ? { text: 'main ✓' } : { text: `在 ${checkout.branch}`, alert: 'warning' })
    if (checkout.behind > 0) out.push({ text: `落后 ${checkout.behind}`, alert: 'warning' })
    if (!checkout.clean) out.push({ text: '未提交改动', alert: 'warning' })
  }
  if (syncPrs === null) {
    out.push({ text: 'core 未知', alert: 'warning' })
    return out
  }
  const pr = syncPrs.find(one => one.project === repo.name)
  out.push(pr ? { text: `core 待同步 #${pr.number}`, alert: 'warning' } : { text: 'core ✓' })
  return out
}

// Every chip is padded, so only two filled chips side by side need a space to keep their colors apart.
const spacedBefore = (list: Badge[], i: number) => i > 0 && list[i - 1]?.alert !== undefined && list[i]?.alert !== undefined

// A gap leaves its cells unpainted, so on the border the line would show between badges.
const badgeRow = (ui: Kit, repo: Repo, list: Badge[]) => (
  <ui.Box key={`badges-${repo.name}`} flexDirection="row" justifyContent="flex-end">
    {list.flatMap((one, i) => [...(spacedBefore(list, i) ? [<ui.Text>{' '}</ui.Text>] : []), chip(ui, one.text, one.alert)])}
  </ui.Box>
)

const badgesWidth = (list: Badge[]) =>
  list.reduce((sum, one, i) => sum + displayWidth(chipText(one.text)) + (spacedBefore(list, i) ? 1 : 0), 0)

const freshness = (state: Runtime, fetchedAt: number) => {
  if (state.state === 'unknown') return '没有可用的快照'
  const age = `快照 ${duration(fetchedAt - state.generatedAt)}前`
  return state.state === 'ok' ? age : `${age} · 已过期`
}

const pipelinePage = (
  ui: Kit,
  config: DriverConfig,
  state: Fetched<Runtime>,
  checks: Fetched<Health>,
  samples: TrendSample[],
  cols: number,
  wanted: Filter,
  bar: RenderChildren,
  open: (id: string) => void,
) => {
  if (state.kind === 'loading') return <ui.Text dimColor>正在读取 orca-factory-driver status --json …</ui.Text>
  if (state.kind === 'failed') return failedView(ui, state)
  const { tickets } = state.value
  const segment = segmentWidth(cols)
  const barriers = tickets.filter(t => t.deployment && t.deployment.state !== 'passed').length
  const waiting = tickets.filter(t => t.stage === 'candidate').length
  return (
    <ui.Box flexDirection="column">
      {bar}
      <ui.Text>{`在途 ${inFlightOf(tickets)}   部署屏障 ${barriers}   待领 ${waiting}`}</ui.Text>
      {trendView(ui, samples, state.fetchedAt, cols)}
      {segment !== null && <ui.Box marginTop={1}>{trackLabels(ui, segment)}</ui.Box>}
      {config.repos.map(one => {
        const own = tickets.filter(t => t.repo === one.path && matches(wanted, t.identifier, t.title))
        const nodes = deploys(one.routing) ? TRACK.length : TRACK.length - 1
        const list = badges(one, checks)
        const onEdge = displayWidth(`${one.name} ${one.prefix}`) + badgesWidth(list) + SECTION_EDGE_FRAME <= cols
        const idle = wanted.text === '' ? <ui.Text dimColor>空闲</ui.Text> : noMatch(ui)
        const rows = own.length === 0 ? [idle] : own.map(t => ticketView(ui, t, nodes, state.fetchedAt, segment, open))
        return onEdge
          ? section(ui, one.name, rows, one.prefix, { key: `edge-${one.name}`, node: badgeRow(ui, one, list) })
          : section(ui, one.name, [badgeRow(ui, one, list), ...rows], one.prefix)
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
    const loading = (await read($, busy)).length > 0
    const samples = await read($, trend)
    const opened = current === 'pipeline' ? await read($, detail) : ''
    const timelineState = await read($, timeline)
    const wanted = await read($, filter)
    const Input: InputElement | undefined = 'Input' in resolved ? resolved.Input : undefined
    const bar = (placeholder: string) => filterBar(ui, Input, wanted, placeholder, () => editFilter($), next => update($, filter, () => next))
    const cols = e.props.bodyColumns
    const config = state.kind === 'ready' ? state.config : undefined
    const selected = config?.repos.find(one => one.name === chosen) ?? config?.repos[0]
    const group = groupOf(current)

    return (
      <ui.Box flexDirection="column" width={cols}>
        <ui.Box flexDirection="row" justifyContent="space-between">
          <ui.Box flexDirection="row" gap={1} flexShrink={0}>
            {GROUPS.map(tabs => (
              <ui.Box flexDirection="row" gap={1}>
                <ui.Text bold={tabs.id === group} dimColor={tabs.id !== group}>{tabs.label}</ui.Text>
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
        </ui.Box>
        <ui.Box flexDirection="row" justifyContent="space-between">
          <ui.Box flexDirection="row" gap={1} flexShrink={0}>
            {config && (
              <ui.Text color={config.overview.service === 'active' ? 'success' : 'error'}>●</ui.Text>
            )}
            {config && <ui.Text>{config.overview.service}</ui.Text>}
            {config && <ui.Text dimColor>{`· ${config.overview.build.slice(0, 7)}`}</ui.Text>}
          </ui.Box>
          <ui.Box flexDirection="row" gap={1} marginLeft={1}>
            {cornerNote(ui, group, state, runtimeState)}
            {loading && 'Raster' in resolved && (
              <resolved.Raster key="spin" columns={1} rows={1} cells={spinnerCells(frame)} />
            )}
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
          {state.kind === 'ready' &&
            current === 'pipeline' &&
            (opened === ''
              ? pipelinePage(ui, state.config, runtimeState, healthState, samples, cols, wanted, bar('票号或标题'), id => openDetail($, id))
              : detailView(ui, opened, runtimeState, state.config.repos, timelineState, () => update($, detail, () => '')))}
          {state.kind === 'ready' && current === 'health' && healthPage(ui, state.config, healthState)}
          {state.kind === 'ready' && current === 'environment' && environmentPage(ui, state.config, cols, wanted, bar('变量名'))}
        </ui.Box>
        <ui.Text dimColor wrap="truncate">
          {helpText(current, opened !== '')}
        </ui.Text>
      </ui.Box>
    )
  })
}
