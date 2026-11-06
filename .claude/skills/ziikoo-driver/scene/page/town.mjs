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

const ortho = nodes => nodes.length < 2 ? [...nodes] : nodes.slice(1).reduce((chain, node) => [...chain, ...elbow(chain.at(-1), node)], [nodes[0]])

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

const serviceDetour = (start, point) => dedupe([start, { x: start.x, y: start.y + SERVICE_ROW_OFFSET + PARCEL_CLEARANCE }, { x: point.x, y: point.y + SERVICE_ROW_OFFSET + PARCEL_CLEARANCE }, point])

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
        const shifted = backward
          ? route.map((node, index) => index > 0 && index < route.length - 1 && route[index - 1].y === node.y && route[index + 1].y === node.y ? { ...node, y: node.y + SERVICE_ROW_OFFSET } : node)
          : route
        // Every hop between waypoints turns orthogonally, so a backward move
        // descends to the service row, rides it under the oncoming lane, and
        // climbs to its slot column instead of cutting across the workshop.
        const merged = backward
          ? ortho([draft.start, { x: draft.start.x, y: draft.start.y + SERVICE_ROW_OFFSET }, { x: shifted.at(-1).x, y: shifted.at(-1).y + SERVICE_ROW_OFFSET }, { x: draft.point.x, y: draft.point.y + SERVICE_ROW_OFFSET }, draft.point])
          : ortho([draft.start, { x: shifted[0].x, y: draft.start.y }, ...shifted, { x: draft.point.x, y: shifted.at(-1).y }, draft.point])
        const trimmed = trimDock(merged)
        points = trimmed.length > 1 ? trimmed : merged
      } else {
        // Convoy: every mover follows the same L as the belt-lane route,
        // translated by its own slot offset from its own belt lane, so the
        // convoy's shape is the resting grid's shape for the whole tween.
        const route = travelPath(layout, stationOf(previous.parcels.get(key).station), draft.point.station)
        const offset = { x: draft.start.x - route[0].x, y: draft.start.y - route[0].y }
        const riding = route.map(node => ({ x: node.x + offset.x, y: node.y + offset.y }))
        points = dedupe([draft.start, ...riding.slice(1), draft.point])
      }
      drafted.set(key, { ...draft, points })
    }
    // Same-row swaps are geometrically unsafe at equal speed (the parcels meet
    // head-on on their shared row), so a displaced parcel heading for a slot
    // another drafted parcel starts from dips to the service row around the
    // crossing, moving immediately so the leaver never passes through an
    // idling parcel.
    const detoured = new Set()
    for (const [key, draft] of drafted) {
      if (draft.moved) continue
      const taken = [...drafted.values()].some(other => other !== draft && other.points[0].x === draft.point.x && other.points[0].y === draft.point.y)
      if (!taken) continue
      const swapDetour = serviceDetour(draft.points[0], draft.points.at(-1))
      drafted.set(key, { ...draft, points: swapDetour })
      detoured.add(key)
    }
    for (const [key, draft] of drafted) {
      // A station-to-station mover leaves immediately; a displaced parcel that
      // did not detour waits for movers whose lane crosses its resting spot,
      // so the waits never form a cycle.
      let wait = 0
      if (!draft.moved && !detoured.has(key)) {
        for (const [otherKey, other] of drafted) {
          if (otherKey === key) continue
          const crossing = other.points.slice(1).some((node, index) => nearSegment(other.points[index], node, draft.points[0]))
            || other.points.slice(1).some((node, index) => nearSegment(other.points[index], node, draft.points.at(-1)))
          const swap = other.points[0].x === draft.point.x && other.points[0].y === draft.point.y
          if (crossing || swap) wait = Math.max(wait, Math.max(240, Math.round(pathLength(other.points) / BELT_PX_PER_MS)))
        }
      }
      // A swap detour clears the shared row at twice the belt speed; equal
      // speed would graze the passing parcel at 34/√2 ≈ 24px, under the 26px
      // clearance the resting grid keeps.
      const speed = detoured.has(key) ? BELT_PX_PER_MS * 2 : BELT_PX_PER_MS
      const duration = Math.max(240, Math.round(pathLength(draft.points) / speed))
      next.motions.set(key, { points: draft.points, start: draft.begin + wait, duration })
    }
  }
  return next
}
