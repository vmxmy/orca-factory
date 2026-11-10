export const OWNER_COLORS = {
  无: '#9d98a8',
  执行器: '#3f6fd8',
  合并通道: '#7b55c9',
  处置Agent: '#eea61b',
  人: '#e0442e',
}

export const ownerColor = owner => OWNER_COLORS[owner] ?? OWNER_COLORS.无

export const TRACK = [
  { station: 'pickup', name: '待领' },
  { station: 'work', name: '施工' },
  { station: 'gate', name: 'gate' },
  { station: 'review', name: '审查' },
  { station: 'pack', name: '开 PR' },
  { station: 'merge', name: '排队合并' },
  { station: 'dispatch', name: '部署' },
  { station: 'smoke', name: 'smoke' },
]

export const STATION_NAMES = {
  pickup: '账簿塔前',
  work: '工作间',
  return: '回流坡道',
  gate: '检验间',
  review: '审查拱门',
  pack: '封箱台',
  merge: '栈桥',
  dispatch: '闸前水道',
  smoke: '闸后水道',
  repair: '处置塔前',
  human: '人工台',
  unknown: '账簿塔旁 · 工位未知',
}

const MIN_BOOTHS = 3
const BOOTH_STEP = 2
const BOOTHS_PER_ROW = 5
const ROW_STEP = 3.8
const FRONT_ROW_Z = -1.75
const DOOR_TO_SLOT = 1.55
const LAST_BOOTH_X = -4.5
const YARD_LAST_BOOTH_X = -9.5
const LIFT = 0.76
const RAFT_HEIGHT = 0.12
const SEALED = new Set(['pack', 'merge', 'dispatch', 'smoke'])
const AFLOAT = new Set(['dispatch', 'smoke'])
const FAILED_DEPLOYMENTS = new Set(['paused', 'failed'])
const GROUND_BACK = -8
const ISLAND_COLUMNS = 2
const ISLAND_COLUMN_STEP = 46
const ISLAND_ROW_STEP = 24
const HUB = { x: -30, z: 22, half: 3 }

const layoutOf = count => {
  const columns = Math.min(count, BOOTHS_PER_ROW)
  const rows = Math.ceil(count / columns)
  const right = rows > 1 ? YARD_LAST_BOOTH_X : LAST_BOOTH_X
  const booths = Array.from({ length: count }, (_, k) => ({
    x: right - BOOTH_STEP * (columns - 1 - (k % columns)),
    z: FRONT_ROW_Z - ROW_STEP * Math.floor(k / columns),
  }))
  const tower = right - BOOTH_STEP * (columns - 1) - 5.7
  const left = tower - 1.8
  const yardBack = booths.at(-1).z - 1.5
  return {
    booths,
    tower,
    ground: { left, right: 11.4, back: GROUND_BACK, front: 3 },
    yard: yardBack < GROUND_BACK ? { left, right: right + 1.2, back: yardBack, front: GROUND_BACK } : null,
    water: { left: 11.4, right: 17.4, back: -9, front: 10 },
  }
}

const STALE_MS = 40_000

export const staleOf = (scene, ageMs) => scene.state !== 'unknown' && (scene.state !== 'ok' || ageMs > STALE_MS)

export const skyOf = (scene, ageMs) => {
  if (scene.state === 'unknown') return 'dark'
  if (scene.maintenance !== 'running') return 'overcast'
  return staleOf(scene, ageMs) ? 'fog' : 'clear'
}

const lined = (first, step, perLine) => index => ({
  x: first.x + step.x * (index % perLine),
  y: first.y + LIFT * Math.floor(index / perLine),
  z: first.z + step.z * (index % perLine),
})

const doorway = (inside, waiting, step, perLine) => {
  const queue = lined(waiting, step, perLine)
  return index => (index === 0 ? inside : queue(index - 1))
}

const slotRules = layout => ({
  pickup: lined({ x: layout.tower + 1.6, y: 0.22, z: 0 }, { x: 0.95, z: 0 }, 3),
  work: index => ({ x: layout.booths[index].x, y: 0.22, z: layout.booths[index].z + DOOR_TO_SLOT }),
  gate: doorway({ x: -1, y: 0.22, z: 0 }, { x: -2.3, y: 0.22, z: 0 }, { x: -0.95, z: 0 }, 2),
  review: doorway({ x: 2.5, y: 0.22, z: 0 }, { x: 1.55, y: 0.22, z: 0 }, { x: -0.95, z: 0 }, 2),
  pack: doorway({ x: 5.5, y: 0.44, z: 0 }, { x: 3.7, y: 0.22, z: 0 }, { x: 0, z: 0 }, 1),
  merge: lined({ x: 10.8, y: 0.3, z: 0 }, { x: -1.05, z: 0 }, 4),
  dispatch: lined({ x: 13.2, y: -0.42, z: 1.3 }, { x: 0, z: -1.4 }, 4),
  smoke: lined({ x: 13.2, y: -0.42, z: 5.4 }, { x: 0, z: 1.4 }, 3),
  return: lined({ x: -6.5, y: 1.5, z: -5.4 }, { x: 0.95, z: 0 }, 3),
  repair: lined({ x: -1, y: 1.5, z: -4.7 }, { x: -0.95, z: 0 }, 4),
  human: doorway({ x: 3, y: 1.7, z: -5.6 }, { x: 2.05, y: 1.5, z: -5.6 }, { x: -0.95, z: 0 }, 3),
  unknown: lined({ x: layout.tower - 0.2, y: 0, z: 1.75 }, { x: -0.95, z: 0 }, 2),
})

export const stationOf = station => (Object.hasOwn(STATION_NAMES, station) ? station : 'unknown')

const byQueuePosition = (left, right) =>
  (left.queuePosition ?? Number.POSITIVE_INFINITY) - (right.queuePosition ?? Number.POSITIVE_INFINITY)

export const mergeQueue = parcels => [...parcels].sort(byQueuePosition)

const alertOf = (tone, station, unknownDeploy) => {
  if (tone === 'failed') return 'bad'
  if (tone === 'attention' || unknownDeploy || station === 'unknown') return 'warn'
  return null
}

const deploymentOf = row => {
  const holding = row.parcels.map(parcel => parcel.deployment).filter(state => state !== undefined && state !== 'passed')
  if (holding.some(state => FAILED_DEPLOYMENTS.has(state))) return { lock: 'closed', beacon: 'failed' }
  if (holding.length > 0) return { lock: 'closed', beacon: 'waiting' }
  if (!row.deploy) return { lock: 'open', beacon: 'off' }
  return { lock: 'open', beacon: row.parcels.some(parcel => parcel.unknownDeploy) ? 'unknown' : 'ok' }
}

const viewOf = (parcel, station, slot, deploy) => {
  const unknownDeploy = parcel.unknownDeploy && deploy
  return {
    identifier: parcel.identifier,
    title: parcel.title,
    owner: parcel.owner,
    color: ownerColor(parcel.owner),
    station,
    stationName: unknownDeploy ? `${STATION_NAMES[station]} · 部署未知` : STATION_NAMES[station],
    track: TRACK.findIndex(step => step.station === station),
    slot,
    band: SEALED.has(station),
    stamp: SEALED.has(station),
    raft: AFLOAT.has(station) || (station === 'merge' && parcel.phase === '已合并'),
    alert: alertOf(parcel.tone, station, unknownDeploy),
    unknown: station === 'unknown',
    label: parcel.label,
    phase: parcel.phase,
    duration: parcel.duration,
    since: parcel.since,
    reason: parcel.reason || parcel.blocker,
    queuePosition: parcel.queuePosition,
    unknownDeploy,
    undeployed: parcel.unknownDeploy && !deploy,
  }
}

export const islandOf = row => {
  const stations = new Map()
  for (const parcel of row.parcels) {
    const station = stationOf(parcel.station)
    stations.set(station, [...(stations.get(station) ?? []), parcel])
  }
  if (stations.has('merge')) stations.set('merge', mergeQueue(stations.get('merge')))
  const working = stations.get('work')?.length ?? 0
  const inFlight = row.parcels.length - (stations.get('pickup')?.length ?? 0)
  const layout = layoutOf(Math.max(MIN_BOOTHS, working))
  const rules = slotRules(layout)
  const parcels = [...stations].flatMap(([station, members]) =>
    members.map((parcel, index) => {
      const view = viewOf(parcel, station, rules[station](index), row.deploy)
      return view.raft && station === 'merge' ? { ...view, slot: { ...view.slot, y: view.slot.y + RAFT_HEIGHT } } : view
    }),
  )
  return {
    name: row.name,
    path: row.path,
    layout,
    parcels,
    working,
    workers: row.workers,
    lantern: stations.has('human'),
    inFlight,
    idle: inFlight === 0,
    ...deploymentOf(row),
  }
}

export const stuckOf = island =>
  island.parcels
    .filter(parcel => parcel.alert !== null)
    .sort((left, right) => (left.since ?? Number.POSITIVE_INFINITY) - (right.since ?? Number.POSITIVE_INFINITY))

const worstAlert = parcels => {
  if (parcels.some(parcel => parcel.alert === 'bad')) return 'bad'
  return parcels.length > 0 ? 'warn' : null
}

const placed = (row, index) => {
  const island = islandOf(row)
  const parcels = island.parcels.map(parcel => ({ ...parcel, key: `${index}/${parcel.identifier}` }))
  const stuck = stuckOf({ parcels })
  return {
    ...island,
    parcels,
    index,
    origin: { x: (index % ISLAND_COLUMNS) * ISLAND_COLUMN_STEP, z: Math.floor(index / ISLAND_COLUMNS) * ISLAND_ROW_STEP },
    plate: { name: island.name, inFlight: island.inFlight, barrier: island.lock === 'closed', stuck: stuck.length, alert: worstAlert(stuck) },
  }
}

export const archipelagoOf = scene => {
  const islands = scene.rows.map(placed)
  const tickets = islands.flatMap(island =>
    island.parcels.map(parcel => ({ ...parcel, island: island.index, project: island.name, path: island.path })),
  )
  return {
    islands,
    tickets,
    stuck: stuckOf({ parcels: tickets }),
    global: islands.length > 1,
    hub: islands.length > 1 ? HUB : null,
  }
}
