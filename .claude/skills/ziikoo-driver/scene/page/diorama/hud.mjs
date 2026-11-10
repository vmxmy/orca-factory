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

const SKY_NOTES = { overcast: '维护中', dark: '没有快照' }
const SEARCH_LIMIT = 8
const UNKNOWN_DEPLOY_NOTE = '部署：未知 · driver 未提供 deployment'

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
  const lines = notes.length === 0 ? [] : [notes.join(' · ')]
  if (parcel.unknownDeploy) lines.push(UNKNOWN_DEPLOY_NOTE)
  return lines.map(text => element('div', { class: 'trip-foot', text }))
}

const detailText = parcel =>
  [
    `${parcel.identifier} · ${parcel.title}`,
    `项目：${parcel.project} · ${parcel.path}`,
    `工位：${parcel.stationName}`,
    `真实阶段：${parcel.phase} / ${parcel.label}`,
    `已观察：${parcel.duration}`,
    ...(parcel.reason ? [`原因：${parcel.reason}`] : []),
    ...(parcel.unknownDeploy ? [UNKNOWN_DEPLOY_NOTE] : []),
  ].join('\n')

const tripActions = parcel =>
  element('div', { class: 'trip-actions' }, [
    element('button', { type: 'button', 'data-copy': parcel.identifier, text: '复制票号' }),
    element('button', { type: 'button', 'data-copy': detailText(parcel), text: '复制详情' }),
    element('span', { class: 'copied', role: 'status' }),
  ])

const copyText = async text => {
  if (navigator.clipboard && window.isSecureContext) {
    await navigator.clipboard.writeText(text)
    return
  }
  const area = element('textarea', { readonly: '', 'aria-hidden': 'true' })
  area.value = text
  area.style.cssText = 'position:fixed;left:-9999px;top:0'
  document.body.append(area)
  area.select()
  try {
    if (!document.execCommand('copy')) throw new Error('浏览器拒绝复制')
  } finally {
    area.remove()
  }
}

const tripHint = island =>
  island
    ? `${island.name}：点一只箱子，或从右侧列表选一张票，看它的行程。`
    : '全局：点一座岛进入项目，或从「卡住的票」直接跳到那张票。'

export const failureText = ({ command, exitCode, reason }) => `${command} 失败${exitCode == null ? '' : `（退出码 ${exitCode}）`}：${reason}`

const keepFocus = (container, render) => {
  const active = container.contains(document.activeElement) ? document.activeElement : null
  const held = active && ['ticket', 'island', 'copy'].find(name => active.dataset[name] !== undefined)
  const value = held && active.dataset[held]
  render()
  if (held) container.querySelector(`[data-${held}="${CSS.escape(value)}"]`)?.focus()
}

const searchResults = ({ islands, tickets }, query) => {
  const needle = query.trim().toLowerCase()
  if (needle === '') return []
  return [
    ...islands.filter(island => island.name.toLowerCase().includes(needle)).map(islandButton),
    ...tickets.filter(ticket => ticket.identifier.toLowerCase().includes(needle)).map(listButton),
  ].slice(0, SEARCH_LIMIT)
}

const stopButtons = () => [
  ['1', '1 全局'],
  ['2', '2 工坊'],
  ['3', '3 这张票'],
].map(([stop, text]) => element('button', { type: 'button', 'data-stop': stop, 'aria-pressed': 'false', text }))

const createSearch = (source, pick) => {
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
  const refresh = () => {
    const found = searchResults(source(), input.value)
    results.replaceChildren(...(found.length > 0 || input.value.trim() === '' ? found : [element('p', { text: '没有匹配的票或项目' })]))
  }
  input.addEventListener('input', refresh)
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
  return { form, toggle, open, refresh: () => keepFocus(results, refresh) }
}

export const createHud = (stage, { onSelect, onStop, onIsland }) => {
  let archipelago = { islands: [], tickets: [], stuck: [], global: false }
  let connection = null
  const pick = button => {
    if (button.dataset.ticket) onSelect(button.dataset.ticket)
    else if (button.dataset.island) onIsland(Number(button.dataset.island))
  }
  const status = element('div', { class: 'status', role: 'status' })
  const alarm = element('ul', { class: 'alarm', role: 'alert', hidden: '' })
  const dock = element('nav', { class: 'dock', 'aria-label': '项目坞', hidden: '' })
  const stops = element('div', { class: 'stops', role: 'group', 'aria-label': '机位' }, stopButtons())
  const search = createSearch(() => archipelago, pick)
  const heading = element('span')
  const count = element('span')
  const stuckList = element('div', { class: 'list' })
  const summary = element('summary')
  const allList = element('div', { class: 'list' })
  const panel = element('section', { class: 'stuck', 'aria-label': '卡住的票' }, [
    element('h2', {}, [heading, count]),
    stuckList,
    element('details', {}, [summary, allList]),
  ])
  const lead = element('div', { class: 'lead' }, [status, alarm, dock])
  const side = element('div', { class: 'side' }, [element('div', { class: 'tools' }, [stops, search.toggle]), search.form, panel])
  const trip = element('div', { class: 'trip', 'aria-live': 'polite' })
  const hud = element('div', { class: 'hud' }, [element('div', { class: 'hud-top' }, [lead, side]), trip])
  stage.append(hud)

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
  trip.addEventListener('click', async event => {
    const button = event.target.closest('button[data-copy]')
    if (!button) return
    const note = trip.querySelector('.copied')
    try {
      await copyText(button.dataset.copy)
      note.textContent = '已复制到剪贴板'
    } catch {
      note.textContent = '浏览器限制了复制，请手动选择行程栏中的文字。'
    }
  })

  return {
    openSearch: search.open,
    update(next) {
      archipelago = next
      const { stuck, tickets, islands } = next
      panel.classList.toggle('clear', stuck.length === 0)
      heading.textContent = stuck.length === 0 ? '没有卡住的票' : '卡住的票'
      count.textContent = stuck.length === 0 ? '' : `${stuck.length} 张`
      summary.textContent = `全部票 · ${tickets.length} 张`
      keepFocus(dock, () => dock.replaceChildren(...islands.map(islandButton)))
      keepFocus(stuckList, () => stuckList.replaceChildren(...stuck.map(stuckButton)))
      keepFocus(allList, () => allList.replaceChildren(...tickets.map(listButton)))
      search.refresh()
    },
    link(next) {
      connection = next
      const lines = [
        ...(next.online ? [] : [next.closed ? '连接已关闭：请从 /ziikoo-driver 面板重新打开。' : '连接断开：画面停在最后一份快照，正在自动重连。']),
        ...next.failures.map(failureText),
      ]
      alarm.replaceChildren(...lines.map(text => element('li', { text })))
      alarm.hidden = lines.length === 0
    },
    margins() {
      const frame = stage.getBoundingClientRect()
      const sideBox = side.getBoundingClientRect()
      const docked = sideBox.left > frame.left + frame.width / 2
      const topEdge = docked ? (alarm.hidden ? status : alarm).getBoundingClientRect().bottom : sideBox.bottom
      return {
        top: topEdge - frame.top + 12,
        right: docked ? frame.right - sideBox.left + 12 : 16,
        bottom: frame.bottom - trip.getBoundingClientRect().top + 12,
        left: docked && !dock.hidden ? dock.getBoundingClientRect().right - frame.left + 12 : 16,
      }
    },
    status(island, scene, now, { sky, stale }) {
      const parts = [
        ...(connection ? [connection.online ? '已连接' : '连接断开'] : []),
        island ? island.name : `${archipelago.islands.length} 个项目`,
        ...(SKY_NOTES[sky] ? [SKY_NOTES[sky]] : []),
        ...(stale ? ['快照过期'] : []),
        `driver ${scene.state}`,
        `快照 ${sinceText(now - scene.generatedAt)}前`,
        `维护 ${scene.maintenance}`,
        ...scanText(scene.nextScanAt, now),
      ]
      status.textContent = parts.join(' · ')
      status.title = island?.path ?? ''
      status.dataset.sky = sky
      if (connection) status.dataset.online = String(connection.online)
    },
    stops(current, selectable, focus) {
      for (const button of stops.querySelectorAll('button')) {
        button.setAttribute('aria-pressed', String(Number(button.dataset.stop) === current))
      }
      stops.querySelector('[data-stop="1"]').hidden = !archipelago.global
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
      keepFocus(trip, () => trip.replaceChildren(tripHead(parcel), tripTrack(parcel), ...tripFoot(parcel), tripActions(parcel)))
    },
    remove() {
      hud.remove()
    },
  }
}
