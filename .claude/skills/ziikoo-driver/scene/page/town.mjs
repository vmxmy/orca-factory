const geometryURL = new URL('./components/geometry.mjs', import.meta.url)
const token = new URL(import.meta.url).searchParams.get('t')
if (token) geometryURL.searchParams.set('t', token)
const {stages} = await import(geometryURL.href)

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
    const x = rows.length === 1 ? 520 : index % 2 === 0 ? 290 : 760
    const y = rows.length === 1 ? 330 : 245 + Math.floor(index / 2) * 320
    const project = { path: row.path, x, y, width: 294, height: 285, row, index }
    projects.set(row.path, project)
    return project
  })
  return { projects, ordered, bounds: { x: 0, y: 0, width: 1040, height: Math.max(760, (ordered.at(-1)?.y || 245) + 280) } }
}

export function visibleProjects(layout, viewport) {
  return layout.ordered.filter(project => project.x + 190 >= viewport.x && project.x - 190 <= viewport.x + viewport.width && project.y + 160 >= viewport.y && project.y - 150 <= viewport.y + viewport.height)
}

export function workshopLayout(row) {
  const groups = new Map(Object.keys(stations).map(station => [station, []]))
  for (const parcel of row.parcels) groups.get(stationOf(parcel.station)).push(parcel)
  const nodes = new Map(Object.entries(stages).map(([station, point]) => [station, {station, x:point.x, y:point.y}]))
  nodes.set('unknown', {station:'unknown', x:520, y:720})
  const parcels = new Map()
  for (const [station, tickets] of groups) {
    const node = nodes.get(station)
    tickets.forEach((parcel, index) => parcels.set(parcelKey(row.path, parcel.identifier), { x: node.x + (index % 4 - (Math.min(tickets.length,4)-1)/2) * 62 * (node.y === 285 ? -1 : 1), y: node.y + Math.floor(index / 4) * 44, station, parcel }))
    node.count = tickets.length
    node.parcelRows = Math.max(1, Math.ceil(tickets.length / 4))
  }
  return { nodes, parcels, bounds: { x: 0, y: 0, width: 1040, height: Math.max(760,...[...parcels.values()].map(point=>point.y+50)) } }
}

const lane = node => ({ x: node.x, y: node.y })
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
    for (let index = fromIndex + step; index !== toIndex + step; index += step) {
      const target = lane(layout.nodes.get(mainline[index]))
      if ((index === 4 && step === 1) || (index === 3 && step === -1)) route.push({x:948,y:route.at(-1).y},{x:948,y:target.y})
      route.push(...elbow(route.at(-1), target))
    }
    return route
  }
  const work = lane(layout.nodes.get('work'))
  const branchY = layout.nodes.get('return').y
  if (from === 'gate' && to === 'repair' || from === 'review' && to === 'return') return ortho([start,{x:start.x,y:branchY},finish])
  const branchRoute = node => ortho([lane(node), {x:node.x,y:branchY}, {x:work.x,y:branchY}, work])
  const first = fromIndex < 0 ? branchRoute(layout.nodes.get(from)) : travelPath(layout, from, 'work')
  const last = toIndex < 0 ? branchRoute(layout.nodes.get(to)).reverse() : travelPath(layout, 'work', to)
  return [...first, ...last.slice(1)]
}

// A motion may dwell at a named waypoint: the tween's timeline splits into
// the ride up to points[dwellAt], the dwell wait, and the ride on, so one
// motion can stage aside, let later flights pass, and settle — which a pure
// polyline cannot express. Callers that ignore dwell see the ride only.
export function pointAlong(points, progress, dwell = 0, dwellAt = 0) {
  if (progress >= 1) return points.at(-1)
  if (dwellAt <= 0 || dwellAt >= points.length - 1) {
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
  const stage = points.slice(0, dwellAt + 1)
  const settle = points.slice(dwellAt)
  const stageDuration = travelTime(stage)
  const settleDuration = travelTime(settle)
  const total = stageDuration + dwell + settleDuration
  const clock = Math.max(0, Math.min(1, progress)) * total
  if (clock <= stageDuration) return pointAlong(stage, clock / stageDuration)
  if (clock <= stageDuration + dwell) return points[dwellAt]
  return pointAlong(settle, (clock - stageDuration - dwell) / settleDuration)
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
const travelTime = points => Math.max(240, Math.round(pathLength(points) / BELT_PX_PER_MS))

const dedupe = nodes => nodes.filter((node, index) => index === 0 || node.x !== nodes[index - 1].x || node.y !== nodes[index - 1].y)

const onSegment = (a, b, c) => (a.x - c.x) * (b.x - c.x) <= 0 && (a.y - c.y) * (b.y - c.y) <= 0 && (a.x === b.x || a.y === b.y)

// A slot that sits on a straight belt run does not need the corner beyond it.
const trimDock = nodes => {
  const list = [...nodes]
  while (list.length > 2 && onSegment(list[1], list[2], list[0])) list.splice(1, 1)
  while (list.length > 2 && onSegment(list.at(-3), list.at(-2), list.at(-1))) list.splice(list.length - 2, 1)
  return list
}

// draw.mjs paints every parcel as a 26x26 box, so a flight path is safe only
// when every point on it keeps 26px of axial clearance from every resting box.
// Resting means the slot the parcel occupies during the tween: pure idlers at
// their unchanged slot, parcels that already flew at their new slot.
const flightClear = (points, byColumn, byRow) => {
  for (let index = 1; index < points.length; index++) {
    const from = points[index - 1]
    const to = points[index]
    if (from.y === to.y) {
      const low = Math.min(from.x, to.x) - PARCEL_CLEARANCE
      const high = Math.max(from.x, to.x) + PARCEL_CLEARANCE
      // A box on a row NEAR the segment (service rows sit between slot rows)
      // also intersects, so every row bucket within clearance is scanned.
      for (const row of nearKeys(byRow, from.y)) for (const rest of byRow.get(row)) {
        if (Math.abs(rest.y - from.y) < PARCEL_CLEARANCE && rest.x >= low && rest.x <= high) return false
      }
    } else if (from.x === to.x) {
      const low = Math.min(from.y, to.y) - PARCEL_CLEARANCE
      const high = Math.max(from.y, to.y) + PARCEL_CLEARANCE
      for (const column of nearKeys(byColumn, from.x)) for (const rest of byColumn.get(column)) {
        if (Math.abs(rest.x - from.x) < PARCEL_CLEARANCE && rest.y >= low && rest.y <= high) return false
      }
    }
  }
  // Endpoints rest at their slot's exact coordinates; a box whose center sits
  // under 26px from any rester also fails, not only segment crossings.
  const ends = [points[0], points.at(-1)]
  for (const end of ends) {
    for (const row of nearKeys(byRow, end.y)) for (const rest of byRow.get(row)) {
      if (Math.max(Math.abs(end.x - rest.x), Math.abs(end.y - rest.y)) < PARCEL_CLEARANCE) return false
    }
    for (const column of nearKeys(byColumn, end.x)) for (const rest of byColumn.get(column)) {
      if (Math.max(Math.abs(end.x - rest.x), Math.abs(end.y - rest.y)) < PARCEL_CLEARANCE) return false
    }
  }
  return true
}

// Bucket keys within clearance of a coordinate: boxes on a row or column
// close to — but not exactly on — a segment line still intersect its boxes.
const nearKeys = (buckets, value) => [...buckets.keys()].filter(key => Math.abs(key - value) < PARCEL_CLEARANCE)

// Bucket resters by their slot column and row so a flight only checks the
// handful of boxes that share a column or a row with one of its segments.
const restIndex = (parcels, skipKey) => {
  const byColumn = new Map()
  const byRow = new Map()
  for (const [key, point] of parcels) {
    if (key === skipKey) continue
    if (!byColumn.has(point.x)) byColumn.set(point.x, [])
    if (!byRow.has(point.y)) byRow.set(point.y, [])
    byColumn.get(point.x).push(point)
    byRow.get(point.y).push(point)
  }
  return { byColumn, byRow }
}

// Candidate paths between two slots, cheapest first: the straight L in both
// elbow orders, then a U-dive at every candidate column — descend on the
// start column, run along the service row to the climb column (which may lie
// beyond either endpoint, rounding a stacked column block through the empty
// corridor), and climb to the target row.
const flightCandidates = (start, end, serviceY, columns) => {
  const list = []
  if (start.x === end.x || start.y === end.y) {
    list.push([start, end])
  } else {
    list.push([start, { x: start.x, y: end.y }, end])
    list.push([start, { x: end.x, y: start.y }, end])
  }
  const climbColumns = [...new Set([end.x, ...columns])]
  for (const x of climbColumns) {
    if (start.y === end.y) {
      const dip = ortho([start, { x: start.x, y: serviceY }, { x, y: serviceY }, { x, y: end.y }, end])
      if (dip.length > 2) list.push(dip)
    } else {
      const dive = ortho([start, { x: start.x, y: serviceY }, { x, y: serviceY }, end])
      if (dive.length > 2) list.push(dive)
    }
  }
  return list
}

export function reconcile(state, scene, { now = 0, fresh = false, reduced = false } = {}) {
  const signature = JSON.stringify(scene)
  if (signature === state.signature && !fresh && (!reduced || state.motions.size === 0)) return state
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
    // Every drafted parcel flies strictly one at a time. Serial flights
    // collapse the collision problem to mover-versus-resters: while one
    // parcel rides, every other painted box is static, so a candidate path
    // only has to clear the resting picture at its own start. That picture
    // matches the renderer frame for frame — landed parcels rest at their
    // new slot, unflown drafted parcels are held at their old slot by their
    // not-yet-started motion, idlers never move — and it shifts by exactly
    // one slot per commit, so re-checking after each landing is enough.
    const drafted = new Map()
    for (const [key, point] of layout.parcels) {
      const old = previous.parcels.get(key)
      if (!old) continue
      const running = state.motions.get(key)
      const start = running ? pointAlong(running.points, (now - running.start) / running.duration, running.dwell ?? 0, running.dwellAt ?? 0) : old
      if (old.station === point.station && old.x === point.x && old.y === point.y) continue
      drafted.set(key, { key, start, point, moved: old.station !== point.station })
    }
    if (drafted.size) {
      const slotColumns = [...new Set([...layout.parcels.values(), ...previous.parcels.values()].map(point => point.x))]
      const west = Math.min(...slotColumns) - 34
      const east = Math.max(...slotColumns) + 34
      const dipColumns = [...new Set([...slotColumns, west, east])]
      const deepY = Math.max(...[...previous.parcels.values(), ...layout.parcels.values()].map(point => point.y)) + SERVICE_ROW_OFFSET + PARCEL_CLEARANCE
      const upperY = Math.min(...[...previous.parcels.values(), ...layout.parcels.values()].map(point => point.y)) - SERVICE_ROW_OFFSET - PARCEL_CLEARANCE
      // A vanished parcel's slot is already vacant; keeping its ghost would
      // block a target forever.
      const resting = new Map([...previous.parcels].filter(([key]) => layout.parcels.has(key)))
      let chain = now
      const commit = (key, path) => {
        const duration = travelTime(path)
        next.motions.set(key, { points: path, start: chain, duration })
        chain += duration
      }
      // Candidate paths, cheapest first. Dips and tours ride a service lane
      // below the slot hall: local lanes first (short hops), the global deep
      // lane last. A tour may climb at an approach column east or west of the
      // target and dock horizontally, which is how a wrapped parcel rounds a
      // stacked column block through the empty corridor.
      const candidatePaths = (from, to) => {
        const list = []
        if (from.x === to.x || from.y === to.y) list.push([from, to])
        else list.push([from, { x: from.x, y: to.y }, to], [from, { x: to.x, y: from.y }, to])
        const lanes = [...new Set([to.y + SERVICE_ROW_OFFSET + PARCEL_CLEARANCE, from.y + SERVICE_ROW_OFFSET + PARCEL_CLEARANCE, deepY, to.y - SERVICE_ROW_OFFSET - PARCEL_CLEARANCE, from.y - SERVICE_ROW_OFFSET - PARCEL_CLEARANCE, upperY])]
        for (const lane of lanes) {
          list.push(...flightCandidates(from, to, lane, dipColumns))
          for (const column of [west, east, from.x]) {
            for (const approach of [to.x, west, east]) {
              list.push(dedupe(ortho([from, { x: column, y: from.y }, { x: column, y: lane }, { x: approach, y: lane }, { x: approach, y: to.y }, to])))
            }
          }
        }
        return list.map(dedupe).filter(points => points.length > 1).sort((left, right) => pathLength(left) - pathLength(right))
      }
      const clearPath = (flight, from = flight.start, to = flight.point) => {
        const rest = restIndex(resting, flight.key)
        const clear = points => flightClear(points, rest.byColumn, rest.byRow)
        const local = (start, finish) => candidatePaths(start, finish).find(clear)
        if (!flight.moved || to !== flight.point) return local(from, to)
        const source = stationOf(previous.parcels.get(flight.key).station)
        const target = layout.parcels.get(flight.key).station
        const route = travelPath(layout, source, target)
        const belt = trimDock(ortho([from, ...route.slice(1), to]))
        if (clear(belt)) return belt
        const serviceNodes = new Map([...layout.nodes].map(([station, node]) => {
          const index = mainline.indexOf(station)
          const serviceY = index < 0 ? layout.nodes.get('return').y + 35 : index < 4 ? upperY - PARCEL_CLEARANCE : layout.nodes.get('return').y - 35
          return [station, { ...node, x: ['review', 'pack'].includes(station) ? Math.max(880, east) + PARCEL_CLEARANCE : node.x, y: serviceY }]
        }))
        const service = travelPath({ nodes: serviceNodes }, source, target)
        const entry = local(from, service[0])
        const exit = local(service.at(-1), to)
        if (!entry || !exit) return undefined
        const detour = dedupe([...entry, ...service.slice(1), ...exit.slice(1)])
        return clear(detour) ? detour : undefined
      }
      // Movers leave first, easternmost departure ahead — a same-station
      // convoy then rides the belt in reverse order, each through slots the
      // previous flight just vacated. Displaced parcels follow in ascending
      // target slot: a refill chain unwinds head-first, each flight landing
      // in the slot the previous one emptied.
      const order = () => [...drafted.values()].sort((left, right) => {
        if (left.moved !== right.moved) return left.moved ? -1 : 1
        return left.moved ? right.start.x - left.start.x : left.point.y - right.point.y || left.point.x - right.point.x
      })
      const pending = new Set(drafted.keys())
      const parked = []
      let guard = drafted.size * 2 + 2
      while (pending.size && guard-- > 0) {
        const ready = order().find(flight => pending.has(flight.key) && clearPath(flight))
        if (ready) {
          const path = clearPath(ready)
          commit(ready.key, path)
          resting.set(ready.key, ready.point)
          pending.delete(ready.key)
          continue
        }
        // Nothing flies: an interlocked cycle. One member parks in the empty
        // corridor beside the hall — its old slot frees the chain behind it —
        // and settles into its real slot once everyone else landed. Parking
        // prefers the parcel whose old slot is another stuck parcel's target;
        // if no park path clears either, the loop stops and the remaining
        // parcels hold their position until the schedule ends.
        const stuck = order().filter(flight => pending.has(flight.key))
        const blocksTarget = one => stuck.some(other => other.key !== one.key
          && other.point.x === previous.parcels.get(one.key).x
          && other.point.y === previous.parcels.get(one.key).y)
        let staged = null
        for (const flight of stuck.sort((left, right) => (blocksTarget(right) ? 1 : 0) - (blocksTarget(left) ? 1 : 0) || left.point.x - right.point.x)) {
          const lane = flight.start.y + SERVICE_ROW_OFFSET + PARCEL_CLEARANCE
          const parkSlot = (column, depth) => ({ x: column, y: depth })
          for (const slot of [parkSlot(west, lane), parkSlot(east, lane), parkSlot(west, deepY), parkSlot(east, deepY)]) {
            const leg = clearPath(flight, flight.start, slot)
            if (leg && leg.at(-1).x === slot.x && leg.at(-1).y === slot.y) { staged = { flight, slot, leg }; break }
          }
          if (staged) break
        }
        if (!staged) break
        const { flight, slot, leg } = staged
        commit(flight.key, leg)
        resting.set(flight.key, slot)
        pending.delete(flight.key)
        parked.push({ flight, slot, leg, start: next.motions.get(flight.key).start, parkRide: travelTime(leg) })
      }
      // Parcels still pending when the loop stops keep their position until
      // the schedule ends, then the renderer snaps them to their slot with
      // nothing in flight.
      for (const key of pending) {
        const hold = drafted.get(key).start
        next.motions.set(key, { points: [hold, hold], start: chain, duration: 1 })
      }
      // Settles run after the chain, one at a time, in ascending target
      // order; a blocked settle retries on later rounds because every landed
      // settle changes the arrangement, and a settle that never clears keeps
      // the park position instead of looping forever.
      let cursor = chain
      let remaining = parked.slice().sort((left, right) => left.flight.point.y - right.flight.point.y || left.flight.point.x - right.flight.point.x)
      let progress = true
      while (remaining.length && progress) {
        progress = false
        for (const stage of remaining.slice()) {
          const dwell = Math.max(0, cursor - (stage.start + stage.parkRide))
          const leg = clearPath(stage.flight, stage.slot, stage.flight.point)
          if (!leg) continue
          const settleRide = travelTime(leg)
          const points = dedupe([...stage.leg.slice(0, -1), ...leg])
          const dwellAt = points.findIndex((point, index) => index > 0 && point.x === stage.slot.x && point.y === stage.slot.y)
          next.motions.set(stage.flight.key, { points, start: stage.start, duration: stage.parkRide + dwell + settleRide, dwell, dwellAt })
          resting.set(stage.flight.key, stage.flight.point)
          cursor = stage.start + stage.parkRide + dwell + settleRide
          remaining = remaining.filter(one => one !== stage)
          progress = true
        }
      }
      chain = Math.max(chain, cursor)
    }
  }
  return next
}
