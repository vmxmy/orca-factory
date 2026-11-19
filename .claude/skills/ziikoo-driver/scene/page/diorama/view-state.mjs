import { skyOf, staleOf, STATION_NAMES, TRACK } from './model.mjs'

// Presentation state deliberately lives outside Scene and Archipelago. Renderers can
// consume one immutable frame while the transport continues to own the source facts.
const PULSE_TTL_MS = 3_200
const MAX_PULSES = 24

const identityOf = (ticket, change = {}) => {
  const path = change.path ?? ticket?.path ?? ''
  const identifier = change.ticket ?? change.identifier ?? ticket?.identifier ?? change.text ?? ''
  return `${path}\0${identifier}`
}

const SIDE_ROUTE = { return: 'repair', repair: 'human', human: 'gate' }

const routeOf = ticket => {
  if (!ticket) return { current: null, next: null }
  const current = TRACK[ticket.track]?.station === ticket.station
    ? TRACK[ticket.track]
    : ticket.track < 0 && ticket.station ? { station: ticket.station, name: ticket.stationName } : null
  if (!current || ticket.unknown) return { current: null, next: null }
  const nextStation = SIDE_ROUTE[current.station] ?? TRACK[ticket.track + 1]?.station
  const next = nextStation && (TRACK.find(step => step.station === nextStation) ?? { station: nextStation, name: STATION_NAMES[nextStation] ?? nextStation })
  return {
    current: { station: current.station, name: current.name },
    next: next ? { station: next.station, name: next.name } : null,
  }
}

const overviewOf = archipelago => {
  const islands = archipelago?.islands ?? []
  return {
    total: islands.length,
    busy: islands.filter(island => island.status === 'busy').length,
    attention: islands.filter(island => island.plate?.alert !== null && island.plate?.alert !== undefined).length,
    idle: islands.filter(island => island.idle).length,
  }
}

const freshnessOf = (scene, connection, at) => {
  const age = Math.max(0, at - (scene?.generatedAt ?? at))
  const online = connection?.online !== false
  let mode = 'fresh'
  if (!online) mode = 'disconnected'
  else if (scene?.state === 'unknown') mode = 'unknown'
  else if (staleOf(scene, age)) mode = 'stale'
  return {
    mode,
    age,
    sky: skyOf(scene, age),
    readableAge: mode === 'fresh' ? `快照 ${Math.floor(age / 1000)} 秒前` : mode === 'disconnected' ? '连接断开 · 保留本地快照' : mode === 'unknown' ? '没有快照' : `快照过期 · ${Math.floor(age / 1000)} 秒前`,
  }
}

const readableOf = enabled => ({
  enabled: Boolean(enabled),
  symbols: Boolean(enabled),
  largerLabels: Boolean(enabled),
  patterns: Boolean(enabled),
})

const freezeFrame = frame => Object.freeze({
  ...frame,
  overview: Object.freeze(frame.overview),
  freshness: Object.freeze(frame.freshness),
  selected: Object.freeze(frame.selected),
  follow: Object.freeze(frame.follow),
  readable: Object.freeze(frame.readable),
  idleDisclosure: Object.freeze(frame.idleDisclosure),
  cues: Object.freeze(frame.cues),
  pulses: Object.freeze(frame.pulses.map(pulse => Object.freeze(pulse))),
})

export const createViewState = ({ scene, archipelago, connection = { online: true, closed: false, failures: [] }, selected = null, now = () => Date.now(), reducedMotion = false, readable = false, follow = false } = {}) => {
  let currentScene = scene ?? { state: 'unknown', generatedAt: 0, rows: [] }
  let currentArchipelago = archipelago ?? { islands: [], tickets: [], global: false, hub: null }
  let currentConnection = connection
  let selectedKey = selected
  let motionReduced = Boolean(reducedMotion)
  let readableEnabled = Boolean(readable)
  let followEnabled = Boolean(follow)
  let idleExpanded = false
  let pendingFollow = null
  const pulses = new Map()

  const frame = (at = now()) => {
    const freshness = freshnessOf(currentScene, currentConnection, at)
    const ticket = currentArchipelago.tickets?.find(one => one.key === selectedKey) ?? null
    const route = routeOf(ticket)
    const livePulses = [...pulses.values()].filter(pulse => pulse.expiresAt > at).sort((a, b) => b.createdAt - a.createdAt)
    return freezeFrame({
      overview: overviewOf(currentArchipelago),
      idleDisclosure: {
        collapsed: currentArchipelago.global === true && !idleExpanded,
        expanded: idleExpanded,
        count: currentArchipelago.islands?.filter(island => island.idle).length ?? 0,
      },
      freshness,
      selected: { key: selectedKey, ticket, current: route.current, next: route.next },
      pulses: livePulses,
      follow: {
        enabled: followEnabled,
        target: pendingFollow,
      },
      readable: readableOf(readableEnabled),
      reducedMotion: motionReduced,
      cues: {
        static: motionReduced,
        pulse: !motionReduced,
      },
    })
  }

  return {
    frame,
    update(next = {}) {
      if (next.scene) currentScene = next.scene
      if (next.archipelago) currentArchipelago = next.archipelago
      if (Object.hasOwn(next, 'connection')) currentConnection = next.connection
      if (Object.hasOwn(next, 'selected')) selectedKey = next.selected
      if (Object.hasOwn(next, 'reducedMotion')) motionReduced = Boolean(next.reducedMotion)
      if (Object.hasOwn(next, 'readable')) readableEnabled = Boolean(next.readable)
      if (Object.hasOwn(next, 'follow')) followEnabled = Boolean(next.follow)
      if (Object.hasOwn(next, 'idleExpanded')) idleExpanded = Boolean(next.idleExpanded)
    },
    notify(changes = [], at = now()) {
      for (const change of changes) {
        const ticket = currentArchipelago.tickets?.find(one => one.path === change.path && one.identifier === (change.ticket ?? change.identifier))
        const key = identityOf(ticket, change)
        const previous = pulses.get(key)
        const pulse = {
          key,
          type: change.type ?? 'move',
          path: change.path ?? ticket?.path,
          ticket: change.ticket ?? change.identifier ?? ticket?.identifier,
          text: change.text ?? '',
          createdAt: previous?.createdAt ?? at,
          expiresAt: at + PULSE_TTL_MS,
        }
        pulses.set(key, pulse)
        if (followEnabled && change.type !== 'leave') pendingFollow = { key, path: pulse.path, ticket: pulse.ticket }
      }
      while (pulses.size > MAX_PULSES) pulses.delete(pulses.keys().next().value)
    },
    link(next) {
      currentConnection = next ?? { online: true, closed: false, failures: [] }
    },
    select(key) {
      selectedKey = key
    },
    follow(enabled = !followEnabled) {
      followEnabled = Boolean(enabled)
      if (!followEnabled) pendingFollow = null
    },
    acknowledgeFollow() {
      pendingFollow = null
    },
    overview(expanded = !idleExpanded) {
      idleExpanded = Boolean(expanded)
    },
    readable(enabled = !readableEnabled) {
      readableEnabled = Boolean(enabled)
    },
    motion(enabled) {
      motionReduced = Boolean(enabled)
    },
  }
}
