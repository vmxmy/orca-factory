import { createHash } from 'node:crypto'
import { isIP } from 'node:net'
import { networkInterfaces } from 'node:os'

import type { DriverConfig, Runtime, Scene } from '../types'
import { parseConfig } from '../hooks/config'
import { parseRuntime } from '../hooks/runtime'
import { sceneOf } from '../hooks/scene'

const fail = (reason: string): never => {
  console.error(reason)
  process.exit(1)
}
const failWith = (reason: unknown) => fail(reason instanceof Error ? reason.message : String(reason))
process.on('uncaughtException', failWith)
process.on('unhandledRejection', failWith)

const SOURCE_STREAM_LIMIT = 2
const SOURCE_REQUEST_LIMIT = 120
const SOURCE_WINDOW_MS = 60_000
const LOOPBACK_BINDS = new Set(['127.0.0.1', '::1', 'localhost'])

const access = process.env.ZIIKOO_SCENE_ACCESS ?? 'token'
if (access !== 'token' && access !== 'public') fail(`ZIIKOO_SCENE_ACCESS 只能是 token 或 public，收到 ${access}`)
const isPublic = access === 'public'
const token = isPublic ? '' : process.env.ZIIKOO_SCENE_TOKEN || fail('缺少 ZIIKOO_SCENE_TOKEN')

const maxClientsText = process.env.ZIIKOO_SCENE_MAX_CLIENTS ?? '50'
const maxClients = Number(maxClientsText)
if (!/^\d+$/.test(maxClientsText) || !Number.isSafeInteger(maxClients) || maxClients <= 0) {
  fail(`工坊沙盘事件流客户端上限必须是正整数，收到 ${maxClientsText}`)
}

const bind = process.env.ZIIKOO_SCENE_BIND ?? 'auto'
if (isPublic && !LOOPBACK_BINDS.has(bind)) fail(`公开模式只能监听回环地址（127.0.0.1、::1、localhost），收到 ${bind}`)
const portText = process.env.ZIIKOO_SCENE_PORT ?? '0'
const port = Number(portText)
if (!/^\d+$/.test(portText) || !Number.isInteger(port) || port < 0 || port > 65535) {
  fail(`工坊沙盘服务端口必须是 0–65535 的整数，收到 ${portText}`)
}
const interfaces = networkInterfaces()
let hostname = bind === 'auto' ? '0.0.0.0' : bind
if (bind !== 'auto' && bind !== 'localhost' && isIP(bind) === 0) {
  const addresses = interfaces[bind]
  if (!addresses) fail(`接口 ${bind} 不存在`)
  hostname = addresses.find(address => address.family === 'IPv4')?.address ?? fail(`接口 ${bind} 没有 IPv4 地址`)
}
const advertised = hostname === '0.0.0.0'
  ? interfaces.nebula1?.find(address => address.family === 'IPv4')?.address
    ?? Object.values(interfaces).flat().find(address => address?.family === 'IPv4' && !address.internal)?.address
    ?? '127.0.0.1'
  : hostname

const page = (await Bun.file(new URL('./page/index.html', import.meta.url)).text()).replaceAll('__TOKEN__', encodeURIComponent(token))
const importmap = /<script type="importmap">([\s\S]*?)<\/script>/.exec(page)?.[1] ?? fail('page/index.html 缺少 importmap')
const securityHeaders = {
  'X-Content-Type-Options': 'nosniff',
  'Content-Security-Policy': [
    "default-src 'self'",
    `script-src 'self' 'sha256-${createHash('sha256').update(importmap).digest('base64')}'`,
    "img-src 'self' data:",
    "connect-src 'self'",
    "object-src 'none'",
    "base-uri 'none'",
    "form-action 'none'",
    "frame-ancestors 'none'",
  ].join('; '),
}

const sourceStreams = new Map<string, number>()
const sourceRequests = new Map<string, number>()
let windowEndsAt = Date.now() + SOURCE_WINDOW_MS

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
  '/live.mjs': { path: 'live.mjs', type: 'text/javascript; charset=utf-8' },
  ...Object.fromEntries(['index', 'model', 'world', 'camera', 'hud', 'overlay', 'view-state'].map(name => [`/diorama/${name}.mjs`, { path: `diorama/${name}.mjs`, type: 'text/javascript; charset=utf-8' }])),
  '/diorama/diorama.css': { path: 'diorama/diorama.css', type: 'text/css; charset=utf-8' },
  '/vendor/three.module.min.js': { path: 'vendor/three.module.min.js', type: 'text/javascript; charset=utf-8' },
  '/vendor/three.core.min.js': { path: 'vendor/three.core.min.js', type: 'text/javascript; charset=utf-8' },
  '/vendor/LICENSE.three': { path: 'vendor/LICENSE.three', type: 'text/plain; charset=utf-8' },
}

const listen = (listenPort: number) => {
  return Bun.serve({
    hostname,
    port: listenPort,
    idleTimeout: 0,
    async fetch(request) {
      const response = await respond(request)
      for (const [name, value] of Object.entries(securityHeaders)) response.headers.set(name, value)
      return response
    },
  })
}

const respond = async (request: Request): Promise<Response> => {
  const url = new URL(request.url)
  const source = request.headers.get('CF-Connecting-IP') ?? 'local'
  if (isPublic) {
    const now = Date.now()
    if (now >= windowEndsAt) {
      sourceRequests.clear()
      windowEndsAt = now + SOURCE_WINDOW_MS
    }
    const count = (sourceRequests.get(source) ?? 0) + 1
    sourceRequests.set(source, count)
    if (count > SOURCE_REQUEST_LIMIT) {
      return new Response(null, { status: 429, headers: { 'Retry-After': String(Math.max(1, Math.ceil((windowEndsAt - now) / 1000))) } })
    }
  } else if (url.searchParams.get('t') !== token && request.headers.get('X-Token') !== token) {
    return new Response(null, { status: 404 })
  }
  if (request.method !== 'GET') return new Response(null, { status: 404 })
  const headers = { 'Cache-Control': 'no-store', 'Referrer-Policy': 'no-referrer' }
  if (url.pathname === '/snapshot') {
    return Response.json(current ?? { reason: '等待 driver 快照' }, { status: current ? 200 : 503, headers })
  }
  if (url.pathname === '/events') {
    if (clients.size >= maxClients) {
      return new Response(null, { status: 503, headers: { ...headers, 'Retry-After': '5' } })
    }
    if (isPublic && (sourceStreams.get(source) ?? 0) >= SOURCE_STREAM_LIMIT) {
      return new Response(null, { status: 429, headers: { ...headers, 'Retry-After': '5' } })
    }
    let client: ReadableStreamDefaultController<Uint8Array>
    let keepalive: ReturnType<typeof setInterval>
    const detach = () => {
      clearInterval(keepalive)
      if (!clients.delete(client)) return
      if (clients.size === 0) unattendedSince = Date.now()
      if (!isPublic) return
      const remaining = (sourceStreams.get(source) ?? 1) - 1
      if (remaining > 0) sourceStreams.set(source, remaining)
      else sourceStreams.delete(source)
    }
    const stream = new ReadableStream<Uint8Array>({
      start(controller) {
        client = controller
        clients.add(client)
        if (isPublic) sourceStreams.set(source, (sourceStreams.get(source) ?? 0) + 1)
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
    return new Response(page, { headers: { ...headers, 'Content-Type': 'text/html; charset=utf-8' } })
  }
  const file = files[url.pathname]
  if (!file) return new Response(null, { status: 404 })
  const cacheControl = url.pathname.startsWith('/vendor/') ? 'public, max-age=86400' : 'public, max-age=300'
  return new Response(Bun.file(new URL(`./page/${file.path}`, import.meta.url)), { headers: { 'Cache-Control': cacheControl, 'Referrer-Policy': 'no-referrer', 'Content-Type': file.type } })
}

let server: Bun.Server<undefined>
try {
  try {
    server = listen(port === 0 ? 43210 : port)
  } catch (error) {
    if (port !== 0 || !error || typeof error !== 'object' || !('code' in error) || error.code !== 'EADDRINUSE') throw error
    server = listen(0)
  }
} catch (error) {
  const code = error && typeof error === 'object' && 'code' in error ? error.code : undefined
  fail(code === 'EADDRINUSE' ? `端口 ${port} 被占用` : `工坊沙盘服务无法监听 ${hostname}:${port}：${String(error).split(/\r?\n/)[0]}`)
}

const urlHost = isIP(advertised) === 6 ? `[${advertised}]` : advertised
console.log(`listening http://${urlHost}:${server.port}/${isPublic ? '' : `?t=${encodeURIComponent(token)}`}`)

const repeat = async (kind: 'status' | 'config', interval: number) => {
  await poll(kind)
  setTimeout(() => void repeat(kind, interval), interval)
}
void repeat('status', 5_000)
void repeat('config', 60_000)
setInterval(() => {
  if (clients.size === 0 && Date.now() - unattendedSince >= 15 * 60_000) process.exit(0)
}, 10_000)
