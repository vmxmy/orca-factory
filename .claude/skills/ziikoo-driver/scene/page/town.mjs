export const stations = {
  pickup: ['领取', '领料公告板与箱筐架，显示等待领取的真实票据。'],
  work: ['施工', '锻造炉与铁砧，执行器正在实现或修改代码。'],
  gate: ['门禁', '验收扫描架，运行仓库门禁。灯光不代表后端未提供的测试结果。'],
  review: ['审查', '审查书桌与书架，读取实际审查阶段。'],
  pack: ['开 PR', '包装工作台，准备或创建拉取请求。'],
  merge: ['排队合并', '并列轨道与信号灯，等待合并通道。排列不代表真实队列位次。'],
  dispatch: ['部署', '吊运架与货箱，观察真实部署状态。'],
  smoke: ['smoke', '压力检测舱，观察真实冒烟验证状态。'],
  repair: ['修理', '轮组维修架，处置 Agent 正在处理异常。'],
  return: ['返工', '返工转盘，沿支线回到施工位修改。'],
  human: ['人工', '人工圆桌，等待人工处理。'],
  unknown: ['未知阶段', '后端阶段未映射到工位，保留原始阶段与原因。'],
}

const mainline = ['pickup', 'work', 'gate', 'review', 'pack', 'merge', 'dispatch', 'smoke']
export const stationOf = station => Object.hasOwn(stations, station) ? station : 'unknown'
export const parcelKey = (project, identifier) => JSON.stringify([project, identifier])

export function townLayout(rows) {
  const projects = new Map()
  const ordered = rows.map((row, index) => {
    const x = rows.length === 1 ? 0 : index % 2 === 0 ? -175 : 175
    const y = rows.length === 1 ? 0 : Math.floor(index / 2) * 250
    const project = { path: row.path, x, y, width: 260, height: 244, row, index }
    projects.set(row.path, project)
    return project
  })
  return { projects, ordered, bounds: { x: -330, y: -120, width: 660, height: Math.max(260, Math.ceil(rows.length / 2) * 250) } }
}

export function visibleProjects(layout, viewport) {
  const start = Math.max(0, Math.floor((viewport.y - 110) / 250)) * 2
  const end = Math.min(layout.ordered.length, (Math.ceil((viewport.y + viewport.height + 110) / 250) + 1) * 2)
  return layout.ordered.slice(start, Math.max(start, end)).filter(project => project.x + 140 >= viewport.x && project.x - 140 <= viewport.x + viewport.width)
}

export function workshopLayout(row) {
  const groups = new Map(Object.keys(stations).map(station => [station, []]))
  for (const parcel of row.parcels) groups.get(stationOf(parcel.station)).push(parcel)
  const band = keys => Math.max(1, ...keys.map(station => Math.ceil(groups.get(station).length / 4))) * 44
  const lower = 140 + band(mainline.slice(0, 4)) + 200
  const branch = lower + band(mainline.slice(4)) + 200
  const nodes = new Map()
  for (const [index, station] of mainline.entries()) nodes.set(station, { station, x: index < 4 ? 140 + index * 220 : 140 + (7 - index) * 220, y: index < 4 ? 140 : lower })
  for (const [index, station] of ['repair', 'return', 'human', 'unknown'].entries()) nodes.set(station, { station, x: 140 + index * 220, y: branch })
  const parcels = new Map()
  for (const [station, tickets] of groups) {
    const node = nodes.get(station)
    tickets.forEach((parcel, index) => parcels.set(parcelKey(row.path, parcel.identifier), { x: node.x + index % 4 * 34, y: node.y + 85 + Math.floor(index / 4) * 44, station, parcel }))
    node.count = tickets.length
    node.parcelRows = Math.max(1, Math.ceil(tickets.length / 4))
  }
  return { nodes, parcels, bounds: { x: 0, y: 20, width: 980, height: branch + band(['repair', 'return', 'human', 'unknown']) + 130 } }
}

const lane = node => ({ x: node.x, y: node.y + 85 })
const elbow = (from, to) => from.x === to.x || from.y === to.y ? [to] : [{ x: to.x, y: from.y }, to]

const ortho = nodes => nodes.length < 2 ? [...nodes] : nodes.slice(1).reduce((chain, node) => [...chain, ...elbow(chain.at(-1), node)], [nodes[0]])

export function travelPath(layout, source, target) {
  const from = stationOf(source)
  const to = stationOf(target)
  const start = lane(layout.nodes.get(from))
  const finish = lane(layout.nodes.get(to))
  if (from === to) return [start, finish]
  const fromIndex = mainline.indexOf(from)
  const toIndex = mainline.indexOf(to)
  if (fromIndex >= 0 && toIndex >= 0) {
    const route = [start]
    const step = fromIndex < toIndex ? 1 : -1
    for (let index = fromIndex + step; index !== toIndex + step; index += step) route.push(...elbow(route.at(-1), lane(layout.nodes.get(mainline[index]))))
    return route
  }
  const work = lane(layout.nodes.get('work'))
  const branchY = layout.nodes.get('return').y + 35
  const branchRoute = node => [lane(node), { x: node.x, y: branchY }, { x: 30, y: branchY }, { x: 30, y: work.y + 45 }, { x: work.x, y: work.y + 45 }, work]
  const first = fromIndex < 0 ? branchRoute(layout.nodes.get(from)) : travelPath(layout, from, 'work')
  const last = toIndex < 0 ? branchRoute(layout.nodes.get(to)).reverse() : travelPath(layout, 'work', to)
  return [...first, ...last.slice(1)]
}

export function pointAlong(points, progress) {
  const lengths = points.slice(1).map((point, index) => Math.hypot(point.x - points[index].x, point.y - points[index].y))
  let distance = Math.max(0, Math.min(1, progress)) * lengths.reduce((sum, length) => sum + length, 0)
  for (const [index, length] of lengths.entries()) {
    if (distance <= length && length > 0) {
      const ratio = distance / length
      return { x: points[index].x + (points[index + 1].x - points[index].x) * ratio, y: points[index].y + (points[index + 1].y - points[index].y) * ratio }
    }
    distance -= length
  }
  return points.at(-1)
}

export const initialState = () => ({ scene: null, signature: '', project: null, selection: null, view: 'town', camera: { x: 0, y: 0, zoom: 1 }, motions: new Map() })

// Every parcel rides the belt at one speed, so a convoy keeps its spacing and
// crossing traffic never depends on per-parcel timing. A parcel whose slot row
// is being crossed, or one moving backward on the mainline, dips to a service
// row below the lane instead of meeting oncoming parcels.
const BELT_PX_PER_MS = 0.45
const SERVICE_ROW_OFFSET = 44
const PARCEL_CLEARANCE = 26

const pathLength = points => points.reduce((sum, node, index) => index ? sum + Math.hypot(node.x - points[index - 1].x, node.y - points[index - 1].y) : 0, 0)

const dedupe = nodes => nodes.filter((node, index) => index === 0 || node.x !== nodes[index - 1].x || node.y !== nodes[index - 1].y)

const onSegment = (a, b, c) => (a.x - c.x) * (b.x - c.x) <= 0 && (a.y - c.y) * (b.y - c.y) <= 0 && (a.x === b.x || a.y === b.y)

// A slot that sits on a straight belt run does not need the corner beyond it.
const trimDock = nodes => {
  const list = [...nodes]
  while (list.length > 2 && onSegment(list[1], list[2], list[0])) list.splice(1, 1)
  while (list.length > 2 && onSegment(list.at(-3), list.at(-2), list.at(-1))) list.splice(list.length - 2, 1)
  return list
}

const nearSegment = (a, b, point) => {
  const within = (value, low, high) => value >= Math.min(low, high) - PARCEL_CLEARANCE && value <= Math.max(low, high) + PARCEL_CLEARANCE
  return a.y === b.y && Math.abs(a.y - point.y) < PARCEL_CLEARANCE && within(point.x, a.x, b.x)
    || a.x === b.x && Math.abs(a.x - point.x) < PARCEL_CLEARANCE && within(point.y, a.y, b.y)
}

// A parcel resting beside the lane a lone mover wants to ride would be cut in
// half by the 26px painted box, so the mover dips to the service row around
// every occupied slot column strictly between its endpoints and climbs back
// on its target column: horizontal runs stay on drawn lanes, vertical hops on
// node or slot columns, and no timing constraint is needed against idlers.
// Resting means the slot the parcel occupies during the tween, so displaced
// parcels count at their old slot and pure idlers at their unchanged one.
const occupiedColumns = (previous, moverKey) => {
  const columns = new Set()
  for (const [key, point] of previous.parcels) {
    if (key === moverKey) continue
    columns.add(point.x)
  }
  return columns
}

const laneDetour = (nodes, columns, serviceY) => {
  if (!columns.size) return null
  const hops = []
  for (let index = 1; index < nodes.length; index++) {
    const from = nodes[index - 1]
    const to = nodes[index]
    if (from.y !== to.y) { hops.push([to]); continue }
    // A mid-tween reconcile feeds fractional coordinates in, so blocked
    // columns are found by interval comparison, never by stepping pixel by
    // pixel toward an integer endpoint.
    const low = Math.min(from.x, to.x)
    const high = Math.max(from.x, to.x)
    const blocked = [...columns.values()].filter(x => x > low && x < high)
    if (!blocked.length) { hops.push([to]); continue }
    // The whole blocked hop rides the service row: the mover descends on its
    // own column, runs beneath every idler on the lane, and climbs on the
    // target column, so its box never crosses a resting box at any speed.
    hops.push([{ x: from.x, y: serviceY }, { x: to.x, y: serviceY }, to])
  }
  const merged = [nodes[0]]
  for (const hop of hops) merged.push(...hop.filter(point => {
    const previous = merged.at(-1)
    return !previous || point.x !== previous.x || point.y !== previous.y
  }))
  return merged.length > 1 ? ortho(merged) : null
}

export function reconcile(state, scene, { now = 0, fresh = false, reduced = false } = {}) {
  const signature = JSON.stringify(scene)
  if (signature === state.signature && !fresh && !reduced) return state
  const next = { ...state, scene, signature, motions: fresh || reduced ? new Map() : new Map(state.motions) }
  const projects = townLayout(scene.rows)
  const previousRow = state.scene?.rows.find(row => row.path === state.project)
  const row = scene.rows.find(one => one.path === state.project)
  if (!row) {
    next.project = scene.rows[0]?.path ?? null
    next.selection = null
    next.motions.clear()
    next.view = 'town'
    const anchor = projects.projects.get(next.project)
    next.camera = { x: anchor?.x ?? 0, y: anchor?.y ?? 0, zoom: state.camera.zoom }
    return next
  }
  const layout = workshopLayout(row)
  if (next.selection?.kind === 'parcel' && !layout.parcels.has(next.selection.key)) next.selection = null
  for (const key of next.motions.keys()) if (!layout.parcels.has(key)) next.motions.delete(key)
  if (next.view === 'town' && previousRow) {
    const previous = townLayout(state.scene.rows).projects.get(row.path)
    const anchor = projects.projects.get(row.path)
    next.camera = { ...state.camera, x: state.camera.x + anchor.x - previous.x, y: state.camera.y + anchor.y - previous.y }
  }
  if (!fresh && !reduced && previousRow) {
    const previous = workshopLayout(previousRow)
    const drafted = new Map()
    for (const [key, point] of layout.parcels) {
      const old = previous.parcels.get(key)
      if (!old) continue
      const running = state.motions.get(key)
      const start = running ? pointAlong(running.points, (now - running.start) / running.duration) : old
      const moved = old.station !== point.station
      const displaced = old.x !== point.x || old.y !== point.y
      if (!moved && !displaced) continue
      // Parcels rest beside their station's belt lane. A lone mover rides the
      // painted belt: long runs on the belt lane itself, corners at the belt's
      // node columns, and only the docking steps between lane and slot row
      // leave it. When several parcels move at once, each rides a same-shaped
      // path translated by its own slot offset (rigid convoy): equal speed and
      // equal length keep the grid spacing between them for the whole tween,
      // so simultaneous parcels can never bunch on a shared corner.
      drafted.set(key, { start, point, moved, duration: 0, points: null, begin: now, wait: 0 })
    }
    const moverCount = [...drafted.values()].filter(one => one.moved).length
    for (const [key, draft] of drafted) {
      let points
      if (!draft.moved) {
        points = [draft.start, ...elbow(draft.start, draft.point), draft.point]
      } else if (moverCount === 1) {
        const route = travelPath(layout, stationOf(previous.parcels.get(key).station), draft.point.station)
        const fromIndex = mainline.indexOf(stationOf(previous.parcels.get(key).station))
        const toIndex = mainline.indexOf(stationOf(draft.point.station))
        const backward = fromIndex >= 0 && toIndex >= 0 && toIndex < fromIndex
        // A backward move rides the belt backwards: it climbs its own station
        // column to the upper lane, rides that lane left to the target
        // column, and docks down to the slot row, mirroring the forward belt.
        const belt = ortho([draft.start, ...route.slice(1), draft.point])
        const docked = trimDock(belt)
        const beltRun = docked.length > 1 ? docked : belt
        const columns = occupiedColumns(previous, key)
        const serviceY = draft.point.y + SERVICE_ROW_OFFSET + PARCEL_CLEARANCE
        const dipped = laneDetour(beltRun, columns, serviceY)
        points = dipped || beltRun
      } else {
        // Convoy: every mover follows the same L as the belt-lane route,
        // translated by its own slot offset from its own belt lane, so the
        // convoy's shape is the resting grid's shape for the whole tween.
        // A hop whose translated run crosses a resting column dips whole to
        // the service row — the dip's segment lengths are identical for every
        // mover, so the convoy stays rigid while it clears waiting parcels.
        const route = travelPath(layout, stationOf(previous.parcels.get(key).station), draft.point.station)
        const offset = { x: draft.start.x - route[0].x, y: draft.start.y - route[0].y }
        const riding = route.map(node => ({ x: node.x + offset.x, y: node.y + offset.y }))
        const plain = dedupe([draft.start, ...riding.slice(1), draft.point])
        const columns = occupiedColumns(previous, key)
        const serviceY = draft.point.y + SERVICE_ROW_OFFSET + PARCEL_CLEARANCE
        points = laneDetour(plain, columns, serviceY) || plain
      }
      drafted.set(key, { ...draft, points })
    }
    // Same-row swaps are geometrically unsafe at equal speed (the parcels
    // would cut through each other's painted boxes), and a displaced parcel's
    // target slot is the mover's origin column. The mover already dips around
    // idle parcels on its lane, so the displaced parcel only needs to wait
    // until every mover has finished before taking the vacated slot; waits
    // never form a cycle because movers never wait.
    for (const [key, draft] of drafted) {
      let wait = 0
      if (!draft.moved) {
        for (const [otherKey, other] of drafted) {
          if (otherKey === key || !other.moved) continue
          wait = Math.max(wait, Math.max(240, Math.round(pathLength(other.points) / BELT_PX_PER_MS)))
        }
      }
      const raw = Math.round(pathLength(draft.points) / BELT_PX_PER_MS)
      const duration = draft.points.length > 2 ? raw : Math.max(240, raw)
      next.motions.set(key, { points: draft.points, start: draft.begin + wait, duration })
    }
  }
  return next
}
