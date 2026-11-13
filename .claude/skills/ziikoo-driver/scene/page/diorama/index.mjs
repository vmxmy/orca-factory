import { createCamera } from './camera.mjs'
import { createHud, hubText } from './hud.mjs'
import { archipelagoOf, skyOf, staleOf } from './model.mjs'
import { createOverlay } from './overlay.mjs'
import { createWorld } from './world.mjs'

const CLICK_TOLERANCE = 6
const WHEEL_STEP = 1.1
const STATUS_INTERVAL = 1000
const NUDGES = { ArrowLeft: [-1, 0], ArrowRight: [1, 0], ArrowUp: [0, 1], ArrowDown: [0, -1] }

const defaultFocus = islands => (islands.find(one => !one.idle) ?? islands[0]).index

const openingOf = ({ islands, tickets, global }, { path, ticket }) => {
  if (islands.length === 0) throw new Error('Scene 里没有项目')
  const named = tickets.filter(one => one.identifier === ticket)
  const parcel = named.find(one => one.path === path) ?? named[0]
  if (parcel) return { focus: parcel.island, stop: 3, selected: parcel.key }
  const island = islands.find(one => one.path === path)
  if (island) return { focus: island.index, stop: 2, selected: null }
  return { focus: defaultFocus(islands), stop: global ? 1 : 2, selected: null }
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

export const mountDiorama = (stage, { scene, path, ticket, now = () => Date.now(), onView = () => {} }) => {
  let current = scene
  let archipelago = archipelagoOf(current)
  let { focus, stop, selected } = openingOf(archipelago, { path, ticket })
  const lifetime = new AbortController()
  const motion = matchMedia('(prefers-reduced-motion: reduce)')
  let reduced = motion.matches
  motion.addEventListener('change', () => {
    reduced = motion.matches
  }, { signal: lifetime.signal })

  const canvas = document.createElement('canvas')
  canvas.tabIndex = 0
  canvas.setAttribute('aria-label', canvasHelp(archipelago.global))
  stage.append(canvas)

  let world
  try {
    world = createWorld(canvas)
  } catch (error) {
    canvas.remove()
    throw error
  }
  const lens = createCamera(archipelago)
  const parcelOf = key => archipelago.tickets.find(parcel => parcel.key === key)
  let margins = { top: 0, right: 0, bottom: 0, left: 0 }
  let size = { width: stage.clientWidth, height: stage.clientHeight }

  const refreshStatus = () => {
    const at = now()
    const age = at - current.generatedAt
    const sky = skyOf(current, age)
    world.setSky(sky)
    hud.status(stop === 1 ? null : archipelago.islands[focus], current, at, { sky, stale: staleOf(current, age) })
    overlay.hub(hubText(current, at))
  }

  const showSelection = () => {
    const parcel = parcelOf(selected)
    hud.stops(stop, parcel !== undefined, focus)
    hud.trip(parcel, stop === 1 ? null : archipelago.islands[focus])
    onView(stop === 1 ? { path: null, ticket: null } : { path: archipelago.islands[focus].path, ticket: parcel?.identifier ?? null })
  }

  const setStop = next => {
    const parcel = parcelOf(selected)
    stop = next === 3 && parcel ? 3 : next === 1 && archipelago.global ? 1 : 2
    showSelection()
    overlay.focus(focus)
    refreshStatus()
    margins = hud.margins()
    lens.resize(size.width, size.height, margins)
    if (stop === 3) lens.frameNeighborhood(world.neighborhood(parcel.key))
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
    else if (stop === 2 && archipelago.global) setStop(1)
  }

  const hud = createHud(stage, { onSelect: select, onStop: setStop, onIsland: enter })
  const overlay = createOverlay(stage, world, { onIsland: enter })

  const show = next => {
    archipelago = next
    world.build(archipelago)
    lens.retarget(archipelago)
    hud.update(archipelago)
    overlay.update(archipelago)
    canvas.setAttribute('aria-label', canvasHelp(archipelago.global))
  }

  pointer(canvas, lens, ndc => {
    const hit = world.pick(ndc, lens.camera)
    if (hit?.key) select(hit.key)
    else if (hit && (stop === 1 || hit.island !== focus)) enter(hit.island)
    else select(null)
  })
  canvas.addEventListener('dblclick', () => {
    if (selected) setStop(3)
  })

  stage.addEventListener('keydown', event => {
    const { key, target } = event
    const { islands, global } = archipelago
    if (target.closest('input')) return
    if (key === 'Escape') back()
    else if (key === 'Home') setStop(stop)
    else if (target.closest('.list, .dock')) return
    else if ((global ? ['1', '2', '3'] : ['2', '3']).includes(key)) setStop(Number(key))
    else if ((key === '[' || key === ']') && global) enter((focus + (key === ']' ? 1 : islands.length - 1)) % islands.length)
    else if (key === '/') hud.openSearch()
    else if (NUDGES[key] && target === canvas) lens.nudge(...NUDGES[key])
    else return
    event.preventDefault()
  }, { signal: lifetime.signal })

  show(archipelago)
  world.select(selected)
  const clock = setInterval(refreshStatus, STATUS_INTERVAL)

  const observer = new ResizeObserver(() => {
    size = { width: stage.clientWidth, height: stage.clientHeight }
    overlay.forgetSizes()
    world.resize(size.width, size.height)
    setStop(stop)
    lens.snap()
  })
  observer.observe(stage)

  let request = 0
  const frame = time => {
    lens.step(reduced)
    world.animate(time, reduced)
    overlay.place(lens.camera, { stop, selected, margins, size })
    world.render(lens.camera)
    request = requestAnimationFrame(frame)
  }
  request = requestAnimationFrame(frame)

  const update = next => {
    if (next.rows.length === 0) throw new Error('Scene 里没有项目，沙盘不能更新；请先 dispose，有项目后再重新挂载')
    const previous = parcelOf(selected)
    const held = { path: archipelago.islands[focus].path, ticket: previous?.identifier, station: previous?.station }
    current = next
    show(archipelagoOf(next))
    const island = archipelago.islands.find(one => one.path === held.path)
    const parcel = island && archipelago.tickets.find(one => one.island === island.index && one.identifier === held.ticket)
    selected = parcel?.key ?? null
    world.select(selected)
    if (!island) {
      focus = defaultFocus(archipelago.islands)
      setStop(1)
      return
    }
    focus = island.index
    if ((stop === 3 && !parcel) || (stop === 1 && !archipelago.global)) {
      setStop(2)
      return
    }
    overlay.focus(focus)
    showSelection()
    refreshStatus()
    if (stop === 3 && parcel.station !== held.station) {
      margins = hud.margins()
      lens.resize(size.width, size.height, margins)
      lens.frameNeighborhood(world.neighborhood(parcel.key))
      lens.snap()
    }
  }

  return {
    get archipelago() {
      return archipelago
    },
    update,
    link(next) {
      hud.link(next)
      refreshStatus()
      margins = hud.margins()
      lens.resize(size.width, size.height, margins)
    },
    dispose() {
      cancelAnimationFrame(request)
      clearInterval(clock)
      observer.disconnect()
      lifetime.abort()
      world.dispose()
      canvas.remove()
      hud.remove()
      overlay.remove()
    },
    state: () => {
      const parcel = world.parcels.get(selected)
      return {
        stop,
        focus: archipelago.islands[focus].path,
        selected: parcelOf(selected)?.identifier ?? null,
        reduced,
        view: { ...lens.view },
        goal: { ...lens.goal },
        quaternion: lens.camera.quaternion.toArray(),
        at: parcel ? parcel.group.position.toArray() : null,
        target: parcel ? parcel.target.toArray() : null,
        marks: [...world.parcels.values()]
          .filter(one => one.mark.visible)
          .map(one => ({ y: one.mark.position.y, rotation: one.mark.rotation.y })),
      }
    },
  }
}
