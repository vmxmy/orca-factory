import type {
  Alert,
  Checkout,
  Deployment,
  DoctorCheck,
  Fetched,
  Health,
  LogIssue,
  Runtime,
  RuntimeTicket,
  Timeline,
  TimelineEntry,
} from '../types'
import { fields, flag, list, text } from './config'
import type { Fields } from './config'

const time = (o: Fields, key: string, path: string): number => {
  const value = Date.parse(text(o, key, path))
  if (Number.isNaN(value)) throw new Error(`${path}.${key} 不是时间`)
  return value
}

const count = (o: Fields, key: string, path: string): number => {
  const value = o[key]
  if (typeof value !== 'number') throw new Error(`${path}.${key} 不是数字`)
  return value
}

const optionalText = (o: Fields, key: string, path: string) => (o[key] === undefined ? undefined : text(o, key, path))

const parseDeployment = (value: unknown, path: string): Deployment => {
  const o = fields(value, path)
  return {
    queueId: text(o, 'queueId', path),
    sha: text(o, 'sha', path),
    workflow: optionalText(o, 'workflow', path),
    state: text(o, 'state', path),
    reason: optionalText(o, 'reason', path),
  }
}

const parseTicket = (value: unknown, path: string): RuntimeTicket => {
  const o = fields(value, path)
  return {
    repo: text(o, 'repo', path),
    identifier: text(o, 'identifier', path),
    owner: text(o, 'owner', path),
    phase: text(o, 'phase', path),
    stage: optionalText(o, 'stage', path),
    title: optionalText(o, 'title', path),
    since: o.since === undefined ? undefined : time(o, 'since', path),
    blockedReason: optionalText(o, 'blockedReason', path),
    deployment: o.deployment === undefined ? undefined : parseDeployment(o.deployment, `${path}.deployment`),
  }
}

export const parseRuntime = (stdout: string): Runtime => {
  const o = fields(JSON.parse(stdout), '输出')
  return {
    state: text(o, 'state', '输出'),
    generatedAt: time(o, 'generatedAt', '输出'),
    nextScanAt: o.nextScanAt === undefined ? undefined : time(o, 'nextScanAt', '输出'),
    tickets: list(o.tickets, 'tickets').map((one, i) => parseTicket(one, `tickets[${i}]`)),
  }
}

export const inFlightOf = (tickets: RuntimeTicket[]) => tickets.filter(t => t.stage === undefined).length

const holdsDeploy = (t: RuntimeTicket) => t.deployment !== undefined && t.deployment.state !== 'passed'

export const barriersOf = (tickets: RuntimeTicket[]) => tickets.filter(holdsDeploy).length

export const statusLine = (state: Fetched<Runtime>): string | undefined => {
  if (state.kind === 'loading') return undefined
  if (state.kind === 'failed') return 'Driver 状态读取失败'
  const { state: snapshot, tickets } = state.value
  if (snapshot === 'unknown') return 'Driver 没有可用的快照'
  const inFlight = inFlightOf(tickets)
  const barriers = barriersOf(tickets)
  return [
    inFlight === 0 ? 'Driver 空闲' : `Driver 在途 ${inFlight}`,
    ...(barriers === 0 ? [] : [`部署屏障 ${barriers}`]),
    ...(snapshot === 'ok' ? [] : ['快照过期']),
  ].join(' · ')
}

const ENTERED: { label: string; holds: (t: RuntimeTicket) => boolean }[] = [
  { label: '已合并', holds: t => t.phase === '已合并' },
  { label: '升级到二线 Agent', holds: t => t.owner === '处置Agent' },
  { label: '转人', holds: t => t.stage === 'human' },
  { label: '部署暂停', holds: t => t.deployment?.state === 'paused' },
  { label: '打回修改', holds: t => t.phase === '写 correction' },
]

// A ticket that was merged or held a deployment leaves the list once it is done; any other leaves for reasons the list cannot tell.
const finished = (t: RuntimeTicket) => t.phase === '已合并' || holdsDeploy(t)

export const noticeOf = (before: RuntimeTicket[], after: RuntimeTicket[]): string | undefined => {
  const earlier = new Map(before.map(t => [t.identifier, t]))
  const still = new Set(after.map(t => t.identifier))
  const entered = after.flatMap(t => {
    const was = earlier.get(t.identifier)
    return ENTERED.filter(rule => rule.holds(t) && !(was && rule.holds(was))).map(rule => `${t.identifier} ${rule.label}`)
  })
  const left = before.filter(t => !still.has(t.identifier) && finished(t)).map(t => `${t.identifier} 已完成`)
  const lines = [...entered, ...left]
  return lines.length === 0 ? undefined : lines.join('；')
}

// doctor leaves details out of a check it could not probe, such as when project discovery fails.
const details = (checks: Fields, name: string): Fields | null => {
  const check = fields(checks[name], `checks.${name}`)
  return check.details === undefined ? null : fields(check.details, `checks.${name}.details`)
}

const parseCheck = ([id, value]: [string, unknown]): DoctorCheck => {
  const o = fields(value, `checks.${id}`)
  return { id, status: text(o, 'status', `checks.${id}`), reason: text(o, 'reason', `checks.${id}`) }
}

const parseCheckout = (value: unknown, path: string): Checkout => {
  const o = fields(value, path)
  return { path: text(o, 'path', path), branch: text(o, 'branch', path), clean: flag(o, 'clean', path), behind: count(o, 'behind', path) }
}

export const parseHealth = (stdout: string): Health => {
  const checks = fields(fields(JSON.parse(stdout), '输出').checks, 'checks')
  const projects = 'checks.checkouts.details.projects'
  const prs = 'checks.sync_prs.details.prs'
  const checkouts = details(checks, 'checkouts')
  const syncPrs = details(checks, 'sync_prs')
  return {
    checkouts: checkouts && list(checkouts.projects, projects).map((one, i) => parseCheckout(one, `${projects}[${i}]`)),
    syncPrs: syncPrs && list(syncPrs.prs, prs).map((one, i) => {
      const o = fields(one, `${prs}[${i}]`)
      return { project: text(o, 'project', `${prs}[${i}]`), number: count(o, 'number', `${prs}[${i}]`) }
    }),
    checks: Object.entries(checks).map(parseCheck),
  }
}

type Shown = { label: string; alert?: Alert }

const EVENTS: Record<string, Shown> = {
  claim: { label: '认领' },
  workerOk: { label: 'worker 完成' },
  workerFail: { label: 'worker 失败', alert: 'warning' },
  workerStall: { label: 'worker 卡住', alert: 'warning' },
  workerUnstarted: { label: 'worker 未启动', alert: 'warning' },
  branchOk: { label: '分支检查通过' },
  branchNo: { label: '分支检查未通过', alert: 'warning' },
  gateGreen: { label: 'gate 通过' },
  gateRed: { label: 'gate 未通过', alert: 'warning' },
  reviewGreen: { label: '审查通过' },
  reviewRed: { label: '审查未通过', alert: 'warning' },
  reviewEmpty: { label: '审查无输出', alert: 'warning' },
  sessionOk: { label: 'correction 已写好' },
  sessionEmpty: { label: 'correction 为空', alert: 'warning' },
  openPr: { label: '开 PR' },
  merge: { label: '合并' },
  conflict: { label: '合并冲突', alert: 'error' },
  execCrash: { label: '执行器崩溃', alert: 'error' },
  readFail: { label: '读取失败', alert: 'error' },
  infraFailure: { label: '基础设施错误', alert: 'error' },
  requeue: { label: '重新排队' },
  tick: { label: '定时检查' },
}

const loose = (o: Fields, key: string) => (typeof o[key] === 'string' ? o[key] : '')

const MESSAGES: Record<string, (o: Fields) => Shown> = {
  transition: o => EVENTS[loose(o, 'event')] ?? { label: `${loose(o, 'from')} → ${loose(o, 'to')}` },
  'ticket completed': o => ({ label: `进入${loose(o, 'phase')}` }),
  'command started': () => ({ label: '执行器启动' }),
  'command exited': () => ({ label: '执行器退出' }),
}

// The 4 MiB stdout cap can cut the last line mid-object.
const record = (line: string): Fields | undefined => {
  try {
    return fields(JSON.parse(line), '日志行')
  } catch {
    return undefined
  }
}

type Issue = Omit<LogIssue, 'at'>

const firstLine = (text: string) => text.split('\n')[0] || undefined

// runx logs every command it runs and the executor every step it calls; only the failed ones are issues.
const ISSUES: Record<string, (o: Fields) => Issue | undefined> = {
  'external command': o => {
    const detail = firstLine(loose(o, 'stderr'))
    if (o.timed_out === true) return { label: `${loose(o, 'executable')} 超时`, detail, alert: 'error' }
    if (o.exit_code === 0) return undefined
    return { label: `${loose(o, 'executable')} 退出码 ${String(o.exit_code)}`, detail, alert: 'error' }
  },
  'external.end': o => {
    if (loose(o, 'result') === 'ok') return undefined
    const verdict = loose(o, 'verdict')
    return { label: `${loose(o, 'step')} 出错`, detail: verdict === '' ? undefined : `结论 ${verdict}`, alert: 'error' }
  },
  correction: o => ({ label: `打回：${loose(o, 'reason')}`, detail: firstLine(loose(o, 'fail_summary')), alert: 'warning' }),
}

// The driver writes every event to the log twice, as the same line.
export const parseTimeline = (stdout: string): Omit<Timeline, 'id'> => {
  const lines = [...new Set(stdout.split('\n'))]
    .flatMap(line => {
      const o = record(line)
      const at = o ? Date.parse(loose(o, 'time')) : Number.NaN
      return o && !Number.isNaN(at) ? [{ at, o }] : []
    })
    .sort((a, b) => a.at - b.at)
  return {
    entries: lines.flatMap(({ at, o }): TimelineEntry[] => {
      const show = MESSAGES[loose(o, 'msg')]
      return show ? [{ at, ...show(o) }] : []
    }),
    issues: pairFailures(
      lines.flatMap(({ at, o }) => {
        const issue = ISSUES[loose(o, 'msg')]?.(o)
        return issue ? [{ at, msg: loose(o, 'msg'), ...issue }] : []
      }),
    ),
  }
}

// A step that fails logs its failed command first, within the same second; one row names the step and keeps what the
// command row said (exit code or timeout, stderr), so one failure does not take two of the few rows shown.
const PAIR_MS = 1_000

const pairFailures = (issues: (LogIssue & { msg: string })[]): LogIssue[] =>
  issues
    .reduce<(LogIssue & { msg: string })[]>((kept, one) => {
      const last = kept.at(-1)
      if (last?.msg !== 'external command' || one.msg !== 'external.end' || one.at - last.at > PAIR_MS) return [...kept, one]
      const command = last.detail === undefined ? last.label : `${last.label}：${last.detail}`
      return [...kept.slice(0, -1), { ...one, detail: one.detail === undefined ? command : `${one.detail} · ${command}` }]
    }, [])
    .map(({ msg, ...issue }) => issue)
