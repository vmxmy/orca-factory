import type { Alert, Checkout, Deployment, DoctorCheck, Health, Runtime, RuntimeTicket, TimelineEntry } from '../types'
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
    tickets: list(o.tickets, 'tickets').map((one, i) => parseTicket(one, `tickets[${i}]`)),
  }
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

// The driver writes every event to the log twice.
export const parseTimeline = (stdout: string): TimelineEntry[] => {
  const seen = new Set<string>()
  return stdout
    .split('\n')
    .flatMap(line => {
      const o = record(line)
      const show = o && MESSAGES[loose(o, 'msg')]
      const at = o ? Date.parse(loose(o, 'time')) : Number.NaN
      if (!o || !show || Number.isNaN(at)) return []
      const id = `${loose(o, 'time')} ${loose(o, 'msg')} ${loose(o, 'event')}`
      if (seen.has(id)) return []
      seen.add(id)
      return [{ at, ...show(o) }]
    })
    .sort((a, b) => a.at - b.at)
}
