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
    tickets.forEach((parcel, index) => parcels.set(parcelKey(row.path, parcel.identifier), { x: node.x - 51 + index % 4 * 34, y: node.y + 85 + Math.floor(index / 4) * 44, station, parcel }))
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
    for (const [key, point] of layout.parcels) {
      const old = previous.parcels.get(key)
      if (!old) continue
      const running = state.motions.get(key)
      const start = running ? pointAlong(running.points, (now - running.start) / running.duration) : old
      const moved = old.station !== point.station
      const displaced = old.x !== point.x || old.y !== point.y
      if (!moved && !displaced) continue
      // Parcels rest beside their station's belt lane. The tween rides the
      // parcel slot row for the long segments (parallel to the painted belt, so
      // a convoy keeps its spacing instead of collapsing onto the lane) and
      // uses the painted belt lane itself for the corner steps between rows,
      // so every turn happens on the belt the eye can see.
      let points
      if (moved) {
        const route = travelPath(layout, old.station, point.station)
        const offset = { x: start.x - route[0].x, y: start.y - route[0].y }
        const riding = route.map(node => ({ x: node.x + offset.x, y: node.y + offset.y }))
        const step = (from, to) => (Math.abs(from.x - to.x) + Math.abs(from.y - to.y) ? [...elbow(from, to), to] : [])
        const core = [start, ...step(start, riding[0]), ...riding.slice(1), ...step(route.at(-1), point)]
        points = core.filter((node, index) => index === 0 || node.x !== core[index - 1].x || node.y !== core[index - 1].y)
      } else {
        points = [start, ...elbow(start, point), point]
      }
      next.motions.set(key, { points, start: now, duration: moved ? 1800 : 600 })
    }
  }
  return next
}
