import { Vector3 } from 'three'

import { element } from './hud.mjs'
import { stationNeighborhood } from './model.mjs'

const LABEL_MIN_WIDTH = 620
const LABEL_GAP = 2
const PLATE_Y = 3.4
const HUB_PLATE_Y = 7
const ARROW_INSET = 10
const EDGE = 4

const stationLabels = ({ layout, working, workers }) => [
  ['pickup', '主账本', layout.tower, 2.6, 0],
  ['work', `工作间 · ${working}/${workers}`, layout.booths.reduce((sum, booth) => sum + booth.x, 0) / layout.booths.length, 1.9, layout.booths[0].z],
  ['gate', '检验间 · gate', -1, 2.2, 0],
  ['review', '审查', 2.5, 2.7, 0],
  ['pack', '封箱 · 开 PR', 5.5, 0.6, 0],
  ['merge', '栈桥 · 合并队列', 9.3, 0.5, 0],
  ['dispatch', '船闸 · 部署屏障', 11.2, 1.6, 3],
  ['smoke', '灯塔 · smoke', 16.2, 4, 6.6],
  ['return', '回流坡道 · correction', -6.5, 1.9, -4.6],
  ['repair', '处置塔', -1, 5.3, -6.3],
  ['human', '等人批', 3.4, 4.5, -6.2],
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

const heldOf = container => {
  const active = document.activeElement
  if (!container.contains(active) || active.dataset.island === undefined) return undefined
  return { island: active.dataset.island, kind: active.classList.contains('arrow') ? 'arrow' : 'plate' }
}

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

const markerOf = () => element('span', { class: 'label tag-marker', hidden: '' })

const rectAt = (point, { width, height }) => ({ left: point.x - width / 2, right: point.x + width / 2, top: point.y - 6 - height, bottom: point.y - 6 })

const foldStationTags = entries => {
  const groups = []
  for (const entry of entries) {
    const matches = groups.filter(group => group.members.some(other =>
      entry.item.parcel.island === other.item.parcel.island && entry.item.parcel.station === other.item.parcel.station && overlaps(entry.rect, other.rect),
    ))
    if (matches.length === 0) groups.push({ ...entry, members: [entry] })
    else {
      matches[0].members.push(entry, ...matches.slice(1).flatMap(group => group.members))
      for (const group of matches.slice(1)) groups.splice(groups.indexOf(group), 1)
    }
  }
  return groups
}

const fitTag = (point, size, taken, area) => {
  const above = rectAt(point, size)
  const shift = (dx, dy) => ({ left: above.left + dx, right: above.right + dx, top: above.top + dy, bottom: above.bottom + dy })
  return [above, shift(size.width / 2 + 8, size.height / 2), shift(-size.width / 2 - 8, size.height / 2), shift(0, size.height + 12), shift(0, -size.height - LABEL_GAP)]
    .find(rect => rect.left >= area.left && rect.right <= area.right && rect.top >= area.top && rect.bottom <= area.bottom && !taken.some(other => overlaps(rect, other)))
}

const positionTag = (node, rect) => {
  node.style.left = `${(rect.left + rect.right) / 2}px`
  node.style.top = `${rect.bottom}px`
}

const reveal = (node, visible) => {
  if (node.hidden === visible) node.hidden = !visible
}

const sizeOf = item => {
  item.size ??= { width: item.node.offsetWidth, height: item.node.offsetHeight }
  return item.size
}

const HUMAN_BADGES = new Map([
  ['scope', { label: '等批准', path: 'M12 2 15 4 19 5 20 9 22 12 20 15 19 19 15 20 12 22 9 20 5 19 4 15 2 12 4 9 5 5 9 4Z M7 12 10 15 17 8' }],
  ['deploy', { label: '等部署', path: 'M3 15H21L18 21H6Z M12 3V15 M10 5 4 12H10Z M14 7 19 12H14Z' }],
  ['takeover', { label: '等接手', path: 'M14 3A6 6 0 0 0 7 10L3 14A3 3 0 0 0 7 18L11 14A6 6 0 0 0 19 7L15 11 11 7Z' }],
])

const humanBadge = kind => {
  const badge = HUMAN_BADGES.get(kind)
  if (!badge) return []
  const svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg')
  for (const [name, value] of Object.entries({ class: 'human-badge', viewBox: '0 0 24 24', role: 'img', 'aria-label': badge.label })) {
    svg.setAttribute(name, value)
  }
  const path = document.createElementNS(svg.namespaceURI, 'path')
  path.setAttribute('d', badge.path)
  svg.append(path)
  return [svg]
}

const ticketLabel = parcel => ({
  ...itemOf(
    element('span', {
      class: ['label', 'ticket', parcel.alert ?? ''].join(' ').trim(),
    }, [
      ...humanBadge(parcel.humanKind),
      [parcel.identifier, ...(parcel.since !== undefined && parcel.duration ? [parcel.duration] : []), ...(parcel.unknown ? ['未知'] : [])].join(' · '),
    ]),
    new Vector3(),
  ),
  parcel,
  marker: markerOf(),
  count: element('span', { class: 'fold-count' }),
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
  const legend = element('div', { class: 'tag-legend', 'aria-label': '标签图例', hidden: '' })
  const anchored = element('div', { class: 'plates' })
  stage.append(labels, anchored, legend)
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
  let refocus
  let viewport = { width: 0, height: 0 }

  const screenOf = (position, camera) => {
    projected.copy(position).project(camera)
    return { x: ((projected.x + 1) / 2) * viewport.width, y: ((1 - projected.y) / 2) * viewport.height }
  }

  const place = (item, camera, taken) => {
    const { width, height } = sizeOf(item)
    const point = screenOf(item.position, camera)
    const bottom = point.y - 6
    const half = width / 2
    const onStage = point.x >= 0 && point.x <= viewport.width
    const x = onStage ? clampTo(point.x, half + EDGE, viewport.width - half - EDGE) : point.x
    const rect = settle({ left: x - half, right: x + half, top: bottom - height, bottom }, taken)
    item.node.style.left = `${x}px`
    item.node.style.top = `${rect.bottom}px`
    taken.push(rect)
  }

  const shownOf = (stop, selected) => {
    const global = stop === 1
    const neighborhood = stop === 3 ? stationNeighborhood(tickets.find(ticket => ticket.parcel.key === selected).parcel.station) : null
    const marked = tickets
      .filter(ticket => ticket.parcel.alert !== null || ticket.parcel.key === selected)
      .sort((left, right) => Number(right.parcel.key === selected) - Number(left.parcel.key === selected))
    return {
      tickets: marked,
      plates: global ? [...plates, ...(hub ? [hub] : [])] : [],
      stations: !global && viewport.width > LABEL_MIN_WIDTH ? stations.filter(item => !neighborhood || neighborhood.includes(item.station)) : [],
    }
  }

  const placeTags = (shown, camera, area, taken) => {
    const entries = shown.tickets.map(item => {
      item.baseSize ??= sizeOf(item)
      const point = screenOf(item.position, camera)
      return { item, point, rect: rectAt(point, item.baseSize) }
    })
    const groups = foldStationTags(entries)
    for (const { item, members } of groups) {
      const count = members.length > 1 ? ` +${members.length - 1}` : ''
      if (item.count.textContent !== count) {
        item.count.textContent = count
        item.size = null
      }
      item.node.dataset.tickets = JSON.stringify(members.map(member => member.item.parcel.identifier))
      item.node.title = members.map(member => member.item.node.textContent.replace(/ \+\d+$/, '')).join('\n')
      for (const member of members.slice(1)) reveal(member.item.node, false)
    }
    const tags = [...groups, ...shown.stations.map(item => ({ item, point: screenOf(item.position, camera) }))]
    const overflow = []
    for (const tag of tags) {
      const rect = fitTag(tag.point, sizeOf(tag.item), taken, area)
      tag.rect = rect
      reveal(tag.item.node, Boolean(rect))
      if (rect) {
        positionTag(tag.item.node, rect)
        taken.push(rect)
      } else if (inside(tag.point, areaOf({ left: 0, top: 0, right: 0, bottom: 0 }, viewport))) overflow.push(tag)
    }
    reveal(legend, overflow.length > 0)
    if (overflow.length === 0) return
    const legendRect = { left: area.left, right: Math.min(area.left + 220, area.right), top: Math.max(area.top, area.bottom - 100), bottom: area.bottom }
    legend.style.left = `${legendRect.left}px`
    legend.style.top = `${legendRect.top}px`
    legend.style.width = `${legendRect.right - legendRect.left}px`
    legend.style.height = `${legendRect.bottom - legendRect.top}px`
    for (const tag of tags) {
      if (!tag.rect || !overlaps(tag.rect, legendRect)) continue
      reveal(tag.item.node, false)
      taken.splice(taken.indexOf(tag.rect), 1)
      overflow.push(tag)
    }
    taken.push(legendRect)
    const lines = []
    overflow.forEach(({ item, point, members }, index) => {
      const number = String(index + 1)
      item.marker.textContent = number
      item.marker.dataset.tickets = item.node.dataset.tickets ?? '[]'
      const size = { width: 24, height: 24 }
      let rect = fitTag(point, size, taken, area)
      for (let x = area.left + 12; !rect && x + 12 <= area.right; x += 26) {
        const candidate = settle(rectAt({ x, y: area.bottom + 6 }, size), taken)
        if (candidate.top >= area.top) rect = candidate
      }
      reveal(item.marker, Boolean(rect))
      if (rect) {
        positionTag(item.marker, rect)
        taken.push(rect)
      }
      lines.push(`${number} · ${members ? members.map(member => member.item.node.textContent.replace(/ \+\d+$/, '')).join('；') : item.node.textContent}`)
    })
    const text = lines.join('\n')
    if (legend.textContent !== text) legend.textContent = text
  }

  return {
    update(archipelago) {
      refocus ??= heldOf(anchored)
      islands = archipelago.islands
      plates = islands.map(island => itemOf(plateOf(island), anchorOf(island)))
      hub = archipelago.hub
        ? itemOf(element('span', { class: 'plate hub', hidden: '' }), new Vector3(archipelago.hub.x, HUB_PLATE_Y, archipelago.hub.z))
        : null
      arrows = islands.filter(island => island.plate.alert !== null).map(island => itemOf(arrowOf(island), anchorOf(island)))
      tickets = archipelago.tickets.map(ticketLabel)
      for (const ticket of tickets) {
        ticket.node.hidden = true
        ticket.node.append(ticket.count)
      }
      stations = []
      focused = null
      anchored.replaceChildren(...plates.map(plate => plate.node), ...(hub ? [hub.node] : []), ...arrows.map(arrow => arrow.node))
      labels.replaceChildren(...tickets.flatMap(ticket => [ticket.node, ticket.marker]))
    },
    focus(index) {
      if (index === focused) return
      focused = index
      for (const station of stations) {
        station.node.remove()
        station.marker.remove()
      }
      const island = islands[index]
      stations = stationLabels(island).map(([station, text, x, y, z]) =>
        ({ ...itemOf(element('span', { class: 'label', text, hidden: '' }), new Vector3(x + island.origin.x, y, z + island.origin.z)), station, marker: markerOf() }),
      )
      labels.append(...stations.flatMap(station => [station.node, station.marker]))
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
      for (const item of [...tickets, ...stations]) reveal(item.marker, false)
      const taken = []
      placeTags(shown, camera, area, taken)
      for (const plate of shown.plates) place(plate, camera, taken)
      for (const { arrow, point } of pointing) pointTo(arrow, point, area)
      if (refocus === undefined) return
      anchored.querySelector(`.${refocus.kind}[data-island="${CSS.escape(refocus.island)}"]:not([hidden])`)?.focus({ preventScroll: true })
      refocus = undefined
    },
    forgetSizes() {
      for (const item of [...tickets, ...plates, ...(hub ? [hub] : []), ...stations, ...arrows]) item.size = null
      for (const ticket of tickets) {
        ticket.baseSize = null
        ticket.count.textContent = ''
      }
    },
    remove() {
      labels.remove()
      anchored.remove()
      legend.remove()
    },
  }
}
