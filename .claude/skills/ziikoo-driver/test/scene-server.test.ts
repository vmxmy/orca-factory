import { expect, test } from 'claude-code/testing'

const plugin = new URL('..', import.meta.url).pathname
const serverPath = new URL('../scene/server.ts', import.meta.url).pathname
const token = 'scene-server-test-token'
let child: ReturnType<typeof Bun.spawn> | undefined

const launch = async () => {
  child = Bun.spawn(['bun', serverPath], {
    cwd: plugin,
    env: {
      ...process.env,
      ZIIKOO_SCENE_TOKEN: token,
      ZIIKOO_SCENE_BIND: '127.0.0.1',
      ZIIKOO_SCENE_PORT: '0',
      ZIIKOO_SCENE_MAX_CLIENTS: '2',
    },
    stdout: 'pipe',
    stderr: 'pipe',
  })
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
          const match = /listening (http:\/\/[^/]+)/.exec(output)
          if (match) {
            clearTimeout(timeout)
            resolve(match[1]!)
            return
          }
        }
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
