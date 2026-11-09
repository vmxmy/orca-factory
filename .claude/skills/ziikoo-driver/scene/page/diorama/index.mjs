import { Vector3 } from 'three'

import { createCamera } from './camera.mjs'
import { createHud } from './hud.mjs'
import { islandOf, skyOf } from './model.mjs'
import { createWorld } from './world.mjs'

const CLICK_TOLERANCE = 6
const WHEEL_STEP = 1.1
const LABEL_MIN_WIDTH = 620
const LABEL_GAP = 2
const STATUS_INTERVAL = 1000
const NUDGES = { ArrowLeft: [-1, 0], ArrowRight: [1, 0], ArrowUp: [0, 1], ArrowDown: [0, -1] }

const stationLabels = ({ layout, working, workers }) => [
  ['主账本', layout.tower, 2.6, 0],
  [`工作间 · ${working}/${workers}`, layout.booths.reduce((sum, booth) => sum + booth.x, 0) / layout.booths.length, 1.9, layout.booths[0].z],
  ['检验间 · gate', -1, 2.2, 0],
  ['审查', 2.5, 2.7, 0],
  ['封箱 · 开 PR', 5.5, 0.6, 0],
  ['栈桥 · 合并队列', 9.3, 0.5, 0],
  ['船闸 · 部署屏障', 11.2, 1.6, 3],
  ['灯塔 · smoke', 16.2, 4, 6.6],
  ['回流坡道 · correction', -6.5, 1.9, -4.6],
  ['处置塔', -1, 5.3, -6.3],
  ['等人批', 3.4, 4.5, -6.2],
]

const tag = (parent, className, text) => {
  const node = document.createElement('span')
  node.className = className
  node.textContent = text
  parent.append(node)
  return node
}

export const mountDiorama = (stage, { scene, path, now = () => Date.now() }) => {
  const row = scene.rows.find(one => one.path === path)
  if (!row) throw new Error(`Scene 里没有项目 ${path}`)
  const island = islandOf(row)
  const motion = matchMedia('(prefers-reduced-motion: reduce)')
  let reduced = motion.matches
  motion.addEventListener('change', () => {
    reduced = motion.matches
  })

  const canvas = document.createElement('canvas')
  canvas.tabIndex = 0
  canvas.setAttribute('aria-label', `${island.name} 工坊沙盘。拖动平移，滚轮缩放，数字键 2 回工坊、3 看这张票，方向键平移，Home 复位，Esc 退一级。票也可以从右侧列表选择。`)
  const labels = document.createElement('div')
  labels.className = 'labels'
  stage.append(canvas, labels)

  const world = createWorld(canvas, island)
  const lens = createCamera(island.layout)
  const parcelOf = identifier => island.parcels.find(parcel => parcel.identifier === identifier)
  let stop = 2
  let selected = null

  const setStop = next => {
    const parcel = parcelOf(selected)
    stop = next === 3 && parcel ? 3 : 2
    lens.resize(stage.clientWidth, stage.clientHeight, hud.margins())
    if (stop === 3) lens.frameSlot(parcel.slot)
    else lens.frameIsland()
    if (reduced) lens.snap()
    hud.stops(stop, parcel !== undefined)
  }

  const select = identifier => {
    selected = parcelOf(identifier) ? identifier : null
    world.select(selected)
    hud.trip(parcelOf(selected))
    if (stop === 3) setStop(selected ? 3 : 2)
    else hud.stops(stop, selected !== null)
  }

  const hud = createHud(stage, island, { onSelect: select, onStop: setStop })
  hud.trip(undefined)
  hud.stops(stop, false)

  const stations = stationLabels(island).map(([text, x, y, z]) => ({
    node: tag(labels, 'label', text),
    position: new Vector3(x, y, z),
  }))
  const tickets = island.parcels.map(parcel => ({
    parcel,
    position: new Vector3(0, parcel.alert ? 2 : 1.1, 0).add(world.parcels.get(parcel.identifier).group.position),
    node: tag(labels, ['label', 'ticket', parcel.alert ?? ''].join(' ').trim(), parcel.unknown ? `${parcel.identifier} · 未知` : parcel.identifier),
  }))
  const overlaps = (a, b) => a.left < b.right && b.left < a.right && a.top < b.bottom && b.top < a.bottom
  const settle = (rect, taken) => {
    const blocker = taken.find(other => overlaps(rect, other))
    if (!blocker) return rect
    const lift = rect.bottom - blocker.top + LABEL_GAP
    return settle({ ...rect, top: rect.top - lift, bottom: rect.bottom - lift }, taken)
  }
  const place = (node, position, taken) => {
    node.hidden = false
    const point = position.clone().project(lens.camera)
    const x = ((point.x + 1) / 2) * stage.clientWidth
    const bottom = ((1 - point.y) / 2) * stage.clientHeight - 6
    const half = node.offsetWidth / 2
    const rect = settle({ left: x - half, right: x + half, top: bottom - node.offsetHeight, bottom }, taken)
    node.style.left = `${x}px`
    node.style.top = `${rect.bottom}px`
    taken.push(rect)
  }
  const placeLabels = () => {
    const taken = []
    const shown = tickets
      .filter(ticket => ticket.parcel.alert !== null || ticket.parcel.identifier === selected)
      .sort((left, right) => Number(right.parcel.identifier === selected) - Number(left.parcel.identifier === selected))
    for (const ticket of tickets) ticket.node.hidden = !shown.includes(ticket)
    for (const ticket of shown) place(ticket.node, ticket.position, taken)
    const wide = stage.clientWidth > LABEL_MIN_WIDTH
    for (const station of stations) {
      station.node.hidden = !wide
      if (wide) place(station.node, station.position, taken)
    }
  }

  const pointers = new Map()
  let travelled = 0
  let pinch = 0
  canvas.addEventListener('pointerdown', event => {
    canvas.setPointerCapture(event.pointerId)
    pointers.set(event.pointerId, [event.clientX, event.clientY])
    travelled = 0
    pinch = 0
  })
  canvas.addEventListener('pointermove', event => {
    const last = pointers.get(event.pointerId)
    if (!last) return
    const dx = event.clientX - last[0]
    const dy = event.clientY - last[1]
    pointers.set(event.pointerId, [event.clientX, event.clientY])
    if (pointers.size === 2) {
      const [a, b] = [...pointers.values()]
      const distance = Math.hypot(a[0] - b[0], a[1] - b[1])
      if (pinch) lens.zoom(pinch / distance)
      pinch = distance
      travelled = CLICK_TOLERANCE + 1
      return
    }
    travelled += Math.abs(dx) + Math.abs(dy)
    lens.drag(dx, dy)
  })
  canvas.addEventListener('pointerup', event => {
    pointers.delete(event.pointerId)
    if (travelled > CLICK_TOLERANCE) return
    const bounds = canvas.getBoundingClientRect()
    const ndc = {
      x: ((event.clientX - bounds.left) / bounds.width) * 2 - 1,
      y: -((event.clientY - bounds.top) / bounds.height) * 2 + 1,
    }
    select(world.pick(ndc, lens.camera))
  })
  canvas.addEventListener('pointercancel', event => pointers.delete(event.pointerId))
  canvas.addEventListener('dblclick', () => {
    if (selected) setStop(3)
  })
  canvas.addEventListener('wheel', event => {
    event.preventDefault()
    lens.zoom(event.deltaY > 0 ? WHEEL_STEP : 1 / WHEEL_STEP)
  }, { passive: false })
  stage.addEventListener('keydown', event => {
    const key = event.key
    if (key === '2' || key === '3') setStop(Number(key))
    else if (key === 'Home') setStop(stop)
    else if (key === 'Escape' && stop === 3) setStop(2)
    else if (key === 'Escape' && selected) select(null)
    else if (NUDGES[key]) lens.nudge(...NUDGES[key])
    else return
    event.preventDefault()
  })

  const refreshStatus = () => {
    const at = now()
    const sky = skyOf(scene, at - scene.generatedAt)
    world.setSky(sky)
    hud.status(scene, at, sky)
  }
  refreshStatus()
  setInterval(refreshStatus, STATUS_INTERVAL)

  new ResizeObserver(() => {
    world.resize(stage.clientWidth, stage.clientHeight)
    setStop(stop)
    lens.snap()
  }).observe(stage)

  const frame = time => {
    lens.step(reduced)
    world.animate(time, reduced)
    placeLabels()
    world.render(lens.camera)
    requestAnimationFrame(frame)
  }
  requestAnimationFrame(frame)

  return {
    island,
    state: () => ({
      stop,
      selected,
      reduced,
      view: { ...lens.view },
      goal: { ...lens.goal },
      quaternion: lens.camera.quaternion.toArray(),
      marks: [...world.parcels.values()]
        .filter(parcel => parcel.mark.visible)
        .map(parcel => ({ y: parcel.mark.position.y, rotation: parcel.mark.rotation.y })),
    }),
  }
}
