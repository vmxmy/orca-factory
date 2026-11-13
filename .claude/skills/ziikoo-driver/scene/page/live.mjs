import { failureText } from './diorama/hud.mjs'
import { mountDiorama } from './diorama/index.mjs'
import { archipelagoOf } from './diorama/model.mjs'

const token = new URL(import.meta.url).searchParams.get('t')
const resource = pathname => `${pathname}?t=${encodeURIComponent(token)}`
const stage = document.querySelector('.diorama')
const notice = document.createElement('p')
notice.className = 'notice'
stage.append(notice)
const failures = new Map()
let diorama = null
let broken = false
let empty = false
let connected = false
let closed = false
let events
let snapshotRequest
let previousTickets = null

const report = () => {
  const listed = [...failures.values()]
  if (diorama) diorama.link({ online: connected, closed, failures: listed })
  else if (!broken) notice.textContent = [empty ? '暂无项目' : '等待 driver 快照', connected ? '已连接' : '连接断开', ...listed.map(failureText)].join(' · ')
}

const fail = (command, reason) => {
  failures.set(command, { command, exitCode: null, reason })
  report()
}

const followView = ({ path, ticket }) => {
  const url = new URL(location.href)
  for (const [name, value] of [['project', path], ['ticket', ticket]]) {
    if (value) url.searchParams.set(name, value)
    else url.searchParams.delete(name)
  }
  history.replaceState(null, '', url)
}

const disconnect = () => {
  events?.close()
  events = undefined
  connected = false
  snapshotRequest?.abort()
  snapshotRequest = undefined
}

const mount = scene => {
  const query = new URLSearchParams(location.search)
  try {
    diorama = mountDiorama(stage, { scene, path: query.get('project'), ticket: query.get('ticket'), onView: followView })
  } catch (error) {
    broken = true
    disconnect()
    notice.className = 'notice failure'
    notice.textContent = error.message
    return
  }
  notice.remove()
  window.diorama = Object.freeze({ state: () => diorama.state() })
}

const unmount = () => {
  diorama?.dispose()
  diorama = null
  delete window.diorama
  empty = true
  stage.append(notice)
}

const receive = scene => {
  if (broken) return
  if (!Array.isArray(scene?.rows)) return fail('snapshot', '快照缺少 rows')
  failures.delete('snapshot')
  const tickets = new Map(archipelagoOf(scene).tickets.map(ticket => [`${ticket.path}\0${ticket.identifier}`, ticket]))
  const changes = []
  const add = (type, ticket, detail) => changes.push({ type, text: `${ticket.project} · ${ticket.identifier} · ${detail}`, path: ticket.path })
  if (previousTickets) {
    for (const [id, ticket] of tickets) {
      const previous = previousTickets.get(id)
      if (!previous) add('enter', ticket, `进入 · ${ticket.stationName}`)
      else if (ticket.station !== previous.station) add('move', ticket, `移动 · ${previous.stationName} → ${ticket.stationName}`)
      else if (ticket.phase !== previous.phase || ticket.owner !== previous.owner || ticket.label !== previous.label) {
        add('move', ticket, `移动 · ${ticket.stationName} · ${previous.label} → ${ticket.label} · ${ticket.owner}`)
      }
      if (ticket.alert && (ticket.alert !== previous?.alert || ticket.reason !== previous?.reason)) {
        add('alert', ticket, `告警 · ${ticket.stationName} · ${ticket.reason || ticket.label}`)
      }
    }
    for (const [id, ticket] of previousTickets) {
      if (!tickets.has(id)) add('leave', ticket, `离开 · ${ticket.stationName}`)
    }
  }
  previousTickets = tickets
  if (scene.rows.length === 0) unmount()
  else if (diorama) diorama.update(scene)
  else mount(scene)
  diorama?.notify(changes)
  report()
}

const fetchSnapshot = async () => {
  const controller = new AbortController()
  snapshotRequest?.abort()
  snapshotRequest = controller
  try {
    const response = await fetch(resource('/snapshot'), { signal: controller.signal })
    if (!response.ok) throw new Error(`快照 HTTP ${response.status}`)
    const scene = await response.json()
    if (controller === snapshotRequest) receive(scene)
  } catch (error) {
    if (error.name !== 'AbortError') fail('snapshot', error.message)
  }
}

const connect = () => {
  if (events || broken) return
  events = new EventSource(resource('/events'))
  events.onopen = () => {
    connected = true
    closed = false
    failures.clear()
    report()
  }
  events.addEventListener('scene', event => {
    snapshotRequest?.abort()
    receive(JSON.parse(event.data))
  })
  events.addEventListener('error', event => {
    if (event.data) {
      const failure = JSON.parse(event.data)
      if (failure.recovered) failures.delete(failure.command)
      else failures.set(failure.command, failure)
    } else {
      connected = false
      closed = event.target.readyState === EventSource.CLOSED
    }
    report()
  })
}

addEventListener('pagehide', () => {
  disconnect()
  report()
})
addEventListener('pageshow', event => {
  if (!event.persisted) return
  connect()
  void fetchSnapshot()
})
connect()
void fetchSnapshot()
