import { TRACK } from './model.mjs'

export const element = (tag, attributes = {}, children = []) => {
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
const SEARCH_LIMIT = 8

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

export const hubText = (scene, now) => ['调度岛', `driver ${scene.state}`, ...scanText(scene.nextScanAt, now)].join(' · ')

const ticketButton = (parcel, details) =>
  element('button', { type: 'button', 'data-ticket': parcel.key, 'aria-pressed': 'false' }, [
    swatch(parcel.color),
    element('b', { text: parcel.identifier }),
    ...details,
  ])

const lineOf = text => element('span', { text, title: text })

const stuckButton = parcel =>
  ticketButton(parcel, [
    element('em', { text: parcel.duration }),
    lineOf([parcel.project, parcel.stationName, parcel.reason || parcel.label].join(' · ')),
  ])

const listButton = parcel => ticketButton(parcel, [lineOf(`${parcel.project} · ${parcel.stationName}`)])

const islandButton = island =>
  element('button', { type: 'button', 'data-island': String(island.index), title: island.path, class: island.idle ? 'idle' : '' }, [
    element('span', { text: island.name }),
    ...(island.plate.stuck > 0 ? [element('em', { class: island.plate.alert, text: `${island.plate.stuck} 卡住` })] : []),
  ])

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

const tripHint = island =>
  island
    ? `${island.name}：点一只箱子，或从右侧列表选一张票，看它的行程。`
    : '全局：点一座岛进入项目，或从「卡住的票」直接跳到那张票。'

const searchResults = ({ islands, tickets }, query) => {
  const needle = query.trim().toLowerCase()
  if (needle === '') return []
  return [
    ...islands.filter(island => island.name.toLowerCase().includes(needle)).map(islandButton),
    ...tickets.filter(ticket => ticket.identifier.toLowerCase().includes(needle)).map(listButton),
  ].slice(0, SEARCH_LIMIT)
}

const stopButtons = global => [
  ...(global ? [['1', '1 全局']] : []),
  ['2', '2 工坊'],
  ['3', '3 这张票'],
].map(([stop, text]) => element('button', { type: 'button', 'data-stop': stop, 'aria-pressed': 'false', text }))

const createSearch = (archipelago, pick) => {
  const input = element('input', { type: 'search', placeholder: '票号或项目名', 'aria-label': '搜索票号或项目名', autocomplete: 'off', spellcheck: 'false' })
  const results = element('div', { class: 'list results' })
  const form = element('form', { class: 'search', role: 'search', hidden: '' }, [input, results])
  const toggle = element('button', { type: 'button', class: 'find', 'aria-expanded': 'false', text: '/ 搜索' })
  let returnTo = null

  const close = () => {
    form.hidden = true
    toggle.setAttribute('aria-expanded', 'false')
    returnTo?.focus()
  }
  const open = () => {
    returnTo = document.activeElement === document.body ? null : document.activeElement
    form.hidden = false
    toggle.setAttribute('aria-expanded', 'true')
    input.select()
    input.focus()
  }
  input.addEventListener('input', () => {
    const found = searchResults(archipelago, input.value)
    results.replaceChildren(...(found.length > 0 || input.value.trim() === '' ? found : [element('p', { text: '没有匹配的票或项目' })]))
  })
  form.addEventListener('submit', event => {
    event.preventDefault()
    results.querySelector('button')?.click()
  })
  form.addEventListener('keydown', event => {
    if (event.key !== 'Escape') return
    event.preventDefault()
    event.stopPropagation()
    close()
  })
  results.addEventListener('click', event => {
    const button = event.target.closest('button')
    if (!button) return
    close()
    pick(button)
  })
  toggle.addEventListener('click', () => (form.hidden ? open() : close()))
  return { form, toggle, open }
}

export const createHud = (stage, archipelago, { onSelect, onStop, onIsland }) => {
  const pick = button => {
    if (button.dataset.ticket) onSelect(button.dataset.ticket)
    else if (button.dataset.island) onIsland(Number(button.dataset.island))
  }
  const status = element('div', { class: 'status', role: 'status' })
  const dock = element('nav', { class: 'dock', 'aria-label': '项目坞', hidden: '' }, archipelago.islands.map(islandButton))
  const stops = element('div', { class: 'stops', role: 'group', 'aria-label': '机位' }, stopButtons(archipelago.global))
  const search = createSearch(archipelago, pick)
  const stuck = archipelago.stuck
  const panel = element('section', { class: stuck.length === 0 ? 'stuck clear' : 'stuck', 'aria-label': '卡住的票' }, [
    element('h2', {}, [
      element('span', { text: stuck.length === 0 ? '没有卡住的票' : '卡住的票' }),
      element('span', { text: stuck.length === 0 ? '' : `${stuck.length} 张` }),
    ]),
    element('div', { class: 'list' }, stuck.map(stuckButton)),
    element('details', {}, [
      element('summary', { text: `全部票 · ${archipelago.tickets.length} 张` }),
      element('div', { class: 'list' }, archipelago.tickets.map(listButton)),
    ]),
  ])
  const lead = element('div', { class: 'lead' }, [status, dock])
  const side = element('div', { class: 'side' }, [element('div', { class: 'tools' }, [stops, search.toggle]), search.form, panel])
  const trip = element('div', { class: 'trip', 'aria-live': 'polite' })
  stage.append(element('div', { class: 'hud' }, [element('div', { class: 'hud-top' }, [lead, side]), trip]))

  stops.addEventListener('click', event => {
    const button = event.target.closest('button[data-stop]')
    if (button) onStop(Number(button.dataset.stop))
  })
  for (const list of [panel, dock]) {
    list.addEventListener('click', event => {
      const button = event.target.closest('button')
      if (button) pick(button)
    })
  }

  return {
    openSearch: search.open,
    margins() {
      const frame = stage.getBoundingClientRect()
      const sideBox = side.getBoundingClientRect()
      const docked = sideBox.left > frame.left + frame.width / 2
      const topEdge = docked ? status.getBoundingClientRect().bottom : sideBox.bottom
      return {
        top: topEdge - frame.top + 12,
        right: docked ? frame.right - sideBox.left + 12 : 16,
        bottom: frame.bottom - trip.getBoundingClientRect().top + 12,
        left: docked && !dock.hidden ? dock.getBoundingClientRect().right - frame.left + 12 : 16,
      }
    },
    status(island, scene, now, sky) {
      const parts = [
        island ? island.name : `${archipelago.islands.length} 个项目`,
        ...(SKY_NOTES[sky] ? [SKY_NOTES[sky]] : []),
        `driver ${scene.state}`,
        `快照 ${sinceText(now - scene.generatedAt)}前`,
        `维护 ${scene.maintenance}`,
        ...scanText(scene.nextScanAt, now),
      ]
      status.textContent = parts.join(' · ')
      status.title = island?.path ?? ''
      status.dataset.sky = sky
    },
    stops(current, selectable, focus) {
      for (const button of stops.querySelectorAll('button')) {
        button.setAttribute('aria-pressed', String(Number(button.dataset.stop) === current))
      }
      stops.querySelector('[data-stop="3"]').disabled = !selectable
      dock.hidden = !archipelago.global || current === 1
      for (const button of dock.querySelectorAll('button')) {
        button.setAttribute('aria-current', String(Number(button.dataset.island) === focus))
      }
    },
    trip(parcel, island) {
      for (const button of panel.querySelectorAll('button[data-ticket]')) {
        button.setAttribute('aria-pressed', String(button.dataset.ticket === parcel?.key))
      }
      if (!parcel) {
        trip.replaceChildren(element('div', { class: 'trip-head' }, [element('span', { class: 'why', text: tripHint(island) })]))
        return
      }
      trip.replaceChildren(tripHead(parcel), tripTrack(parcel), ...tripFoot(parcel))
    },
  }
}
