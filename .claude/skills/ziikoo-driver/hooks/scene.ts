import type { DriverConfig, Parcel, Runtime, RuntimeTicket, Scene, Station } from '../types'
import { position, duration } from './ticket'
import { deploys } from './config'

const parcelOf = (ticket: RuntimeTicket, now: number): Parcel => {
  const pos = position(ticket)
  let station: Station
  if (pos.at === 5) station = ticket.deployment?.state === 'smoke-intent' ? 'smoke' : 'dispatch'
  else if (pos.at === 0) station = 'pickup'
  else if (pos.at === null) station = ticket.stage === 'human' ? 'human' : ticket.owner === '处置Agent' ? 'repair' : 'unknown'
  else if (pos.at === 1) station = pos.tone === 'attention' ? 'return' : 'work'
  else if (pos.at === 2) station = 'gate'
  else if (pos.at === 3) station = 'review'
  else station = pos.label === '开 PR' ? 'pack' : 'merge'
  return {
    identifier: ticket.identifier,
    url: ticket.url,
    title: ticket.title ?? ticket.identifier,
    owner: ticket.owner,
    queuePosition: ticket.queuePosition,
    lane: ticket.lane,
    corrections: ticket.corrections,
    stages: ticket.stages,
    phase: ticket.phase,
    label: pos.label,
    station,
    tone: ticket.deployment?.state === 'passed' && station === 'merge' ? 'done' : pos.tone,
    since: ticket.since,
    duration: ticket.since === undefined ? '未知' : duration(now - ticket.since),
    reason: ticket.blockedReason || ticket.deployment?.reason,
    humanKind: ticket.humanKind,
    humanReason: ticket.humanReason,
    blocker: ticket.blockedReason,
    deployment: ticket.deployment?.state,
    unknownDeploy: ticket.phase === '已合并' && ticket.deployment === undefined,
  }
}

export const sceneOf = (runtime: Runtime, config: DriverConfig): Scene => {
  const workers = Number(config.scheduler.find(one => one.key === 'max_in_flight')?.value ?? 1)
  const repos = [...config.repos]
  for (const ticket of runtime.tickets) {
    if (!repos.some(repo => repo.path === ticket.repo)) {
      repos.push({ name: ticket.repo, path: ticket.repo, remote: '', prefix: '', roles: [], routing: [] })
    }
  }
  return {
    generatedAt: runtime.generatedAt,
    today: runtime.today,
    nextScanAt: runtime.nextScanAt,
    state: runtime.state,
    maintenance: config.overview.maintenance,
    rows: repos.map(repo => ({
      name: repo.name,
      path: repo.path,
      deploy: deploys(repo.routing),
      workers: Number.isFinite(workers) && workers > 0 ? Math.floor(workers) : 1,
      parcels: runtime.tickets.filter(ticket => ticket.repo === repo.path).map(ticket => parcelOf(ticket, runtime.generatedAt)),
    })),
  }
}
