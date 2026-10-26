import type {
  DriverConfig,
  EnvEntry,
  Overview,
  Repo,
  Role,
  RoleId,
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

type Fields = Record<string, unknown>

const isFields = (value: unknown): value is Fields =>
  typeof value === 'object' && value !== null && !Array.isArray(value)

const fields = (value: unknown, path: string): Fields => {
  if (!isFields(value)) throw new Error(`${path} 不是对象`)
  return value
}

const list = (value: unknown, path: string): unknown[] => {
  if (!Array.isArray(value)) throw new Error(`${path} 不是数组`)
  return value
}

const text = (o: Fields, key: string, path: string): string => {
  const value = o[key]
  if (typeof value !== 'string') throw new Error(`${path}.${key} 不是字符串`)
  return value
}

const flag = (o: Fields, key: string, path: string): boolean => {
  const value = o[key]
  if (typeof value !== 'boolean') throw new Error(`${path}.${key} 不是布尔值`)
  return value
}

const count = (o: Fields, key: string, path: string): number => {
  const value = o[key]
  if (typeof value !== 'number') throw new Error(`${path}.${key} 不是数字`)
  return value
}

const member = <K extends string>(table: Record<K, string>, value: string): value is K =>
  Object.hasOwn(table, value)

const oneOf = <K extends string>(table: Record<K, string>, o: Fields, key: string, path: string): K => {
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
    globalPause: flag(o, 'globalPause', 'overview'),
    pauseSeconds: count(o, 'pauseSeconds', 'overview'),
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
    scheduler: list(o.scheduler, 'scheduler').map((one, i) => parseSetting(one, `scheduler[${i}]`)),
    environment: list(o.environment, 'environment').map((one, i) => parseEnv(one, `environment[${i}]`)),
    problems: list(o.problems, 'problems').map((one, i) => {
      if (typeof one !== 'string') throw new Error(`problems[${i}] 不是字符串`)
      return one
    }),
  }
}

export const routingSummary = (routing: Setting[]): string => {
  const value = (key: string) => routing.find(one => one.key === key)?.value ?? ''
  const escalate = value('AGENT_TIER_ESCALATE') === '1' ? '开' : '关'
  const small = value('AGENT_REVIEWER_SMALL_MAX_ESTIMATE')
  return `打回升档 ${escalate} · 小票审查 ≤${small || '-'}`
}
