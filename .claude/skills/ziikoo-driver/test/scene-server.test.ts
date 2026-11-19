import { expect, test } from 'claude-code/testing'

const plugin = new URL('..', import.meta.url).pathname
const serverPath = new URL('../scene/server.ts', import.meta.url).pathname
const token = 'scene-server-test-token'
let child: ReturnType<typeof Bun.spawn> | undefined
let listeningUrl = ''

const spawnServer = (env: Record<string, string | undefined> = {}) => {
  child = Bun.spawn(['bun', serverPath], {
    cwd: plugin,
    env: {
      ...process.env,
      ZIIKOO_SCENE_TOKEN: token,
      ZIIKOO_SCENE_BIND: '127.0.0.1',
      ZIIKOO_SCENE_PORT: '0',
      ZIIKOO_SCENE_MAX_CLIENTS: '2',
      ZIIKOO_SCENE_ACCESS: 'token',
      ...env,
    },
    stdout: 'pipe',
    stderr: 'pipe',
  })
  return child
}

const launch = async (env: Record<string, string | undefined> = {}) => {
  spawnServer(env)
  return await new Promise<string>((resolve, reject) => {
    let output = ''
    const timeout = setTimeout(() => reject(new Error(`scene server did not listen: ${output}`)), 10_000)
    const readOutput = async () => {
      const reader = child!.stdout.getReader()
      const decoder = new TextDecoder()
      try {
        for (;;) {
          const { done, value } = await reader.read()
          if (done) break
          output += decoder.decode(value, { stream: true })
          const match = /listening (http:\/\/\S+)/.exec(output)
          if (match) {
            clearTimeout(timeout)
            listeningUrl = match[1]!
            resolve(new URL(listeningUrl).origin)
            return
          }
        }
        clearTimeout(timeout)
        reject(new Error(`scene server exited before listening: ${await new Response(child!.stderr).text()}`))
      } catch (error) {
        clearTimeout(timeout)
        reject(error)
      }
    }
    void readOutput()
  })
}

const halt = async () => {
  if (!child) return
  child.kill('SIGTERM')
  await child.exited
  child = undefined
}

test('沙盘服务缓存静态文件并限制事件流连接数', async () => {
  if (typeof globalThis.fetch !== 'function' || !('Bun' in globalThis)) return
  const origin = await launch()
  try {
    const request = (path: string, init?: RequestInit) => fetch(`${origin}${path}?t=${token}`, init)

    for (const [path, maxAge] of [
      ['/live.mjs', '300'],
      ['/diorama/index.mjs', '300'],
      ['/diorama/diorama.css', '300'],
      ['/vendor/three.module.min.js', '86400'],
    ]) {
      const response = await request(path)
      expect(response.status).toBe(200)
      expect(response.headers.get('cache-control')).toBe(`public, max-age=${maxAge}`)
      await response.body?.cancel()
    }

    for (const path of ['/', '/snapshot']) {
      const response = await request(path)
      expect(response.headers.get('cache-control')).toBe('no-store')
      await response.body?.cancel()
    }

    const controllers = [new AbortController(), new AbortController()]
    const events = await Promise.all([
      request('/events', { signal: controllers[0]!.signal }),
      request('/events', { signal: controllers[1]!.signal }),
    ])
    expect(events.map(response => response.status)).toEqual([200, 200])
    const rejected = await request('/events')
    expect(rejected.status).toBe(503)
    expect(rejected.headers.get('retry-after')).toBe('5')
    await rejected.body?.cancel()

    controllers[0]!.abort()
    await new Promise(resolve => setTimeout(resolve, 100))
    const replacement = await request('/events')
    expect(replacement.status).toBe(200)
    await replacement.body?.cancel()
    controllers[1]!.abort()
  } finally {
    await halt()
  }
})

const expectSecurityHeaders = (response: Response) => {
  expect(response.headers.get('x-content-type-options')).toBe('nosniff')
  const csp = response.headers.get('content-security-policy') ?? ''
  expect(csp.includes("frame-ancestors 'none'")).toBe(true)
  expect(csp.includes("script-src 'self' 'sha256-")).toBe(true)
  expect(csp.split(';').find(directive => directive.trim().startsWith('script-src'))?.includes('unsafe-inline')).toBe(false)
}

test('沙盘服务公开模式免令牌并按来源限流', async () => {
  const publicEnv = { ZIIKOO_SCENE_ACCESS: 'public', ZIIKOO_SCENE_TOKEN: undefined, ZIIKOO_SCENE_MAX_CLIENTS: '3' }
  try {
    const origin = await launch(publicEnv)
    expect(listeningUrl.includes('?t=')).toBe(false)
    const request = async (path: string, source?: string, init?: RequestInit) => {
      const response = await fetch(`${origin}${path}`, {
        ...init,
        headers: source ? { 'CF-Connecting-IP': source } : {},
      })
      expectSecurityHeaders(response)
      return response
    }
    const home = await request('/')
    expect(home.status).toBe(200)
    expect((await home.text()).includes('__TOKEN__')).toBe(false)
    expect((await request('/missing')).status).toBe(404)
    expect((await request('/', undefined, { method: 'POST' })).status).toBe(404)

    const first = new AbortController()
    const streams = [
      await request('/events', '192.0.2.1', { signal: first.signal }),
      await request('/events', '192.0.2.1'),
    ]
    expect(streams.map(response => response.status)).toEqual([200, 200])
    const rejected = await request('/events', '192.0.2.1')
    expect(rejected.status).toBe(429)
    expect(Number(rejected.headers.get('retry-after')) > 0).toBe(true)
    streams.push(await request('/events', '192.0.2.2'))
    expect(streams[2]!.status).toBe(200)
    expect((await request('/events', '192.0.2.3')).status).toBe(503)
    first.abort()
    await new Promise(resolve => setTimeout(resolve, 100))
    const replacement = await request('/events', '192.0.2.1')
    expect(replacement.status).toBe(200)
    await replacement.body?.cancel()
    await streams[1]!.body?.cancel()
    await streams[2]!.body?.cancel()

    for (let index = 1; index <= 121; index++) {
      const response = await request('/live.mjs', '192.0.2.4')
      expect(response.status).toBe(index <= 120 ? 200 : 429)
      if (index === 121) expect(Number(response.headers.get('retry-after')) > 0).toBe(true)
      await response.body?.cancel()
    }
    const other = await request('/live.mjs', '192.0.2.5')
    expect(other.status).toBe(200)
    await other.body?.cancel()
    // The homepage and two rejected routes above already used three local requests.
    for (let index = 4; index <= 121; index++) {
      const response = await request('/live.mjs')
      expect(response.status).toBe(index <= 120 ? 200 : 429)
      await response.body?.cancel()
    }
  } finally {
    await halt()
  }

  for (const env of [
    { ...publicEnv, ZIIKOO_SCENE_BIND: '0.0.0.0' },
    { ...publicEnv, ZIIKOO_SCENE_ACCESS: 'invalid' },
    { ZIIKOO_SCENE_TOKEN: undefined },
  ]) {
    const server = spawnServer(env)
    const timeout = setTimeout(() => server.kill('SIGTERM'), 2000)
    try {
      const code = await server.exited
      expect(code > 0).toBe(true)
      expect(server.signalCode).toBe(null)
    } finally {
      clearTimeout(timeout)
      await halt()
    }
  }
})
