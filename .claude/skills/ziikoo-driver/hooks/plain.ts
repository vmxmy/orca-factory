import type {
  Alert,
  DoctorCheck,
  DriverConfig,
  EnvEntry,
  Fetched,
  Health,
  Page,
  Repo,
  Runtime,
  RuntimeTicket,
  SchedulerSetting,
  Snapshot,
  Timeline,
} from '../types'
import { ROLE_LABELS, SETTINGS, SETTING_GROUPS, SOURCE_LABELS, routingSummary } from './config'
import { barriersOf, inFlightOf } from './runtime'
import { NO_MATCH, clockTime, duration, failureTitle, matches, position, reasonOf } from './view'

const TOOL_KEYS = new Set(['go', 'gh', 'git', 'jq', 'claude', 'codex'])

export const CHECK_LABELS: Record<string, string> = {
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

export const checkStatus = (status: string) => CHECK_STATUS[status] ?? { text: status, alert: 'warning' as const }

const CHECK_ORDER = Object.keys(CHECK_LABELS)

const checkRank = (id: string) => {
  const at = CHECK_ORDER.indexOf(id)
  return at === -1 ? CHECK_ORDER.length : at
}

export const sortedChecks = (checks: DoctorCheck[]) => [...checks].sort((a, b) => checkRank(a.id) - checkRank(b.id))

export const ENV_GROUPS = ['driver.env', '服务环境与代理', '工具路径']

export const envValue = (entry: EnvEntry) => {
  if (entry.secret) return entry.set ? '已设置' : '未设置'
  if (!entry.set) return TOOL_KEYS.has(entry.key) ? '未找到' : '未设置'
  return entry.value ?? '已设置'
}

export const envGroup = (entry: EnvEntry) => {
  if (entry.source === 'driver.env') return 'driver.env'
  return TOOL_KEYS.has(entry.key) ? '工具路径' : '服务环境与代理'
}

export const shownTickets = (one: Repo, tickets: RuntimeTicket[], wanted: string) =>
  tickets.filter(t => t.repo === one.path && matches(wanted, t.identifier, t.title))

export type Badge = { text: string; alert?: Alert }

export const badges = (repo: Repo, state: Fetched<Health>): Badge[] => {
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

export const freshness = (state: Runtime, fetchedAt: number) => {
  if (state.state === 'unknown') return '没有可用的快照'
  const age = `快照 ${duration(fetchedAt - state.generatedAt)}前`
  return state.state === 'ok' ? age : `${age} · 已过期`
}

export const idleText = ({ nextScanAt }: Runtime, fetchedAt: number) => {
  if (nextScanAt === undefined) return '空闲'
  return nextScanAt <= fetchedAt ? '空闲 · 即将巡检' : `空闲 · 下次巡检约 ${duration(nextScanAt - fetchedAt)}后`
}

export type PageView = {
  page: Page
  snapshot: Snapshot
  runtime: Fetched<Runtime>
  health: Fetched<Health>
  timeline: Fetched<Timeline>
  detail: string
  filter: string
  repo: string
}

type Failed = { exitCode: number | null; stderr: string }
type Ready<T> = Extract<Fetched<T>, { kind: 'ready' }>

const failureLines = (state: Failed) => [failureTitle(state.exitCode), ...state.stderr.split('\n').filter(line => line.trim() !== '')]

const titled = (name: string, wanted: string) => `Driver ${name}${wanted === '' ? '' : ` · 筛选：${wanted}`}`

const stageText = (t: RuntimeTicket, now: number) =>
  [position(t).label, ...(t.since === undefined ? [] : [duration(now - t.since)])].join(' · ')

const ticketLines = (t: RuntimeTicket, now: number) => {
  const reason = reasonOf(t)
  return [
    `${t.identifier} · ${stageText(t, now)}`,
    ...(t.title ? [`  ${t.title}`] : []),
    ...(reason ? [`  ↳ ${reason.text}`] : []),
  ]
}

const pipelineBlocks = (config: DriverConfig, state: Ready<Runtime>, checks: Fetched<Health>, wanted: string) => {
  const { tickets } = state.value
  const waiting = tickets.filter(t => t.stage === 'candidate').length
  const { service, build } = config.overview
  return [
    [
      [titled('流水线', wanted), service, build.slice(0, 7), freshness(state.value, state.fetchedAt)].join(' · '),
      `在途 ${inFlightOf(tickets)}   部署屏障 ${barriersOf(tickets)}   待领 ${waiting}`,
    ],
    ...config.repos.map(one => {
      const own = shownTickets(one, tickets, wanted)
      const empty = wanted === '' ? idleText(state.value, state.fetchedAt) : NO_MATCH
      return [
        `${one.name}（${one.prefix}）：${badges(one, checks).map(badge => badge.text).join(' · ')}`,
        ...(own.length === 0 ? [empty] : own.flatMap(t => ticketLines(t, state.fetchedAt))),
      ]
    }),
  ]
}

const factLines = (id: string, state: Fetched<Runtime>, repos: Repo[]) => {
  if (state.kind === 'failed') return [id, ...failureLines(state)]
  if (state.kind === 'loading') return [id]
  const t = state.value.tickets.find(one => one.identifier === id)
  if (!t) return [id, '已不在在途列表']
  const reason = reasonOf(t)
  return [
    t.title ? `${id} ${t.title}` : id,
    `阶段：${stageText(t, state.fetchedAt)}`,
    `归属：${t.owner}`,
    `仓库：${repos.find(one => one.path === t.repo)?.name ?? t.repo}`,
    ...(reason ? [`↳ ${reason.text}`] : []),
  ]
}

const timelineBlocks = (state: Fetched<Timeline>) => {
  if (state.kind === 'failed') return [['时间线', ...failureLines(state)]]
  if (state.kind === 'loading') return []
  const { entries, issues } = state.value
  return [
    ['时间线', ...(entries.length === 0 ? ['没有执行器日志'] : entries.map(one => `${clockTime(one.at)} ${one.label}`))],
    ...(issues.length === 0
      ? []
      : [['最近异常', ...issues.map(one => [clockTime(one.at), one.label, ...(one.detail === undefined ? [] : [one.detail])].join(' '))]]),
  ]
}

const healthBlocks = (config: DriverConfig, checks: Fetched<Health>) => {
  const { build, service, maintenance, snapshot } = config.overview
  return [
    ['Driver 健康', `构建：${build}`, `服务：${service}`, `维护模式：${maintenance}`, `状态快照：${snapshot}`],
    [
      'doctor 检查',
      ...(checks.kind === 'ready'
        ? sortedChecks(checks.value.checks).map(one => [CHECK_LABELS[one.id] ?? one.id, checkStatus(one.status).text, one.reason].join(' · '))
        : checks.kind === 'failed'
          ? failureLines(checks)
          : []),
    ],
    ['问题', ...(config.problems.length === 0 ? ['无'] : config.problems)],
  ]
}

const modelBlocks = (current: Repo | undefined) => {
  if (!current) return [['Driver 模型', '没有纳管仓库']]
  return [
    [
      `Driver 模型 · ${current.name}`,
      ...current.roles.map(one =>
        [ROLE_LABELS[one.role], one.agent || '-', one.model || '默认', one.effort || '-', one.key || '内置'].join(' · '),
      ),
    ],
    [routingSummary(current.routing)],
  ]
}

const settingLine = (one: SchedulerSetting) => {
  const view = SETTINGS[one.key]
  return [view.label, view.show(one.value), SOURCE_LABELS[one.source], one.key].join(' · ')
}

const schedulerBlocks = (settings: SchedulerSetting[]) => [
  ['Driver 调度'],
  ...SETTING_GROUPS.map(group => [group, ...settings.filter(one => SETTINGS[one.key].group === group).map(settingLine)]),
]

const environmentBlocks = (config: DriverConfig, wanted: string) => {
  const entries = config.environment.filter(entry => matches(wanted, entry.key))
  return [
    [titled('环境', wanted), `工厂目录：${config.overview.factoryRoot || '未配置'}`],
    ...ENV_GROUPS.map(group => {
      const shown = entries.filter(entry => envGroup(entry) === group)
      return [group, ...(shown.length === 0 && wanted !== '' ? [NO_MATCH] : shown.map(entry => `${entry.key}：${envValue(entry)}`))]
    }),
  ]
}

// undefined while the data this page shows is still being read.
const blocksOf = (view: PageView, config: DriverConfig): string[][] | undefined => {
  const { runtime, health, timeline, detail, filter } = view
  switch (view.page) {
    case 'pipeline':
      if (detail !== '') {
        const reading = runtime.kind === 'loading' || timeline.kind === 'loading' || (timeline.kind === 'ready' && timeline.value.id !== detail)
        return reading ? undefined : [factLines(detail, runtime, config.repos), ...timelineBlocks(timeline)]
      }
      if (runtime.kind === 'loading') return undefined
      return runtime.kind === 'failed' ? [failureLines(runtime)] : pipelineBlocks(config, runtime, health, filter)
    case 'health':
      return health.kind === 'loading' ? undefined : healthBlocks(config, health)
    case 'models':
      return modelBlocks(config.repos.find(one => one.name === view.repo) ?? config.repos[0])
    case 'scheduler':
      return schedulerBlocks(config.scheduler)
    case 'environment':
      return environmentBlocks(config, filter)
  }
}

export const pageText = (view: PageView): string | undefined => {
  const { snapshot } = view
  if (snapshot.kind === 'loading') return undefined
  if (snapshot.kind === 'failed') return failureLines(snapshot).join('\n')
  return blocksOf(view, snapshot.config)?.map(block => block.join('\n')).join('\n\n')
}
