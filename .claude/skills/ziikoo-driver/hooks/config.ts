import type {
  DriverConfig,
  EnvEntry,
  Overview,
  Repo,
  Role,
  RoleId,
  SchedulerKey,
  SchedulerSetting,
  Setting,
  Source,
} from '../types'

export const SCHEMA = 'driver-config-v1'

export const ROLE_LABELS: Record<RoleId, string> = {
  worker_default: '施工',
  worker_strong: '施工 升档',
  reviewer: '审查',
  reviewer_small: '小票审查',
  resolver: '处置',
  intake: '需求助手',
}

export const SOURCE_LABELS: Record<Source, string> = {
  'driver.toml': 'driver.toml',
  default: '默认',
  'driver.env': 'driver.env',
  'agent-graph.env': 'agent-graph.env',
  service: '服务',
}

export const SETTING_GROUPS = ['并发与限速', 'GitHub', 'Orca 与执行器', '账本'] as const

type SettingView = { group: (typeof SETTING_GROUPS)[number]; label: string; show: (value: string) => string }

const DURATION_UNITS: Record<string, string> = { h: '小时', m: '分钟', s: '秒', ms: '毫秒', µs: '微秒', us: '微秒', ns: '纳秒' }

const duration = (value: string) =>
  [...value.matchAll(/(\d+(?:\.\d+)?)(ms|µs|us|ns|h|m|s)/g)]
    .map(([, amount = '', unit = '']) => `${amount} ${DURATION_UNITS[unit]}`)
    .join(' ') || value

export const SETTINGS: Record<SchedulerKey, SettingView> = {
  max_in_flight: { group: '并发与限速', label: '同时运行的票', show: value => `最多 ${value} 张` },
  'token_bucket.capacity': { group: '并发与限速', label: '领票连发上限', show: value => `${value} 次` },
  'token_bucket.refill_per_second': { group: '并发与限速', label: '领票补充速度', show: value => `每秒 ${value} 次` },
  'scheduler.unregistered_grace': { group: 'Orca 与执行器', label: '执行器登记宽限', show: duration },
  'github.http_timeout': { group: 'GitHub', label: '请求超时', show: duration },
  'orca.worker_start_wait': { group: 'Orca 与执行器', label: 'worker 就绪等待', show: duration },
  'orca.call_timeout': { group: 'Orca 与执行器', label: 'Orca 调用超时', show: duration },
  'ledger.flush_interval': { group: '账本', label: '用量账本写回', show: value => `每 ${duration(value)}` },
}

export type Fields = Record<string, unknown>

const isFields = (value: unknown): value is Fields =>
  typeof value === 'object' && value !== null && !Array.isArray(value)

export const fields = (value: unknown, path: string): Fields => {
  if (!isFields(value)) throw new Error(`${path} 不是对象`)
  return value
}

export const list = (value: unknown, path: string): unknown[] => {
  if (!Array.isArray(value)) throw new Error(`${path} 不是数组`)
  return value
}

export const text = (o: Fields, key: string, path: string): string => {
  const value = o[key]
  if (typeof value !== 'string') throw new Error(`${path}.${key} 不是字符串`)
  return value
}

export const flag = (o: Fields, key: string, path: string): boolean => {
  const value = o[key]
  if (typeof value !== 'boolean') throw new Error(`${path}.${key} 不是布尔值`)
  return value
}

const member = <K extends string>(table: Record<K, unknown>, value: string): value is K =>
  Object.hasOwn(table, value)

const oneOf = <K extends string>(table: Record<K, unknown>, o: Fields, key: string, path: string): K => {
  const value = text(o, key, path)
  if (!member(table, value)) throw new Error(`${path}.${key} 取值未知：${value}`)
  return value
}

const parseOverview = (value: unknown): Overview => {
  const o = fields(value, 'overview')
  return {
    build: text(o, 'build', 'overview'),
    service: text(o, 'service', 'overview'),
    maintenance: text(o, 'maintenance', 'overview'),
    snapshot: text(o, 'snapshot', 'overview'),
    factoryRoot: text(o, 'factoryRoot', 'overview'),
  }
}

const parseRole = (value: unknown, path: string): Role => {
  const o = fields(value, path)
  return {
    role: oneOf(ROLE_LABELS, o, 'role', path),
    agent: text(o, 'agent', path),
    model: text(o, 'model', path),
    effort: text(o, 'effort', path),
    key: text(o, 'key', path),
    raw: text(o, 'raw', path),
  }
}

const parseSetting = (value: unknown, path: string): Setting => {
  const o = fields(value, path)
  return {
    key: text(o, 'key', path),
    value: text(o, 'value', path),
    source: oneOf(SOURCE_LABELS, o, 'source', path),
    set: flag(o, 'set', path),
  }
}

const parseSchedulerSetting = (value: unknown, path: string): SchedulerSetting => ({
  ...parseSetting(value, path),
  key: oneOf(SETTINGS, fields(value, path), 'key', path),
})

const parseEnv = (value: unknown, path: string): EnvEntry => {
  const o = fields(value, path)
  const key = text(o, 'key', path)
  const source = oneOf(SOURCE_LABELS, o, 'source', path)
  const set = flag(o, 'set', path)
  if (o.secret === true) return { key, source, set, secret: true }
  return o.value === undefined
    ? { key, source, set, secret: false }
    : { key, source, set, secret: false, value: text(o, 'value', path) }
}

const parseRepo = (value: unknown, path: string): Repo => {
  const o = fields(value, path)
  return {
    name: text(o, 'name', path),
    remote: text(o, 'remote', path),
    path: text(o, 'path', path),
    prefix: text(o, 'prefix', path),
    roles: list(o.roles, `${path}.roles`).map((one, i) => parseRole(one, `${path}.roles[${i}]`)),
    routing: list(o.routing, `${path}.routing`).map((one, i) => parseSetting(one, `${path}.routing[${i}]`)),
  }
}

export const parseConfig = (stdout: string): DriverConfig => {
  const o = fields(JSON.parse(stdout), '输出')
  if (o.schema !== SCHEMA) throw new Error(`schema 不是 ${SCHEMA}：${String(o.schema)}`)
  return {
    schema: SCHEMA,
    generatedAt: text(o, 'generatedAt', '输出'),
    overview: parseOverview(o.overview),
    repos: list(o.repos, 'repos').map((one, i) => parseRepo(one, `repos[${i}]`)),
    scheduler: list(o.scheduler, 'scheduler').map((one, i) => parseSchedulerSetting(one, `scheduler[${i}]`)),
    environment: list(o.environment, 'environment').map((one, i) => parseEnv(one, `environment[${i}]`)),
    problems: list(o.problems, 'problems').map((one, i) => {
      if (typeof one !== 'string') throw new Error(`problems[${i}] 不是字符串`)
      return one
    }),
  }
}

// Mirrors the driver's newDeployment: any of these makes a merge wait at a deployment barrier.
export const deploys = (routing: Setting[]): boolean =>
  routing.some(one =>
    one.key === 'AGENT_SELF_DEPLOY' ? one.value === '1' : (one.key === 'AGENT_DEPLOY_WORKFLOW' || one.key === 'AGENT_SMOKE_CMD') && one.value !== '',
  )

export const routingSummary = (routing: Setting[]): string => {
  const value = (key: string) => routing.find(one => one.key === key)?.value ?? ''
  const escalate = value('AGENT_TIER_ESCALATE') === '1' ? '开' : '关'
  const small = value('AGENT_REVIEWER_SMALL_MAX_ESTIMATE')
  return `打回升档 ${escalate} · 小票审查 ≤${small || '-'}`
}
