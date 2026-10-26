export type RoleId =
  | 'worker_default'
  | 'worker_strong'
  | 'reviewer'
  | 'reviewer_small'
  | 'resolver'
  | 'intake'

export type Source =
  | 'driver.toml'
  | 'default'
  | 'driver.env'
  | 'agent-graph.env'
  | 'service'

export type Role = {
  role: RoleId
  agent: string
  model: string
  effort: string
  key: string
  raw: string
}

export type Setting = { key: string; value: string; source: Source; set: boolean }

export type SchedulerKey =
  | 'max_in_flight'
  | 'token_bucket.capacity'
  | 'token_bucket.refill_per_second'
  | 'scheduler.unregistered_grace'
  | 'github.http_timeout'
  | 'orca.worker_start_wait'
  | 'orca.call_timeout'
  | 'ledger.flush_interval'

export type SchedulerSetting = Omit<Setting, 'key'> & { key: SchedulerKey }

export type EnvEntry =
  | { key: string; source: Source; set: boolean; secret: true }
  | { key: string; source: Source; set: boolean; secret: false; value?: string }

export type Repo = {
  name: string
  remote: string
  path: string
  prefix: string
  roles: Role[]
  routing: Setting[]
}

export type Overview = {
  build: string
  service: string
  maintenance: string
  snapshot: string
  factoryRoot: string
}

export type DriverConfig = {
  schema: 'driver-config-v1'
  generatedAt: string
  overview: Overview
  repos: Repo[]
  scheduler: SchedulerSetting[]
  environment: EnvEntry[]
  problems: string[]
}

export type Snapshot =
  | { kind: 'loading' }
  | { kind: 'ready'; config: DriverConfig; fetchedAt: number }
  | { kind: 'failed'; exitCode: number | null; stderr: string }

export type Page = 'overview' | 'models' | 'scheduler' | 'environment'

declare module 'claude-code' {
  interface PluginState {
    'ziikoo-driver': {
      snapshot: Shaped<Snapshot>
      page: Shaped<Page>
      repo: Shaped<string>
    }
  }
}
