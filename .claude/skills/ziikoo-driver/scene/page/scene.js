const token = new URL(import.meta.url).searchParams.get('t')
const resource = pathname => `${pathname}?t=${encodeURIComponent(token)}`
const [model, drawing] = await Promise.all([import(resource('/town.mjs')), import(resource('/draw.mjs'))])
const element = selector => document.querySelector(selector)
const canvas = element('#map')
const context = canvas.getContext('2d', { alpha: false })
const cache = document.createElement('canvas')
const cacheContext = cache.getContext('2d', { alpha: false })
const buffer = document.createElement('canvas')
const bufferContext = buffer.getContext('2d', { alpha: false })
const reduced = matchMedia('(prefers-reduced-motion: reduce)')
const failures = new Map()
const metrics = { resizes: 0, draws: 0, baseDraws: 0, visibleProjects: 0 }
let state = model.initialState()
let layout = model.townLayout([])
let connected = false
let fresh = true
let cacheDirty = true
let requested = false
let hits = []
let size = { width: 1, height: 1, ratio: 1 }
let drag = null
let pointerMoved = false
let detailMode = null
let snapshotRequest

const currentRow = () => state.scene?.rows.find(row => row.path === state.project)
const currentLayout = () => {
  if (state.view === 'town') return model.townLayout(state.scene?.rows ?? [])
  const row = currentRow()
  return { ...model.workshopLayout(row), row }
}
const viewport = () => ({ x: state.camera.x - size.width / state.camera.zoom / 2, y: state.camera.y - size.height / state.camera.zoom / 2, width: size.width / state.camera.zoom, height: size.height / state.camera.zoom })
const projectAnchor = () => model.townLayout(state.scene?.rows ?? []).projects.get(state.project)
const screenPoint = point => ({ x: (point.x - state.camera.x) * state.camera.zoom + size.width / 2, y: (point.y - state.camera.y) * state.camera.zoom + size.height / 2 })
function selectedPoint() {
  const selected = state.selection
  if (selected?.kind === 'parcel') return layout.parcels?.get(selected.key)
  if (selected?.kind === 'station') return layout.nodes?.get(selected.station)
  return state.view === 'town' ? projectAnchor() : { x: layout.bounds.x + layout.bounds.width / 2, y: layout.bounds.y + layout.bounds.height / 2 }
}
function requestDraw(staticChanged = false) {
  cacheDirty ||= staticChanged
  if (requested) return
  requested = true
  requestAnimationFrame(draw)
}
function transform(target) {
  target.setTransform(size.ratio, 0, 0, size.ratio, 0, 0)
  target.translate(size.width / 2, size.height / 2)
  target.scale(state.camera.zoom, state.camera.zoom)
  target.translate(-state.camera.x, -state.camera.y)
  target.imageSmoothingEnabled = false
}
function draw(now) {
  requested = false
  for (const [key, motion] of state.motions) if (now - motion.start >= motion.duration) state.motions.delete(key)
  const view = viewport()
  if (cacheDirty) {
    transform(cacheContext)
    drawing.paintBase(cacheContext, state, layout, view, model)
    metrics.baseDraws++
    cacheDirty = false
  }
  bufferContext.setTransform(1, 0, 0, 1, 0, 0)
  bufferContext.drawImage(cache, 0, 0)
  transform(bufferContext)
  hits = state.scene ? drawing.paintActors(bufferContext, state, layout, view, now, model, reduced.matches) : []
  context.setTransform(1, 0, 0, 1, 0, 0)
  context.drawImage(buffer, 0, 0)
  metrics.draws++
  metrics.visibleProjects = state.view === 'town' ? model.visibleProjects(layout, view).length : 0
  if (!reduced.matches && state.scene && (state.motions.size || connected && state.scene.state === 'ok' && state.scene.maintenance === 'running')) requestDraw()
}
function resize() {
  const bounds = canvas.getBoundingClientRect()
  const ratio = Math.min(devicePixelRatio || 1, 2)
  const width = Math.max(1, Math.round(bounds.width * ratio))
  const height = Math.max(1, Math.round(bounds.height * ratio))
  if (canvas.width === width && canvas.height === height) return
  buffer.width = width
  buffer.height = height
  bufferContext.fillStyle = '#b9c78c'
  bufferContext.fillRect(0, 0, width, height)
  bufferContext.drawImage(canvas, 0, 0, width, height)
  canvas.width = width
  canvas.height = height
  context.drawImage(buffer, 0, 0)
  cache.width = width
  cache.height = height
  size = { width: bounds.width, height: bounds.height, ratio }
  metrics.resizes++
  if (state.selection) keepVisible()
  requestDraw(true)
}
function focus(point, zoom) {
  if (!point) return
  state.camera = { x: point.x, y: point.y, zoom: zoom ?? state.camera.zoom }
  requestDraw(true)
}
function home() {
  if (state.view === 'town') { focus(projectAnchor(), 1); return }
  const bounds = canvas.getBoundingClientRect()
  if (bounds.width < 650) { focus({ x: layout.nodes.get('work').x, y: layout.nodes.get('work').y + 60 }, 1); return }
  focus(selectedPoint(), Math.max(.7, Math.min(1, (bounds.width - 36) / layout.bounds.width, (bounds.height - 48) / layout.bounds.height)))
}
function keepVisible() {
  const point = selectedPoint()
  if (!point) return
  const screen = screenPoint(point)
  if (screen.x < 25 || screen.x > size.width - 25 || screen.y < 55 || screen.y > size.height - 25) focus(point)
}
function setUrl() {
  const url = new URL(location.href)
  for (const name of ['project', 'parcel', 'view']) url.searchParams.delete(name)
  if (state.project) url.searchParams.set('project', state.project)
  if (state.view === 'workshop') url.searchParams.set('view', 'workshop')
  if (state.selection?.kind === 'parcel') url.searchParams.set('parcel', JSON.parse(state.selection.key)[1])
  history.replaceState(null, '', url)
}
function list(container, entries) {
  const existing = new Map([...container.children].map(button => [button.dataset.key, button]))
  const active = new Set()
  entries.forEach((entry, index) => {
    active.add(entry.key)
    let button = existing.get(entry.key)
    if (!button) {
      button = document.createElement('button')
      button.dataset.key = entry.key
      button.append(document.createElement('span'), document.createElement('span'))
      button.children[0].className = 'title'
      button.children[1].className = 'sub'
    }
    button.children[0].textContent = entry.title
    button.children[1].textContent = entry.subtitle ?? ''
    button.title = `${entry.title}\n${entry.subtitle ?? ''}`
    button.setAttribute('aria-current', String(Boolean(entry.selected)))
    button.onclick = entry.click
    if (container.children[index] !== button) container.insertBefore(button, container.children[index] ?? null)
  })
  for (const button of [...container.children]) if (!active.has(button.dataset.key)) button.remove()
}
function selectProject(path, enter = false) {
  if (!state.scene.rows.some(row => row.path === path)) return
  state.project = path
  state.selection = null
  state.motions.clear()
  state.view = enter ? 'workshop' : 'town'
  detailMode = enter ? 'project' : null
  layout = currentLayout()
  renderUI()
  if (enter) home()
  else focus(projectAnchor(), Math.max(.7, state.camera.zoom))
  setUrl()
}
function selectParcel(path, key) {
  if (state.project !== path || state.view !== 'workshop') selectProject(path, true)
  state.selection = { kind: 'parcel', key }
  detailMode = 'parcel'
  renderUI()
  focus(layout.parcels.get(key), Math.max(1, state.camera.zoom))
  setUrl()
}
function selectStation(station) {
  state.selection = { kind: 'station', station }
  detailMode = 'station'
  renderUI()
  focus(layout.nodes.get(station))
  setUrl()
}
function needsAttention(parcel) {
  return parcel.tone === 'attention' || parcel.tone === 'failed' || parcel.unknownDeploy || model.stationOf(parcel.station) === 'unknown' || ['paused', 'failed'].includes(parcel.deployment)
}
function renderUI() {
  const rows = state.scene?.rows ?? []
  const row = currentRow()
  const query = element('#search').value.trim().toLocaleLowerCase()
  const matches = value => String(value).toLocaleLowerCase().includes(query)
  const rowMatches = one => matches(one.name) || matches(one.path)
  const parcelMatches = one => matches(one.identifier) || matches(one.title)
  const visibleRows = rows.filter(one => rowMatches(one) || one.parcels.some(parcelMatches))
  list(element('#projects'), visibleRows.map(one => ({ key: one.path, title: `${one.name} · ${one.parcels.length}`, subtitle: one.path, selected: one.path === state.project, click: () => selectProject(one.path) })))
  const parcelRows = query ? rows : row ? [row] : []
  list(element('#parcels'), parcelRows.flatMap(one => one.parcels.filter(parcel => !query || rowMatches(one) || parcelMatches(parcel)).map(parcel => ({ key: model.parcelKey(one.path, parcel.identifier), title: `${parcel.identifier} · ${parcel.title}`, subtitle: `${one.path} · ${parcel.label} · ${parcel.duration}${needsAttention(parcel) ? ' · 需关注' : ''}`, selected: state.selection?.key === model.parcelKey(one.path, parcel.identifier), click: () => selectParcel(one.path, model.parcelKey(one.path, parcel.identifier)) }))))
  list(element('#nodes'), state.view === 'workshop' ? [...layout.nodes.values()].map(node => ({ key: node.station, title: `${model.stations[node.station][0]} ${node.count}`, selected: state.selection?.station === node.station, click: () => selectStation(node.station) })) : [])
  element('#project-count').textContent = `${rows.length} 座工坊 · 匹配 ${visibleRows.length}`
  element('#enter').disabled = !row
  element('#empty').hidden = rows.length !== 0
  element('#location').textContent = row ? `${state.view === 'town' ? '总览' : '工坊'} · ${row.name}` : '小镇总览'
  element('#location').title = row?.path ?? ''
  renderDetail()
  renderStatus()
  requestDraw()
}
function paragraph(value, className) {
  const item = document.createElement('p')
  item.textContent = value
  if (className) item.className = className
  element('#detail').append(item)
}
function renderDetail() {
  const panel = element('#detail')
  panel.hidden = !detailMode
  element('#workspace').classList.toggle('has-detail', Boolean(detailMode))
  if (!detailMode) { panel.replaceChildren(); return }
  const close = document.createElement('button')
  close.textContent = '收起详情'
  close.onclick = () => { detailMode = null; state.selection = null; renderUI(); setUrl(); element('#enter').focus() }
  const heading = document.createElement('h2')
  panel.replaceChildren(close, heading)
  const row = currentRow()
  if (detailMode === 'attention') {
    heading.textContent = '需要关注'
    let count = 0
    for (const project of state.scene?.rows ?? []) for (const parcel of project.parcels.filter(needsAttention)) {
      const button = document.createElement('button')
      button.textContent = `${project.name} · ${project.path} · ${parcel.identifier} · ${parcel.reason || parcel.label}`
      button.onclick = () => selectParcel(project.path, model.parcelKey(project.path, parcel.identifier))
      panel.append(button)
      count++
    }
    paragraph(count ? `${count} 张票需要关注，保留真实原因。` : '当前没有需要关注的票据。')
    return
  }
  if (!row) return
  if (detailMode === 'parcel') {
    const parcel = layout.parcels.get(state.selection?.key)?.parcel
    if (!parcel) { heading.textContent = '票据已不在当前快照'; paragraph('消失原因未知，不据此认定完成。'); return }
    heading.textContent = parcel.identifier
    paragraph(parcel.title)
    paragraph(`项目：${row.name}\n路径：${row.path}\n工位：${model.stations[model.stationOf(parcel.station)][0]}${model.stationOf(parcel.station) === 'unknown' ? `（${parcel.station}）` : ''}\n真实阶段：${parcel.phase} / ${parcel.label}\n停留时长：${parcel.duration}\n状态：${parcel.tone}`)
    paragraph(`原因：${parcel.reason || parcel.blocker || '未提供'}`, needsAttention(parcel) ? 'reason' : '')
    paragraph(`部署：${!row.deploy ? '不适用 · 未配置部署' : parcel.deployment || (parcel.unknownDeploy ? '未知' : '未提供')}${parcel.unknownDeploy ? '\n部署字段：未知 · driver 未提供 deployment' : ''}${parcel.deployment ? `\n部署原始状态：${parcel.deployment}` : ''}`)
    return
  }
  if (detailMode === 'station') {
    const station = state.selection.station
    heading.textContent = model.stations[station][0]
    paragraph(model.stations[station][1])
    paragraph(`当前票据：${layout.nodes.get(station).count} 张`)
    if (!row.deploy && ['dispatch', 'smoke'].includes(station)) paragraph('不适用 · 项目未配置部署')
    return
  }
  heading.textContent = row.name
  paragraph(`路径：${row.path}\n票据：${row.parcels.length} 张\n执行位：${row.workers}\n部署：${row.deploy ? '已配置' : '不适用 · 未配置部署'}`)
  paragraph('主线：领取、施工、门禁、审查、开 PR、排队合并、部署、smoke。异常沿修理、返工、人工支线处理。')
  paragraph('这里只显示 Scene 已提供的数据。包裹排列不代表真实队列顺序，包裹消失不代表完成。')
}
function renderStatus() {
  const scene = state.scene
  const messages = [connected ? '已连接 · 实时只读' : '连接断开 · 正在重连，保留最近快照']
  if (scene) {
    if (scene.state !== 'ok' || Date.now() - scene.generatedAt > 20000) messages.push(`快照过期或不可用：${scene.state}`)
    if (scene.maintenance !== 'running') messages.push(`维护模式：${scene.maintenance}`)
    const deploymentFailures = scene.rows.flatMap(row => row.parcels).filter(parcel => ['paused', 'failed'].includes(parcel.deployment))
    if (deploymentFailures.length) messages.push(`部署需要关注：${deploymentFailures.length} 张票（${[...new Set(deploymentFailures.map(parcel => parcel.deployment))].join(' / ')}）`)
    element('#snapshot').textContent = `快照 ${new Date(scene.generatedAt).toLocaleString()} · ${scene.rows.length} 个项目`
  }
  for (const failure of failures.values()) messages.push(`${failure.command} 命令错误（${failure.exitCode ?? '未知退出码'}）：${failure.reason}`)
  element('#status').textContent = messages.join('\n')
}
function receive(scene) {
  const previous = state
  const requestedUrl = !state.scene ? new URL(location.href) : null
  state = model.reconcile(state, scene, { now: performance.now(), fresh, reduced: reduced.matches })
  fresh = false
  if (state === previous) { renderStatus(); return }
  if (previous.project && previous.project !== state.project) detailMode = null
  if (previous.selection && !state.selection) detailMode = null
  layout = currentLayout()
  if (requestedUrl) {
    const path = requestedUrl.searchParams.get('project')
    if (scene.rows.some(row => row.path === path)) {
      selectProject(path, requestedUrl.searchParams.get('view') === 'workshop')
      const identifier = requestedUrl.searchParams.get('parcel')
      if (identifier && currentRow().parcels.some(parcel => parcel.identifier === identifier)) selectParcel(path, model.parcelKey(path, identifier))
    }
  }
  renderUI()
  setUrl()
  requestDraw(true)
}
async function fetchSnapshot() {
  const controller = new AbortController()
  snapshotRequest?.abort()
  snapshotRequest = controller
  try {
    const response = await fetch(resource('/snapshot'), { signal: controller.signal })
    if (!response.ok) throw new Error(`快照 HTTP ${response.status}`)
    const scene = await response.json()
    if (controller === snapshotRequest) receive(scene)
  } catch (error) {
    if (error.name !== 'AbortError') { failures.set('snapshot', { command: 'snapshot', reason: error.message }); renderStatus() }
  }
}
element('#overview').onclick = () => {
  state.view = 'town'
  state.selection = null
  state.motions.clear()
  detailMode = null
  layout = currentLayout()
  focus(projectAnchor(), 1)
  renderUI()
  setUrl()
}
element('#enter').onclick = () => { if (state.project) selectProject(state.project, true) }
element('#attention').onclick = () => { detailMode = 'attention'; renderUI() }
element('#search').addEventListener('input', renderUI)
function zoom(factor) { state.camera.zoom = Math.max(.55, Math.min(2.5, state.camera.zoom * factor)); requestDraw(true) }
element('#zoom-in').onclick = () => zoom(1.2)
element('#zoom-out').onclick = () => zoom(1 / 1.2)
element('#reset').onclick = () => state.selection ? focus(selectedPoint(), 1) : home()
canvas.addEventListener('wheel', event => { event.preventDefault(); zoom(event.deltaY < 0 ? 1.1 : 1 / 1.1) }, { passive: false })
canvas.addEventListener('pointerdown', event => {
  canvas.setPointerCapture(event.pointerId)
  drag = { x: event.clientX, y: event.clientY, camera: { ...state.camera } }
  pointerMoved = false
})
canvas.addEventListener('pointermove', event => {
  if (!drag) return
  const deltaX = event.clientX - drag.x
  const deltaY = event.clientY - drag.y
  pointerMoved ||= Math.abs(deltaX) + Math.abs(deltaY) > 4
  state.camera.x = drag.camera.x - deltaX / state.camera.zoom
  state.camera.y = drag.camera.y - deltaY / state.camera.zoom
  requestDraw(true)
})
canvas.addEventListener('pointercancel', () => { drag = null })
canvas.addEventListener('pointerup', event => {
  drag = null
  if (pointerMoved) return
  const bounds = canvas.getBoundingClientRect()
  const point = { x: (event.clientX - bounds.left - size.width / 2) / state.camera.zoom + state.camera.x, y: (event.clientY - bounds.top - size.height / 2) / state.camera.zoom + state.camera.y }
  const hit = [...hits].reverse().find(one => point.x >= one.x && point.x <= one.x + one.width && point.y >= one.y && point.y <= one.y + one.height)
  if (hit?.kind === 'project') selectProject(hit.path, true)
  else if (hit?.kind === 'parcel') selectParcel(state.project, hit.key)
  else if (hit?.kind === 'station') selectStation(hit.station)
})
canvas.addEventListener('keydown', event => {
  const steps = { ArrowLeft: [-60, 0], ArrowRight: [60, 0], ArrowUp: [0, -60], ArrowDown: [0, 60] }
  if (steps[event.key]) {
    event.preventDefault()
    state.camera.x += steps[event.key][0] / state.camera.zoom
    state.camera.y += steps[event.key][1] / state.camera.zoom
    requestDraw(true)
  } else if (event.key === '+' || event.key === '=') zoom(1.2)
  else if (event.key === '-') zoom(1 / 1.2)
  else if (event.key === 'Home') { event.preventDefault(); element('#reset').click() }
})
reduced.addEventListener('change', () => { if (reduced.matches) state.motions.clear(); requestDraw() })
new ResizeObserver(resize).observe(element('#viewport'))
resize()
const events = new EventSource(resource('/events'))
events.onopen = () => { connected = true; fresh = true; failures.delete('snapshot'); renderStatus(); requestDraw() }
events.addEventListener('scene', event => { snapshotRequest?.abort(); failures.delete('snapshot'); receive(JSON.parse(event.data)) })
events.addEventListener('error', event => {
  if (event.data) { const failure = JSON.parse(event.data); if (failure.recovered) failures.delete(failure.command); else failures.set(failure.command, failure) }
  else { connected = false; fresh = true }
  renderStatus()
})
setInterval(renderStatus, 5000)
void fetchSnapshot()
window.addEventListener('pagehide', () => { events.close(); snapshotRequest?.abort() }, { once: true })
window.pixelTown = Object.freeze({ get state() { return state }, get layout() { return layout }, metrics, selectedScreenPoint() { const point = selectedPoint(); if (!point) return null; const bounds = canvas.getBoundingClientRect(); const screen = screenPoint(point); return { x: bounds.left + screen.x, y: bounds.top + screen.y } } })
