import type { DriverConfig, Runtime, Scene } from '../types'
import { parseConfig } from '../hooks/config'
import { parseRuntime } from '../hooks/runtime'
import { sceneOf } from '../hooks/scene'

const token = process.env.ZIIKOO_SCENE_TOKEN
if (!token) throw new Error('缺少 ZIIKOO_SCENE_TOKEN')

const clients = new Set<ReadableStreamDefaultController<Uint8Array>>()
const encoder = new TextEncoder()
const failures = new Map<string, { command: string; exitCode: number | null; reason: string }>()
const firstSeen = new Map<string, number>()
let sequence = 0
let unattendedSince = Date.now()
let config: DriverConfig | undefined
let runtime: Runtime | undefined
let current: Scene | undefined
let previous = ''

const event = (name: string, value: unknown) => encoder.encode(`event: ${name}\ndata: ${JSON.stringify(value)}\n\n`)
const broadcast = (name: string, value: unknown) => {
  for (const client of clients) client.enqueue(event(name, value))
}

const publish = () => {
  if (!runtime || !config) return
  current = sceneOf(runtime, config)
  const active = new Set<string>()
  for (const row of current.rows) {
    for (const parcel of row.parcels) {
      const key = `${row.path}\0${parcel.identifier}`
      active.add(key)
      if (!firstSeen.has(key)) firstSeen.set(key, sequence++)
    }
    row.parcels.sort(
      (left, right) => firstSeen.get(`${row.path}\0${left.identifier}`)! - firstSeen.get(`${row.path}\0${right.identifier}`)!,
    )
  }
  for (const key of firstSeen.keys()) if (!active.has(key)) firstSeen.delete(key)
  const json = JSON.stringify(current)
  if (json === previous) return
  previous = json
  broadcast('scene', current)
}

const poll = async (kind: 'status' | 'config') => {
  let exitCode: number | null = null
  try {
    const child = Bun.spawn(['orca-factory-driver', kind, '--json'], { stdout: 'pipe', stderr: 'pipe' })
    const timeout = setTimeout(() => child.kill(), 25_000)
    let stdout: string
    let stderr: string
    try {
      ;[stdout, stderr, exitCode] = await Promise.all([
        new Response(child.stdout).text(),
        new Response(child.stderr).text(),
        child.exited,
      ])
    } finally {
      clearTimeout(timeout)
    }
    if (exitCode !== 0) throw new Error(stderr.split(/\r?\n/)[0] || '命令失败')
    if (kind === 'status') runtime = parseRuntime(stdout)
    else config = parseConfig(stdout)
    if (failures.delete(kind)) broadcast('error', { command: kind, recovered: true })
    publish()
  } catch (error) {
    const failure = { command: kind, exitCode, reason: error instanceof Error ? error.message.split(/\r?\n/)[0]! : String(error) }
    failures.set(kind, failure)
    broadcast('error', failure)
  }
}

const files: Record<string, { path: string; type: string }> = {
  '/scene.js': { path: 'scene.js', type: 'text/javascript; charset=utf-8' },
  '/vendor/three.module.min.js': { path: 'vendor/three.module.min.js', type: 'text/javascript; charset=utf-8' },
  '/vendor/three.core.min.js': { path: 'vendor/three.core.min.js', type: 'text/javascript; charset=utf-8' },
  '/vendor/OrbitControls.js': { path: 'vendor/OrbitControls.js', type: 'text/javascript; charset=utf-8' },
  '/vendor/LICENSE.three': { path: 'vendor/LICENSE.three', type: 'text/plain; charset=utf-8' },
}

const server = Bun.serve({
  hostname: '127.0.0.1',
  port: 0,
  idleTimeout: 0,
  async fetch(request) {
    const url = new URL(request.url)
    if (url.searchParams.get('t') !== token && request.headers.get('X-Token') !== token) return new Response(null, { status: 404 })
    if (request.method !== 'GET') return new Response(null, { status: 404 })
    const headers = { 'Cache-Control': 'no-store', 'Referrer-Policy': 'no-referrer' }
    if (url.pathname === '/snapshot') {
      return Response.json(current ?? { reason: '等待 driver 快照' }, { status: current ? 200 : 503, headers })
    }
    if (url.pathname === '/events') {
      let client: ReadableStreamDefaultController<Uint8Array>
      let keepalive: ReturnType<typeof setInterval>
      const detach = () => {
        clearInterval(keepalive)
        if (clients.delete(client) && clients.size === 0) unattendedSince = Date.now()
      }
      const stream = new ReadableStream<Uint8Array>({
        start(controller) {
          client = controller
          clients.add(client)
          if (current) client.enqueue(event('scene', current))
          for (const failure of failures.values()) client.enqueue(event('error', failure))
          keepalive = setInterval(() => client.enqueue(encoder.encode(': keepalive\n\n')), 15_000)
          request.signal.addEventListener('abort', detach, { once: true })
        },
        cancel: detach,
      })
      return new Response(stream, { headers: { ...headers, 'Content-Type': 'text/event-stream', Connection: 'keep-alive' } })
    }
    if (url.pathname === '/') {
      const html = await Bun.file(new URL('./page/index.html', import.meta.url)).text()
      return new Response(html.replaceAll('__TOKEN__', encodeURIComponent(token)), { headers: { ...headers, 'Content-Type': 'text/html; charset=utf-8' } })
    }
    const file = files[url.pathname]
    if (!file) return new Response(null, { status: 404 })
    return new Response(Bun.file(new URL(`./page/${file.path}`, import.meta.url)), { headers: { ...headers, 'Content-Type': file.type } })
  },
})

console.log(`listening http://127.0.0.1:${server.port}/?t=${encodeURIComponent(token)}`)

const repeat = async (kind: 'status' | 'config', interval: number) => {
  await poll(kind)
  setTimeout(() => void repeat(kind, interval), interval)
}
void repeat('status', 5_000)
void repeat('config', 60_000)
setInterval(() => {
  if (clients.size === 0 && Date.now() - unattendedSince >= 15 * 60_000) process.exit(0)
}, 10_000)
