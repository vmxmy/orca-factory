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

export type Page = 'pipeline' | 'health' | 'models' | 'scheduler' | 'environment'

export type Group = 'run' | 'config'

export type FetchKind = 'config' | 'status' | 'doctor' | 'logs'

export type Deployment = { queueId: string; sha: string; workflow?: string; state: string; reason?: string }

export type RuntimeTicket = {
  repo: string
  identifier: string
  owner: string
  phase: string
  stage?: string
  title?: string
  since?: number
  blockedReason?: string
  deployment?: Deployment
}

export type Runtime = { state: string; generatedAt: number; tickets: RuntimeTicket[] }

export type Checkout = { path: string; branch: string; clean: boolean; behind: number }

export type DoctorCheck = { id: string; status: string; reason: string }

export type Health = {
  checkouts: Checkout[] | null
  syncPrs: { project: string; number: number }[] | null
  checks: DoctorCheck[]
}

export type TrendSample = { at: number; inFlight: number }

export type Alert = 'warning' | 'error'

export type TimelineEntry = { at: number; label: string; alert?: Alert }

export type Timeline = { id: string; entries: TimelineEntry[] }

export type Filter = { text: string; editing: boolean }

export type Fetched<T> =
  | { kind: 'loading' }
  | { kind: 'ready'; value: T; fetchedAt: number }
  | { kind: 'failed'; exitCode: number | null; stderr: string }

declare module 'claude-code' {
  interface PluginState {
    'ziikoo-driver': {
      snapshot: Shaped<Snapshot>
      page: Shaped<Page>
      repo: Shaped<string>
      runtime: Shaped<Fetched<Runtime>>
      health: Shaped<Fetched<Health>>
      busy: Shaped<FetchKind[]>
      trend: Shaped<TrendSample[]>
      detail: Shaped<string>
      timeline: Shaped<Fetched<Timeline>>
      filter: Shaped<Filter>
    }
  }
}
