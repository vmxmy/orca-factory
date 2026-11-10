import { Vector3 } from 'three'

import { element } from './hud.mjs'

const LABEL_MIN_WIDTH = 620
const LABEL_GAP = 2
const PLATE_Y = 3.4
const HUB_PLATE_Y = 7
const ARROW_INSET = 10
const EDGE = 4

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

const plateOf = island =>
  element('button', {
    type: 'button',
    class: ['plate', island.idle ? 'idle' : '', island.plate.alert ?? ''].join(' ').trim(),
    'data-island': String(island.index),
    title: island.path,
    hidden: '',
  }, [
    element('b', { text: island.name }),
    element('span', { text: island.idle ? '空闲' : `在途 ${island.plate.inFlight}` }),
    ...(island.plate.barrier ? [element('span', { text: '闸关' })] : []),
    ...(island.plate.stuck > 0 ? [element('em', { text: `${island.plate.stuck} 张卡住` })] : []),
  ])

const arrowOf = island =>
  element('button', { type: 'button', class: `arrow ${island.plate.alert}`, 'data-island': String(island.index), hidden: '' }, [
    element('i'),
    element('span', { text: `${island.name} · ${island.plate.stuck} 张卡住` }),
  ])

const overlaps = (a, b) => a.left < b.right && b.left < a.right && a.top < b.bottom && b.top < a.bottom

const settle = (rect, taken) => {
  const blocker = taken.find(other => overlaps(rect, other))
  if (!blocker) return rect
  const lift = rect.bottom - blocker.top + LABEL_GAP
  return settle({ ...rect, top: rect.top - lift, bottom: rect.bottom - lift }, taken)
}

const clampTo = (value, low, high) => Math.min(high, Math.max(low, value))

export const createOverlay = (stage, archipelago, world, { onIsland }) => {
  const labels = element('div', { class: 'labels' })
  const anchored = element('div', { class: 'plates' })
  stage.append(labels, anchored)
  const anchorOf = ({ origin, layout }) => new Vector3(origin.x + layout.tower, PLATE_Y, origin.z)
  const plates = archipelago.islands.map(island => ({ node: plateOf(island), position: anchorOf(island) }))
  const hub = archipelago.hub
    ? { node: element('span', { class: 'plate hub', hidden: '' }), position: new Vector3(archipelago.hub.x, HUB_PLATE_Y, archipelago.hub.z) }
    : null
  const arrows = archipelago.islands
    .filter(island => island.plate.alert !== null)
    .map(island => ({ node: arrowOf(island), position: anchorOf(island) }))
  anchored.append(...plates.map(plate => plate.node), ...(hub ? [hub.node] : []), ...arrows.map(arrow => arrow.node))
  anchored.addEventListener('click', event => {
    const button = event.target.closest('button[data-island]')
    if (button) onIsland(Number(button.dataset.island))
  })

  const tickets = archipelago.tickets.map(parcel => ({
    parcel,
    position: new Vector3(0, parcel.alert ? 2 : 1.1, 0).add(world.parcels.get(parcel.key).group.position),
    node: element('span', {
      class: ['label', 'ticket', parcel.alert ?? ''].join(' ').trim(),
      text: parcel.unknown ? `${parcel.identifier} · 未知` : parcel.identifier,
    }),
  }))
  labels.append(...tickets.map(ticket => ticket.node))
  let stations = []
  let focused = null

  const screenOf = (position, camera) => {
    const point = position.clone().project(camera)
    return { x: ((point.x + 1) / 2) * stage.clientWidth, y: ((1 - point.y) / 2) * stage.clientHeight }
  }

  const place = (node, position, camera, taken, inside = false) => {
    node.hidden = false
    const point = screenOf(position, camera)
    const bottom = point.y - 6
    const half = node.offsetWidth / 2
    const onStage = point.x >= 0 && point.x <= stage.clientWidth
    const x = inside && onStage ? clampTo(point.x, half + EDGE, stage.clientWidth - half - EDGE) : point.x
    const rect = settle({ left: x - half, right: x + half, top: bottom - node.offsetHeight, bottom }, taken)
    node.style.left = `${x}px`
    node.style.top = `${rect.bottom}px`
    taken.push(rect)
  }

  const pointTo = (arrow, camera, margins) => {
    const area = { left: margins.left, top: margins.top, right: stage.clientWidth - margins.right, bottom: stage.clientHeight - margins.bottom }
    const { x, y } = screenOf(arrow.position, camera)
    arrow.node.hidden = x >= area.left && x <= area.right && y >= area.top && y <= area.bottom
    if (arrow.node.hidden) return
    const center = { x: (area.left + area.right) / 2, y: (area.top + area.bottom) / 2 }
    const dx = x - center.x
    const dy = y - center.y
    const reach = Math.min((area.right - center.x) / Math.abs(dx), (area.bottom - center.y) / Math.abs(dy))
    const halfWidth = arrow.node.offsetWidth / 2 + ARROW_INSET
    const halfHeight = arrow.node.offsetHeight / 2 + ARROW_INSET
    arrow.node.style.left = `${clampTo(center.x + dx * reach, area.left + halfWidth, area.right - halfWidth)}px`
    arrow.node.style.top = `${clampTo(center.y + dy * reach, area.top + halfHeight, area.bottom - halfHeight)}px`
    arrow.node.style.setProperty('--turn', `${Math.atan2(dy, dx)}rad`)
  }

  return {
    focus(index) {
      if (index === focused) return
      focused = index
      for (const station of stations) station.node.remove()
      const island = archipelago.islands[index]
      stations = stationLabels(island).map(([text, x, y, z]) => ({
        node: element('span', { class: 'label', text }),
        position: new Vector3(x + island.origin.x, y, z + island.origin.z),
      }))
      labels.append(...stations.map(station => station.node))
    },
    hub(text) {
      if (hub) hub.node.textContent = text
    },
    place(camera, { stop, selected, margins }) {
      const taken = []
      const global = stop === 1
      const shown = tickets
        .filter(ticket => ticket.parcel.alert !== null || ticket.parcel.key === selected)
        .sort((left, right) => Number(right.parcel.key === selected) - Number(left.parcel.key === selected))
      for (const ticket of tickets) ticket.node.hidden = !shown.includes(ticket)
      for (const ticket of shown) place(ticket.node, ticket.position, camera, taken)
      for (const plate of [...plates, ...(hub ? [hub] : [])]) {
        if (global) place(plate.node, plate.position, camera, taken, true)
        else plate.node.hidden = true
      }
      const wide = !global && stage.clientWidth > LABEL_MIN_WIDTH
      for (const station of stations) {
        station.node.hidden = !wide
        if (wide) place(station.node, station.position, camera, taken)
      }
      for (const arrow of arrows) {
        if (global) pointTo(arrow, camera, margins)
        else arrow.node.hidden = true
      }
    },
  }
}
