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

const anchorOf = ({ origin, layout }) => new Vector3(origin.x + layout.tower, PLATE_Y, origin.z)

const itemOf = (node, position) => ({ node, position, size: null })

const reveal = (node, visible) => {
  if (node.hidden === visible) node.hidden = !visible
}

const sizeOf = item => {
  item.size ??= { width: item.node.offsetWidth, height: item.node.offsetHeight }
  return item.size
}

const ticketLabel = parcel => ({
  ...itemOf(
    element('span', {
      class: ['label', 'ticket', parcel.alert ?? ''].join(' ').trim(),
      text: parcel.unknown ? `${parcel.identifier} · 未知` : parcel.identifier,
    }),
    new Vector3(),
  ),
  parcel,
  lift: new Vector3(0, parcel.alert ? 2 : 1.1, 0),
})

const areaOf = (margins, viewport) => ({
  left: margins.left,
  top: margins.top,
  right: viewport.width - margins.right,
  bottom: viewport.height - margins.bottom,
})

const inside = (point, area) => point.x >= area.left && point.x <= area.right && point.y >= area.top && point.y <= area.bottom

const pointTo = (arrow, point, area) => {
  const { width, height } = sizeOf(arrow)
  const center = { x: (area.left + area.right) / 2, y: (area.top + area.bottom) / 2 }
  const dx = point.x - center.x
  const dy = point.y - center.y
  const reach = Math.min((area.right - center.x) / Math.abs(dx), (area.bottom - center.y) / Math.abs(dy))
  const halfWidth = width / 2 + ARROW_INSET
  const halfHeight = height / 2 + ARROW_INSET
  arrow.node.style.left = `${clampTo(center.x + dx * reach, area.left + halfWidth, area.right - halfWidth)}px`
  arrow.node.style.top = `${clampTo(center.y + dy * reach, area.top + halfHeight, area.bottom - halfHeight)}px`
  arrow.node.style.setProperty('--turn', `${Math.atan2(dy, dx)}rad`)
}

export const createOverlay = (stage, world, { onIsland }) => {
  const labels = element('div', { class: 'labels' })
  const anchored = element('div', { class: 'plates' })
  stage.append(labels, anchored)
  anchored.addEventListener('click', event => {
    const button = event.target.closest('button[data-island]')
    if (button) onIsland(Number(button.dataset.island))
  })
  const projected = new Vector3()
  let islands = []
  let plates = []
  let hub = null
  let arrows = []
  let tickets = []
  let stations = []
  let focused = null
  let viewport = { width: 0, height: 0 }

  const screenOf = (position, camera) => {
    projected.copy(position).project(camera)
    return { x: ((projected.x + 1) / 2) * viewport.width, y: ((1 - projected.y) / 2) * viewport.height }
  }

  const place = (item, camera, taken, clamped = false) => {
    const { width, height } = sizeOf(item)
    const point = screenOf(item.position, camera)
    const bottom = point.y - 6
    const half = width / 2
    const onStage = point.x >= 0 && point.x <= viewport.width
    const x = clamped && onStage ? clampTo(point.x, half + EDGE, viewport.width - half - EDGE) : point.x
    const rect = settle({ left: x - half, right: x + half, top: bottom - height, bottom }, taken)
    item.node.style.left = `${x}px`
    item.node.style.top = `${rect.bottom}px`
    taken.push(rect)
  }

  const shownOf = (stop, selected) => {
    const global = stop === 1
    const marked = tickets
      .filter(ticket => ticket.parcel.alert !== null || ticket.parcel.key === selected)
      .sort((left, right) => Number(right.parcel.key === selected) - Number(left.parcel.key === selected))
    return {
      tickets: marked,
      plates: global ? [...plates, ...(hub ? [hub] : [])] : [],
      stations: !global && viewport.width > LABEL_MIN_WIDTH ? stations : [],
    }
  }

  return {
    update(archipelago) {
      islands = archipelago.islands
      plates = islands.map(island => itemOf(plateOf(island), anchorOf(island)))
      hub = archipelago.hub
        ? itemOf(element('span', { class: 'plate hub', hidden: '' }), new Vector3(archipelago.hub.x, HUB_PLATE_Y, archipelago.hub.z))
        : null
      arrows = islands.filter(island => island.plate.alert !== null).map(island => itemOf(arrowOf(island), anchorOf(island)))
      tickets = archipelago.tickets.map(ticketLabel)
      for (const ticket of tickets) ticket.node.hidden = true
      stations = []
      focused = null
      anchored.replaceChildren(...plates.map(plate => plate.node), ...(hub ? [hub.node] : []), ...arrows.map(arrow => arrow.node))
      labels.replaceChildren(...tickets.map(ticket => ticket.node))
    },
    focus(index) {
      if (index === focused) return
      focused = index
      for (const station of stations) station.node.remove()
      const island = islands[index]
      stations = stationLabels(island).map(([text, x, y, z]) =>
        itemOf(element('span', { class: 'label', text, hidden: '' }), new Vector3(x + island.origin.x, y, z + island.origin.z)),
      )
      labels.append(...stations.map(station => station.node))
    },
    hub(text) {
      if (!hub || hub.node.textContent === text) return
      hub.node.textContent = text
      hub.size = null
    },
    place(camera, { stop, selected, margins, size }) {
      viewport = size
      const shown = shownOf(stop, selected)
      const area = areaOf(margins, viewport)
      for (const ticket of shown.tickets) ticket.position.copy(world.parcels.get(ticket.parcel.key).group.position).add(ticket.lift)
      const pointing = stop === 1
        ? arrows.map(arrow => ({ arrow, point: screenOf(arrow.position, camera) })).filter(({ point }) => !inside(point, area))
        : []
      const visible = new Set([...shown.tickets, ...shown.plates, ...shown.stations, ...pointing.map(({ arrow }) => arrow)])
      for (const item of [...tickets, ...plates, ...(hub ? [hub] : []), ...stations, ...arrows]) reveal(item.node, visible.has(item))
      for (const item of visible) sizeOf(item)
      const taken = []
      for (const ticket of shown.tickets) place(ticket, camera, taken)
      for (const plate of shown.plates) place(plate, camera, taken, true)
      for (const station of shown.stations) place(station, camera, taken)
      for (const { arrow, point } of pointing) pointTo(arrow, point, area)
    },
    remove() {
      labels.remove()
      anchored.remove()
    },
  }
}
