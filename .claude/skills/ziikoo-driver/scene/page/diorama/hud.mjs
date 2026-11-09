import { TRACK, stuckOf } from './model.mjs'

const element = (tag, attributes = {}, children = []) => {
  const node = document.createElement(tag)
  for (const [name, value] of Object.entries(attributes)) {
    if (name === 'text') node.textContent = value
    else node.setAttribute(name, value)
  }
  node.append(...children)
  return node
}

const swatch = color => {
  const node = element('i')
  node.style.background = color
  return node
}

const SKY_NOTES = { fog: '快照过期', overcast: '维护中', dark: '没有快照' }

const sinceText = ms => {
  const seconds = Math.floor(Math.max(0, ms) / 1000)
  if (seconds < 60) return `${seconds} 秒`
  if (seconds < 3600) return `${Math.floor(seconds / 60)} 分钟`
  if (seconds < 86_400) return `${Math.floor(seconds / 3600)} 小时`
  return `${Math.floor(seconds / 86_400)} 天`
}

const scanText = (nextScanAt, now) => {
  if (nextScanAt === undefined) return []
  return [nextScanAt > now ? `下次巡检约 ${sinceText(nextScanAt - now)}后` : '即将巡检']
}

const ticketButton = (parcel, details) =>
  element('button', { type: 'button', 'data-ticket': parcel.identifier, 'aria-pressed': 'false' }, [
    swatch(parcel.color),
    element('b', { text: parcel.identifier }),
    ...details,
  ])

const stuckButton = parcel =>
  ticketButton(parcel, [
    element('em', { text: parcel.duration }),
    element('span', { text: [parcel.stationName, parcel.reason || parcel.label].join(' · ') }),
  ])

const listButton = parcel => ticketButton(parcel, [element('span', { text: parcel.stationName })])

const tripHead = parcel =>
  element('div', { class: 'trip-head' }, [
    element('b', { text: parcel.identifier }),
    element('span', { class: 'title', text: parcel.title }),
    element('span', { class: 'own' }, [swatch(parcel.color), `归属 ${parcel.owner}`]),
    element('span', { text: `${parcel.stationName} · ${parcel.label} · 已停留 ${parcel.duration}` }),
    ...(parcel.reason ? [element('span', { class: 'why', text: parcel.reason })] : []),
  ])

const tripTrack = parcel =>
  element('div', { class: 'track' }, TRACK.map((step, index) => {
    const state = parcel.track < 0 ? '' : index < parcel.track ? 'done' : index === parcel.track ? 'now' : ''
    return element('span', { class: state, text: step.name })
  }))

const tripFoot = parcel => {
  const notes = []
  if (parcel.unknown) notes.push(`工位未知 · 阶段 ${parcel.phase}`)
  else if (parcel.track < 0) notes.push(`不在主线：${parcel.stationName}`)
  if (parcel.queuePosition !== undefined) notes.push(`合并队列第 ${parcel.queuePosition} 位`)
  return notes.length === 0 ? [] : [element('div', { class: 'trip-foot', text: notes.join(' · ') })]
}

export const createHud = (stage, island, { onSelect, onStop }) => {
  const status = element('div', { class: 'status', role: 'status' })
  const stops = element('div', { class: 'stops', role: 'group', 'aria-label': '机位' }, [
    element('button', { type: 'button', 'data-stop': '2', 'aria-pressed': 'true', text: '2 工坊' }),
    element('button', { type: 'button', 'data-stop': '3', 'aria-pressed': 'false', text: '3 这张票' }),
  ])
  const stuck = stuckOf(island)
  const stuckList = element('div', { class: 'list' }, stuck.map(stuckButton))
  const allList = element('div', { class: 'list' }, island.parcels.map(listButton))
  const panel = element('section', { class: stuck.length === 0 ? 'stuck clear' : 'stuck', 'aria-label': '卡住的票' }, [
    element('h2', {}, [
      element('span', { text: stuck.length === 0 ? '没有卡住的票' : '卡住的票' }),
      element('span', { text: stuck.length === 0 ? '' : `${stuck.length} 张` }),
    ]),
    stuckList,
    element('details', {}, [element('summary', { text: `全部票 · ${island.parcels.length} 张` }), allList]),
  ])
  const side = element('div', { class: 'side' }, [stops, panel])
  const trip = element('div', { class: 'trip', 'aria-live': 'polite' })
  stage.append(element('div', { class: 'hud' }, [element('div', { class: 'hud-top' }, [status, side]), trip]))

  stops.addEventListener('click', event => {
    const button = event.target.closest('button[data-stop]')
    if (button) onStop(Number(button.dataset.stop))
  })
  panel.addEventListener('click', event => {
    const button = event.target.closest('button[data-ticket]')
    if (button) onSelect(button.dataset.ticket)
  })

  return {
    margins() {
      const frame = stage.getBoundingClientRect()
      const sideBox = side.getBoundingClientRect()
      const docked = sideBox.left > frame.left + frame.width / 2
      const topEdge = docked ? status.getBoundingClientRect().bottom : sideBox.bottom
      return {
        top: topEdge - frame.top + 12,
        right: docked ? frame.right - sideBox.left + 12 : 16,
        bottom: frame.bottom - trip.getBoundingClientRect().top + 12,
        left: 16,
      }
    },
    status(scene, now, sky) {
      const parts = [
        island.name,
        ...(SKY_NOTES[sky] ? [SKY_NOTES[sky]] : []),
        `driver ${scene.state}`,
        `快照 ${sinceText(now - scene.generatedAt)}前`,
        `维护 ${scene.maintenance}`,
        ...scanText(scene.nextScanAt, now),
      ]
      status.textContent = parts.join(' · ')
      status.title = island.path
      status.dataset.sky = sky
    },
    stops(current, selectable) {
      for (const button of stops.querySelectorAll('button')) {
        button.setAttribute('aria-pressed', String(Number(button.dataset.stop) === current))
      }
      stops.querySelector('[data-stop="3"]').disabled = !selectable
    },
    trip(parcel) {
      for (const button of panel.querySelectorAll('button[data-ticket]')) {
        button.setAttribute('aria-pressed', String(button.dataset.ticket === parcel?.identifier))
      }
      if (!parcel) {
        trip.replaceChildren(element('div', { class: 'trip-head' }, [
          element('span', { class: 'why', text: `${island.name}：点一只箱子，或从右侧列表选一张票，看它的行程。` }),
        ]))
        return
      }
      trip.replaceChildren(tripHead(parcel), tripTrack(parcel), ...tripFoot(parcel))
    },
  }
}
