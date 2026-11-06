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

// A motion may dwell at a named waypoint: the tween's timeline splits into
// the ride up to points[dwellAt], the dwell wait, and the ride on, so one
// motion can stage aside, let later flights pass, and settle — which a pure
// polyline cannot express. Callers that ignore dwell see the ride only.
export function pointAlong(points, progress, dwell = 0, dwellAt = 0) {
  if (progress >= 1) return points.at(-1)
  if (!dwell || dwellAt <= 0 || dwellAt >= points.length - 1) {
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
  const stageLength = pathLength(stage)
  const settleLength = pathLength(settle)
  const total = stageLength + dwell + settleLength
  const clock = Math.max(0, Math.min(1, progress)) * total
  if (clock <= stageLength) return pointAlong(stage, stageLength > 0 ? clock / stageLength : 1)
  if (clock <= stageLength + dwell) return points[dwellAt]
  return pointAlong(settle, settleLength > 0 ? (clock - stageLength - dwell) / settleLength : 1)
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
    // A crowded hall (dozens of drafted parcels, like a whole-station refill)
    // is a sliding puzzle whose serial solution is unbounded; it rides the
    // simultaneous rigid convoy instead. Small sets fly one at a time.
    const crowdKeys = [...layout.parcels.keys()].filter(key => {
      const old = previous.parcels.get(key)
      const point = layout.parcels.get(key)
      return old && (old.station !== point.station || old.x !== point.x || old.y !== point.y)
    })
    if (crowdKeys.length > 24) {
      // A crowded refill unwinds staggered: movers leave first through the
      // deep lane under the hall (a packed belt row leaves no other route),
      // then each displaced parcel slides while everyone else rests, so
      // neighbors keep their 34px spacing and a wrap parcel only crosses rows
      // that earlier stagger steps vacated.
      const stage = key => {
        const old = previous.parcels.get(key)
        const point = layout.parcels.get(key)
        if (old.station !== point.station) return 0
        return old.y === point.y ? 1 : 2
      }
      const ordered = [...crowdKeys].sort((left, right) => stage(left) - stage(right)
        || layout.parcels.get(left).y - layout.parcels.get(right).y
        || layout.parcels.get(left).x - layout.parcels.get(right).x)
      const slotColumnsAll = [...new Set([...layout.parcels.values(), ...previous.parcels.values()].map(point => point.x))]
      const west = Math.min(...slotColumnsAll) - 34
      const deepY = Math.max(...[...previous.parcels.values(), ...layout.parcels.values()].map(point => point.y)) + SERVICE_ROW_OFFSET + PARCEL_CLEARANCE
      let slot = 0
      for (const key of ordered) {
        const old = previous.parcels.get(key)
        const point = layout.parcels.get(key)
        const running = state.motions.get(key)
        const start = running ? pointAlong(running.points, (now - running.start) / running.duration, running.dwell ?? 0, running.dwellAt ?? 0) : old
        const begin = now + slot * 80
        const deep = stage(key) !== 1
        const path = deep
          ? dedupe(ortho([start, { x: west, y: start.y }, { x: west, y: deepY }, { x: point.x, y: deepY }, point]))
          : dedupe(ortho([start, ...elbow(start, point), point]))
        next.motions.set(key, { points: path, start: begin, duration: Math.max(240, Math.round(pathLength(path) / BELT_PX_PER_MS)) })
        slot++
      }
      return next
    }
    // One parcel flies at a time. While it flies, every other parcel rests at
    // its current slot (pure idlers at their unchanged slot, earlier flights
    // at their new slot), so the mover-versus-resters clearance check alone
    // guarantees no two painted boxes ever intersect during the tween.
    const drafted = new Map()
    for (const [key, point] of layout.parcels) {
      const old = previous.parcels.get(key)
      if (!old) continue
      const running = state.motions.get(key)
      const start = running ? pointAlong(running.points, (now - running.start) / running.duration, running.dwell ?? 0, running.dwellAt ?? 0) : old
      const moved = old.station !== point.station
      const displaced = old.x !== point.x || old.y !== point.y
      if (!moved && !displaced) continue
      drafted.set(key, { start, point, moved, points: null, key })
    }
    // Belt columns and slot columns a dip may descend on; slot columns come
    // from the union of both layouts so vacated columns count too.
    // Dip columns are every slot column of either layout plus stepping stones
    // 34px outside the occupied span on both sides, so a settle can round a
    // stacked column block through the empty corridor beyond the row.
    const slotColumns = [...new Set([...layout.parcels.values(), ...previous.parcels.values()].map(point => point.x))]
    const spanLow = Math.min(...slotColumns) - 34
    const spanHigh = Math.max(...slotColumns) + 34
    const dipColumns = [...new Set([...slotColumns, spanLow, spanHigh])]
    const resting = new Map(previous.parcels)
    // Displaced parcels fly first in ascending target slot order: a refill
    // chain (every parcel sliding one slot toward the vacated one) unwinds
    // head-first, each flight landing in the slot the previous flight just
    // emptied. Movers fly last, rightmost departure column ahead, because a
    // crowded hall only clears its belt rows once the chain has settled.
    const slotIndex = point => Math.round((point.y - layout.parcels.values().next().value.y) / 44) * 4 + Math.round((point.x - Math.min(...[...layout.parcels.values()].map(one => one.x))) / 34)
    const flightOrder = [...drafted.values()].sort((left, right) => {
      if (left.moved !== right.moved) return left.moved ? 1 : -1
      return left.moved ? right.start.x - left.start.x : slotIndex(left.point) - slotIndex(right.point)
    })
    // A mover rides the painted belt (a backward move rides it backwards);
    // a displaced parcel takes a straight L or a service-row dip. Flights
    // whose target slot another unsettled parcel still occupies are deferred
    // until that parcel vacates; a cycle defers one member to a park slot
    // beyond its row's last parcel, breaking the deadlock.
    const schedulePath = flight => {
      const restIndexNow = restIndex(resting, flight.key)
      if (flight.moved) {
        const route = travelPath(layout, stationOf(previous.parcels.get(flight.key).station), flight.point.station)
        const belt = trimDock(ortho([flight.start, ...route.slice(1), flight.point]))
        const beltRun = belt.length > 1 ? belt : [flight.start, flight.point]
        const candidates = [beltRun]
        for (let index = 1; index < beltRun.length; index++) {
          const from = beltRun[index - 1]
          const to = beltRun[index]
          if (from.y !== to.y) continue
          const serviceY = flight.point.y + SERVICE_ROW_OFFSET + PARCEL_CLEARANCE
          for (const x of dipColumns) {
            const dipped = laneDetour(beltRun, new Set([x]), serviceY)
            if (dipped) candidates.push(dipped)
          }
          // laneDetour dives on the hop's own column; a dive between endpoints
          // needs the approach along the blocked row, so only hops free of row
          // resters up to x can use it.
          for (const x of dipColumns) {
            const approach = [from, { x, y: from.y }]
            if (!flightClear(approach, restIndexNow.byColumn, restIndexNow.byRow)) continue
            const dipped = ortho([{ x, y: from.y }, { x, y: serviceY }, { x: to.x, y: serviceY }, to])
            candidates.push(dedupe([...beltRun.slice(0, index), ...dipped]))
          }
        }
        return candidates.find(points => flightClear(points, restIndexNow.byColumn, restIndexNow.byRow))
      }
      const serviceY = flight.point.y + SERVICE_ROW_OFFSET + PARCEL_CLEARANCE
      return flightCandidates(flight.start, flight.point, serviceY, dipColumns)
        .find(points => flightClear(points, restIndexNow.byColumn, restIndexNow.byRow))
    }
    const queue = flightOrder.slice()
    const deferred = []
    // Parked parcels settle after the whole chain: their single motion rides
    // to the park slot, dwells exactly until every other flight landed, then
    // settles into the real slot. The dwell span is only known once the loop
    // finishes, so the settle leg is appended afterwards.
    const settles = []
    const enqueue = (list, flight) => { if (!list.some(one => one.key === flight.key)) list.push(flight) }
    let chain = now
    let requeues = 0
    const ride = path => Math.max(240, Math.round(pathLength(path) / BELT_PX_PER_MS))
    const commit = (flight, path, dwell = 0) => {
      const duration = Math.max(ride(path), dwell)
      next.motions.set(flight.key, { points: path, start: chain, duration, dwell })
      chain += duration
      resting.set(flight.key, flight.point)
    }
    while (queue.length || deferred.length) {
      if (!queue.length) {
        // Every remaining flight is deferred: an interlocked cycle. Its
        // lowest-target member stages rightward along its row in one motion:
        // ride to a clear park slot, dwell while the rest of the cycle flies
        // to their final slots, then settle. The settle leg is checked
        // against the cycle's final arrangement, not the current one.
        const parked = deferred.find(one => !next.motions.has(one.key))
          ?? deferred.reduce((low, one) => one.point.x < low.point.x || (one.point.x === low.point.x && one.point.y < low.point.y) ? one : low)
        const indexNow = restIndex(resting, parked.key)
        let parkLeg = null
        let parkSlot = null
        const parkSteps = []
        for (let step = 1; step <= 12; step++) { parkSteps.push(step, -step) }
        for (const step of parkSteps) {
          if (parkLeg) break
          parkSlot = { x: parked.start.x + 34 * step, y: parked.start.y }
          // The park column must be empty on every other row: the settle dive
          // descends it, and later parks must not stack under a dweller.
          const occupied = [...(indexNow.byColumn.get(parkSlot.x) ?? [])].some(point => point.y !== parkSlot.y)
          if (occupied) continue
          parkLeg = flightCandidates(parked.start, parkSlot, parkSlot.y + SERVICE_ROW_OFFSET + PARCEL_CLEARANCE, dipColumns)
            .find(points => flightClear(points, indexNow.byColumn, indexNow.byRow))
        }
        const others = deferred.filter(one => one !== parked)
        const finalIndex = restIndex(new Map([...resting, ...others.map(one => [one.key, one.point])]), parked.key)
        const settleLeg = flightCandidates(parkSlot, parked.point, parked.point.y + SERVICE_ROW_OFFSET + PARCEL_CLEARANCE, dipColumns)
          .find(points => flightClear(points, finalIndex.byColumn, finalIndex.byRow))
        if (!parkLeg || !settleLeg) { for (const one of others) enqueue(queue, one); enqueue(queue, parked); deferred.length = 0; requeues = 0; continue }
        commit(parked, parkLeg)
        settles.push({ key: parked.key, park: parkSlot, parkLeg, settleLeg, start: next.motions.get(parked.key).start, parkRide: ride(parkLeg) })
        resting.set(parked.key, parkSlot)
        for (const one of others) enqueue(queue, one)
        deferred.length = 0
        requeues = 0
        continue
      }
      const flight = queue.shift()
      if (next.motions.has(flight.key)) continue
      const path = schedulePath(flight)
      if (path) { commit(flight, path); requeues = 0; continue }
      const index = restIndex(resting, flight.key)
      const endpointBlocked = [...(index.byRow.get(flight.point.y) ?? [])]
        .some(point => Math.max(Math.abs(point.x - flight.point.x), Math.abs(point.y - flight.point.y)) < PARCEL_CLEARANCE)
      if (endpointBlocked) { enqueue(deferred, flight); continue }
      if (requeues < queue.length + deferred.length) { enqueue(queue, flight); requeues++; continue }
      enqueue(deferred, flight)
      for (const queued of queue.splice(0, queue.length)) enqueue(deferred, queued)
      // Every order was tried: the deep-water lane under the slot hall. Dive
      // on the nearest column whose full drop is clear, ride beneath every
      // row, and climb on the target column.
      const deepY = Math.max(flight.start.y, flight.point.y, ...index.byRow.keys()) + SERVICE_ROW_OFFSET + PARCEL_CLEARANCE
      const columns = [flight.start.x, ...dipColumns].sort((left, right) => Math.abs(left - flight.start.x) - Math.abs(right - flight.start.x))
      const drop = x => flightClear([flight.start, { x: flight.start.x, y: flight.point.y }, { x, y: flight.point.y }], index.byColumn, index.byRow)
        && flightClear([{ x, y: flight.point.y }, { x, y: deepY }, { x: flight.point.x, y: deepY }, flight.point], index.byColumn, index.byRow)
      const descent = columns.find(drop)
      if (descent === undefined) {
        // No column drops clear: this flight yields the floor so the parked
        // cycle branch can stage a blocker; if even that fails, a straight
        // settle is the last resort.
        enqueue(deferred, flight)
        continue
      }
      commit(flight, dedupe(ortho([flight.start, { x: flight.start.x, y: flight.point.y }, { x: descent, y: flight.point.y }, { x: descent, y: deepY }, { x: flight.point.x, y: deepY }, flight.point])))
    }
    let cursor = chain
    for (const settle of settles) {
      const motion = next.motions.get(settle.key)
      const dwell = Math.max(0, cursor - (settle.start + settle.parkRide))
      const settleRide = ride(settle.settleLeg)
      // At settle time every dweller still rests at its park slot (they settle
      // after this one), every plain flight at its final slot.
      const atSettle = restIndex(resting, settle.key)
      const points = dedupe([...settle.parkLeg.slice(0, -1), ...settle.settleLeg])
      const dwellAt = points.findIndex((point, index) => index > 0 && point.x === settle.park.x && point.y === settle.park.y)
      next.motions.set(settle.key, { points, start: motion.start, duration: settle.parkRide + dwell + settleRide, dwell, dwellAt })
      if (!flightClear(settle.settleLeg, atSettle.byColumn, atSettle.byRow)) {
        const target = settle.settleLeg.at(-1)
        const deepY = Math.max(settle.park.y, target.y, ...atSettle.byRow.keys()) + SERVICE_ROW_OFFSET + PARCEL_CLEARANCE
        const columns = [...new Set([settle.park.x, target.x, ...dipColumns])]
          .sort((left, right) => Math.abs(left - settle.park.x) - Math.abs(right - settle.park.x))
        const leg = columns.map(x => dedupe(ortho([settle.park, { x: settle.park.x, y: deepY }, { x, y: deepY }, { x, y: target.y }, target])))
          .find(points => flightClear(points, atSettle.byColumn, atSettle.byRow))
        if (leg) {
          const legRide = ride(leg)
          const fallbackPoints = dedupe([...settle.parkLeg.slice(0, -1), ...leg])
          const fallbackAt = fallbackPoints.findIndex((point, index) => index > 0 && point.x === settle.park.x && point.y === settle.park.y)
          next.motions.set(settle.key, { points: fallbackPoints, start: motion.start, duration: settle.parkRide + dwell + legRide, dwell, dwellAt: fallbackAt })
          cursor = settle.start + settle.parkRide + dwell + legRide
          resting.set(settle.key, leg.at(-1))
          continue
        }
      }
      cursor = settle.start + settle.parkRide + dwell + settleRide
      resting.set(settle.key, settle.settleLeg.at(-1))
    }
    chain = Math.max(chain, cursor)
  }
  return next
}
