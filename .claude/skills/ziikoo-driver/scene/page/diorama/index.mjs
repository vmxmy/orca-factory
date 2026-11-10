import { createCamera } from './camera.mjs'
import { createHud, hubText } from './hud.mjs'
import { archipelagoOf, skyOf } from './model.mjs'
import { createOverlay } from './overlay.mjs'
import { createWorld } from './world.mjs'

const CLICK_TOLERANCE = 6
const WHEEL_STEP = 1.1
const STATUS_INTERVAL = 1000
const NUDGES = { ArrowLeft: [-1, 0], ArrowRight: [1, 0], ArrowUp: [0, 1], ArrowDown: [0, -1] }

const openingOf = ({ islands, tickets, global }, { path, ticket }) => {
  if (ticket) {
    const parcel = tickets.find(one => one.identifier === ticket)
    if (!parcel) throw new Error(`Scene 里没有票 ${ticket}`)
    return { focus: parcel.island, stop: 3, selected: parcel.key }
  }
  if (path) {
    const island = islands.find(one => one.path === path)
    if (!island) throw new Error(`Scene 里没有项目 ${path}`)
    return { focus: island.index, stop: 2, selected: null }
  }
  if (islands.length === 0) throw new Error('Scene 里没有项目')
  return { focus: (islands.find(one => !one.idle) ?? islands[0]).index, stop: global ? 1 : 2, selected: null }
}

const canvasHelp = global =>
  [
    '工坊沙盘。拖动平移，滚轮缩放，方向键平移，Home 复位，Esc 退一级。',
    global ? '数字键 1 看全部岛、2 回工坊、3 看这张票，[ 和 ] 切换项目，/ 搜索。' : '数字键 2 回工坊、3 看这张票，/ 搜索。',
    '票也可以从右侧列表选择。',
  ].join('')

const pointer = (canvas, lens, onClick) => {
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
    onClick({
      x: ((event.clientX - bounds.left) / bounds.width) * 2 - 1,
      y: -((event.clientY - bounds.top) / bounds.height) * 2 + 1,
    })
  })
  canvas.addEventListener('pointercancel', event => pointers.delete(event.pointerId))
  canvas.addEventListener('wheel', event => {
    event.preventDefault()
    if (event.deltaY === 0) return
    lens.zoom(event.deltaY > 0 ? WHEEL_STEP : 1 / WHEEL_STEP)
  }, { passive: false })
}

export const mountDiorama = (stage, { scene, path, ticket, now = () => Date.now() }) => {
  const archipelago = archipelagoOf(scene)
  const { islands, global } = archipelago
  let { focus, stop, selected } = openingOf(archipelago, { path, ticket })
  const motion = matchMedia('(prefers-reduced-motion: reduce)')
  let reduced = motion.matches
  motion.addEventListener('change', () => {
    reduced = motion.matches
  })

  const canvas = document.createElement('canvas')
  canvas.tabIndex = 0
  canvas.setAttribute('aria-label', canvasHelp(global))
  stage.append(canvas)

  const world = createWorld(canvas, archipelago)
  const lens = createCamera(archipelago)
  const parcelOf = key => archipelago.tickets.find(parcel => parcel.key === key)
  let margins = { top: 0, right: 0, bottom: 0, left: 0 }

  const refreshStatus = () => {
    const at = now()
    const sky = skyOf(scene, at - scene.generatedAt)
    world.setSky(sky)
    hud.status(stop === 1 ? null : islands[focus], scene, at, sky)
    overlay.hub(hubText(scene, at))
  }

  const showSelection = () => {
    hud.stops(stop, selected !== null, focus)
    hud.trip(parcelOf(selected), stop === 1 ? null : islands[focus])
  }

  const setStop = next => {
    const parcel = parcelOf(selected)
    stop = next === 3 && parcel ? 3 : next === 1 && global ? 1 : 2
    showSelection()
    overlay.focus(focus)
    refreshStatus()
    margins = hud.margins()
    lens.resize(stage.clientWidth, stage.clientHeight, margins)
    if (stop === 3) lens.frameSlot(world.parcels.get(parcel.key).group.position)
    else if (stop === 2) lens.frameIsland(focus)
    else lens.frameAll()
    if (reduced) lens.snap()
  }

  const select = key => {
    const parcel = parcelOf(key)
    selected = parcel ? key : null
    world.select(selected)
    if (parcel && (parcel.island !== focus || stop === 1)) {
      focus = parcel.island
      setStop(2)
    } else if (stop === 3) setStop(selected ? 3 : 2)
    else showSelection()
  }

  const enter = index => {
    focus = index
    selected = null
    world.select(null)
    setStop(2)
  }

  const back = () => {
    if (stop === 3) setStop(2)
    else if (selected) select(null)
    else if (stop === 2 && global) setStop(1)
  }

  const hud = createHud(stage, archipelago, { onSelect: select, onStop: setStop, onIsland: enter })
  const overlay = createOverlay(stage, archipelago, world, { onIsland: enter })

  pointer(canvas, lens, ndc => {
    const hit = world.pick(ndc, lens.camera)
    if (hit?.key) select(hit.key)
    else if (hit && (stop === 1 || hit.island !== focus)) enter(hit.island)
    else select(null)
  })
  canvas.addEventListener('dblclick', () => {
    if (selected) setStop(3)
  })

  const stopKeys = global ? ['1', '2', '3'] : ['2', '3']
  stage.addEventListener('keydown', event => {
    const { key, target } = event
    if (target.closest('input')) return
    if (key === 'Escape') back()
    else if (key === 'Home') setStop(stop)
    else if (target.closest('.list, .dock')) return
    else if (stopKeys.includes(key)) setStop(Number(key))
    else if ((key === '[' || key === ']') && global) enter((focus + (key === ']' ? 1 : islands.length - 1)) % islands.length)
    else if (key === '/') hud.openSearch()
    else if (NUDGES[key] && target === canvas) lens.nudge(...NUDGES[key])
    else return
    event.preventDefault()
  })

  world.select(selected)
  setInterval(refreshStatus, STATUS_INTERVAL)

  new ResizeObserver(() => {
    world.resize(stage.clientWidth, stage.clientHeight)
    setStop(stop)
    lens.snap()
  }).observe(stage)

  const frame = time => {
    lens.step(reduced)
    world.animate(time, reduced)
    overlay.place(lens.camera, { stop, selected, margins })
    world.render(lens.camera)
    requestAnimationFrame(frame)
  }
  requestAnimationFrame(frame)

  return {
    archipelago,
    state: () => ({
      stop,
      focus: islands[focus].path,
      selected: parcelOf(selected)?.identifier ?? null,
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
