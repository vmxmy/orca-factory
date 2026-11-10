const assert = require('node:assert/strict')
const fs = require('node:fs')
const http = require('node:http')
const os = require('node:os')
const path = require('node:path')
const crypto = require('node:crypto')
const { execFileSync, spawn } = require('node:child_process')
const { pathToFileURL } = require('node:url')

const plugin = path.resolve(__dirname, '..')
const preview = path.join(plugin, 'test/design-preview')
const pageRoot = path.join(plugin, 'scene/page')
const shots = process.env.DIORAMA_SCREENSHOTS
const servedRoots = [path.join(plugin, 'scene/page') + path.sep, preview + path.sep]
const types = { '.html': 'text/html', '.mjs': 'text/javascript', '.js': 'text/javascript', '.css': 'text/css', '.json': 'application/json' }
const STATIONS = ['pickup', 'work', 'return', 'gate', 'review', 'pack', 'merge', 'dispatch', 'smoke', 'repair', 'human']
const MARK_REST = 1.45
const FACTORY = '/home/laoxu/github/orca-factory'
const DASHBOARD = '/home/laoxu/github/zhiyun-ai-dashboard'
const ANSIBLE = '/home/laoxu/ansible'
const TWIN = '/home/laoxu/github/token123'
const readJSON = file => JSON.parse(fs.readFileSync(file, 'utf8'))

const brightness = hex => {
  const r = hex >> 16 & 0xff
  const g = hex >> 8 & 0xff
  const b = hex & 0xff
  return 0.2126 * r + 0.7152 * g + 0.0722 * b
}

const parcel = (identifier, station, extra = {}) => ({
  identifier,
  title: identifier,
  owner: '执行器',
  phase: 'worker 运行中',
  label: '施工',
  station,
  tone: 'current',
  duration: '1 分钟',
  unknownDeploy: false,
  ...extra,
})
const row = (parcels, workers = 3) => ({ name: 'fixture', path: '/fixture', deploy: true, workers, parcels })

async function checkModel() {
  const model = await import(pathToFileURL(path.join(plugin, 'scene/page/diorama/model.mjs')))
  const recorded = readJSON(path.join(preview, 'diorama-scene-next-scan.json'))
  assert.equal(recorded.state, 'ok')
  assert.equal(recorded.nextScanAt - recorded.generatedAt, 5_170, 'the recorded next scan interval must survive parseRuntime and sceneOf')
  assert.deepEqual(
    [41_000, 155_170, 155_171].map(age => model.staleOf(recorded, age)),
    [false, false, true],
    'the recorded ok snapshot must stay fresh through its next scan plus the 150-second scan grace and expire one millisecond later',
  )
  const generatedAt = 1e12
  const scheduled = { state: 'ok', generatedAt, nextScanAt: generatedAt + 120_000 }
  assert.deepEqual(
    [90_000, 170_000, 270_000, 270_001, 280_000].map(age => model.staleOf(scheduled, age)),
    [false, false, false, true, true],
    'an ok snapshot must stay fresh through the next scan plus the 150-second scan grace and expire only after it',
  )
  assert.deepEqual(
    [40_000, 40_001, 41_000].map(age => model.staleOf({ state: 'ok', generatedAt }, age)),
    [false, true, true],
    'an ok snapshot without a next scan must keep the 40-second expiry rule',
  )
  console.log('PASS diorama freshness model: next scan grace period, exact boundary, legacy expiry')
  const crowd = Array.from({ length: 7 }, (_, index) => parcel(`W-${index}`, 'work'))
  const island = model.islandOf(row([...STATIONS.map(station => parcel(`S-${station}`, station)), parcel('S-new', 'new-backend-stage'), ...crowd]))
  const find = identifier => island.parcels.find(one => one.identifier === identifier)
  for (const station of STATIONS) {
    assert.equal(find(`S-${station}`).station, station)
    assert.equal(find(`S-${station}`).stationName, model.STATION_NAMES[station])
  }
  assert.equal(island.layout.booths.length, 8, 'booth count must equal work tickets above three')
  assert.equal(model.islandOf(row([], 20)).layout.booths.length, 3, 'booth count must stay three without work tickets, whatever workers says')
  assert.ok(island.layout.booths.some(booth => booth.x === find('S-work').slot.x), 'work parcels must stand at a booth')
  for (const station of ['return', 'repair', 'human']) assert.ok(find(`S-${station}`).slot.y >= 1.5, `${station} must sit on the mezzanine`)
  assert.equal(new Set(island.parcels.map(({ slot }) => `${slot.x},${slot.y},${slot.z}`)).size, island.parcels.length, 'slots must not overlap')
  const unknown = find('S-new')
  assert.deepEqual([unknown.station, unknown.unknown, unknown.alert], ['unknown', true, 'warn'])
  assert.equal(model.ownerColor('不存在的归属'), model.OWNER_COLORS.无)
  assert.equal(island.lantern, true)

  const queue = model.islandOf(row([
    parcel('Q-none', 'merge'),
    parcel('Q-2', 'merge', { queuePosition: 2 }),
    parcel('Q-1', 'merge', { queuePosition: 1 }),
  ])).parcels
  assert.deepEqual(queue.map(one => one.identifier), ['Q-1', 'Q-2', 'Q-none'], 'merge queue must sort by position, missing last')
  assert.ok(queue[0].slot.x > queue[1].slot.x && queue[1].slot.x > queue[2].slot.x, 'queue head must stand nearest the lock')
  assert.equal(queue[2].queuePosition, undefined)
  const humans = model.islandOf(row([
    parcel('H-1', 'human', { since: 200 }),
    parcel('H-2', 'human', { since: 100 }),
  ])).parcels
  assert.equal(humans[0].identifier, 'H-2', 'the oldest human ticket must occupy the desk despite ticket-number order')
  assert.deepEqual(humans[0].slot, { x: 3, y: 1.7, z: -5.6 }, 'the oldest ticket must stand on the human desk')
  assert.equal(model.HUMAN_QUEUE_STEP, 1.3)
  assert.ok(Math.abs(Math.abs(humans[1].slot.x - humans[0].slot.x) - model.HUMAN_QUEUE_STEP) < 1e-9, 'the first waiting ticket must be one human queue step from the desk')
  const waiting = model.islandOf(row([
    parcel('H-20', 'human'), parcel('H-10', 'human', { since: 100 }),
    parcel('H-2', 'human', { since: 100 }), parcel('H-1', 'human', { since: 0 }),
    parcel('H-3', 'human'),
  ])).parcels
  assert.deepEqual(waiting.map(one => one.identifier), ['H-1', 'H-2', 'H-10', 'H-3', 'H-20'], 'human queue must sort by since, then ticket number, with missing times last')
  for (let index = 2; index <= 3; index++) {
    assert.ok(Math.abs(Math.abs(waiting[index].slot.x - waiting[index - 1].slot.x) - model.HUMAN_QUEUE_STEP) < 1e-9, 'waiting slots must be one human queue step apart')
  }
  assert.equal(waiting[4].slot.x, waiting[1].slot.x, 'human queue must wrap after three waiting tickets')
  assert.ok(waiting[4].slot.y > waiting[1].slot.y)
  for (const station of STATIONS.filter(station => !['human', 'merge'].includes(station))) {
    const unchanged = model.islandOf(row([parcel('Z-2', station, { since: 200 }), parcel('Z-1', station, { since: 100 })]))
    assert.deepEqual(unchanged.parcels.map(one => one.identifier), ['Z-2', 'Z-1'], `${station} must retain input order`)
  }
  console.log('PASS human queue model: oldest at desk, missing times last, numeric ties, spacing and three slots per row')
  const merged = parcel('M-1', 'merge', { phase: '已合并', label: '已合并', unknownDeploy: true })
  const blind = model.islandOf(row([merged]))
  assert.deepEqual(
    [blind.parcels[0].unknownDeploy, blind.parcels[0].alert, blind.parcels[0].stationName],
    [true, 'warn', `${model.STATION_NAMES.merge} · 部署未知`],
    'a merged ticket without deployment must warn and say so at its station',
  )
  assert.deepEqual(model.stuckOf(blind).map(one => one.identifier), ['M-1'], 'an unknown deployment must count as stuck')
  assert.deepEqual([blind.lock, blind.beacon], ['open', 'unknown'], 'an unknown deployment must grey the lighthouse and keep the lock rule')
  assert.equal(model.islandOf(row([merged, parcel('M-2', 'dispatch', { deployment: 'failed' })])).beacon, 'failed', 'a failed deployment must outrank an unknown one')
  const undeployed = model.islandOf({ ...row([merged]), deploy: false })
  assert.equal(undeployed.beacon, 'off', 'a project without deployment keeps its lighthouse off')
  assert.deepEqual([undeployed.parcels[0].alert, undeployed.parcels[0].unknownDeploy, undeployed.parcels[0].undeployed], [null, false, true], 'a merged ticket in a project without deployment must not warn')
  assert.deepEqual(model.stuckOf(undeployed), [], 'a merged ticket in a project without deployment is not stuck')
  assert.equal(undeployed.parcels[0].stationName, model.STATION_NAMES.merge, 'a project without deployment must not say 部署未知')
  const taped = model.islandOf(row([
    parcel('C-2', 'merge', { lane: 2, corrections: { count: 2, limit: 3 } }),
    parcel('C-4', 'repair', { corrections: { count: 4, limit: 3 } }),
    parcel('C-none', 'merge'),
  ])).parcels
  assert.deepEqual(
    Object.fromEntries(taped.map(one => [one.identifier, [one.tapes, one.tapesSpent, one.lane]])),
    { 'C-2': [2, false, 2], 'C-4': [3, true, undefined], 'C-none': [0, false, undefined] },
    'tapes must equal the correction count up to the limit, turn spent at the limit, and stay absent without corrections',
  )
  console.log('PASS diorama view model: station mapping, booths, queue order, missing position, unknown station, unknown deployment, correction tapes')

  const named = (name, parcels) => ({ ...row(parcels), name, path: `/${name}` })
  const sceneOf = rows => ({ generatedAt: 0, state: 'ok', maintenance: 'running', rows })
  const busy = model.archipelagoOf(sceneOf([
    named('a', [parcel('A-1', 'work'), parcel('A-2', 'repair', { tone: 'attention', since: 300 })]),
    named('b', []),
    named('c', [parcel('C-1', 'human', { tone: 'failed', since: 100 }), parcel('C-2', 'dispatch', { deployment: 'pending' })]),
    named('d', [parcel('D-1', 'pickup')]),
  ]))
  const calm = model.archipelagoOf(sceneOf(['a', 'b', 'c', 'd'].map(name => named(name, []))))
  assert.deepEqual(busy.islands.map(one => one.origin), calm.islands.map(one => one.origin), 'island positions must follow row order, not state')
  assert.equal(new Set(busy.islands.map(({ origin }) => `${origin.x},${origin.z}`)).size, 4, 'islands must not share a position')
  assert.deepEqual(busy.islands.map(one => one.plate), [
    { name: 'a', inFlight: 2, barrier: false, stuck: 1, alert: 'warn' },
    { name: 'b', inFlight: 0, barrier: false, stuck: 0, alert: null },
    { name: 'c', inFlight: 2, barrier: true, stuck: 1, alert: 'bad' },
    { name: 'd', inFlight: 0, barrier: false, stuck: 0, alert: null },
  ])
  assert.deepEqual(busy.islands.map(one => one.idle), [false, true, false, true], 'an island without in-flight tickets is idle')
  assert.deepEqual(busy.stuck.map(one => [one.identifier, one.project]), [['C-1', 'c'], ['A-2', 'a']], 'stuck tickets must cross projects, longest stay first')
  assert.equal(busy.global, true)
  const twins = model.archipelagoOf(sceneOf([named('x', [parcel('T-1', 'work')]), named('y', [parcel('T-1', 'gate')])]))
  const [first, second] = twins.tickets
  assert.ok(first.identifier === second.identifier && first.key !== second.key, 'the same ticket id in two projects must get two keys')
  assert.deepEqual(twins.islands.map(island => island.parcels.map(one => one.key)), [[first.key], [second.key]], 'each key must stay on its own island')
  assert.equal(model.archipelagoOf(sceneOf([named('solo', [])])).global, false, 'a single project must have no global stop')
  console.log('PASS diorama archipelago model: fixed positions, plates, idle islands, cross-project stuck order, single project')
  return model
}

function dependencies() {
  let playwright
  if (process.env.PLAYWRIGHT_PATH) playwright = require(process.env.PLAYWRIGHT_PATH)
  else {
    try {
      playwright = require('playwright')
    } catch (error) {
      if (error.code !== 'MODULE_NOT_FOUND') throw error
      playwright = require(path.join(execFileSync('npm', ['root', '-g'], { encoding: 'utf8' }).trim(), 'playwright'))
    }
  }
  const chrome = process.env.CHROME_PATH || ['/usr/bin/google-chrome', '/usr/bin/chromium', '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'].find(file => fs.existsSync(file))
  assert.ok(chrome && fs.existsSync(chrome), 'Chrome missing: set CHROME_PATH; browser tests cannot be skipped')
  const installedBun = path.join(process.env.BUN_INSTALL || path.join(os.homedir(), '.bun'), 'bin', 'bun')
  const bun = process.env.BUN_PATH || (fs.existsSync(installedBun) ? installedBun : 'bun')
  return { playwright, chrome, bun }
}

const serve = () => {
  const server = http.createServer((request, response) => {
    const file = path.join(plugin, decodeURIComponent(new URL(request.url, 'http://localhost').pathname))
    if (!servedRoots.some(root => file.startsWith(root)) || !fs.statSync(file, { throwIfNoEntry: false })?.isFile()) {
      response.writeHead(404).end()
      return
    }
    response.writeHead(200, { 'Content-Type': types[path.extname(file)] ?? 'text/plain', 'Cache-Control': 'no-store' })
    response.end(fs.readFileSync(file))
  })
  return new Promise(resolve => server.listen(0, '127.0.0.1', () => resolve(server)))
}

const frames = page => page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))))
const state = page => page.evaluate(() => window.diorama.state())

async function launchPage(browser, url, { webgl = true, ...options } = {}) {
  const page = await browser.newPage({ viewport: { width: 1440, height: 900 }, ...options })
  await page.addInitScript(enabled => {
    const getContext = HTMLCanvasElement.prototype.getContext
    const requestFrame = window.requestAnimationFrame
    window.contexts = []
    window.frameRequests = 0
    HTMLCanvasElement.prototype.getContext = function (kind, ...rest) {
      if (kind.startsWith('webgl') && !enabled) return null
      const context = getContext.call(this, kind, ...rest)
      if (kind.startsWith('webgl')) window.contexts.push(context)
      return context
    }
    window.requestAnimationFrame = callback => {
      window.frameRequests++
      return requestFrame.call(window, callback)
    }
  }, webgl)
  const errors = []
  const requests = []
  page.on('pageerror', error => errors.push(error.message))
  page.on('request', request => requests.push(request.url()))
  await page.goto(url)
  return { page, errors, requests }
}

async function open(browser, url, options = {}) {
  const opened = await launchPage(browser, url, options)
  await opened.page.waitForFunction(() => window.diorama)
  await frames(opened.page)
  return opened
}

const layoutReadsOver20Frames = page =>
  page.evaluate(async () => {
    let reads = 0
    const watched = [[HTMLElement.prototype, 'offsetWidth'], [HTMLElement.prototype, 'offsetHeight'], [Element.prototype, 'clientWidth'], [Element.prototype, 'clientHeight'], [Element.prototype, 'getBoundingClientRect']]
    const saved = watched.map(([owner, name]) => [owner, name, Object.getOwnPropertyDescriptor(owner, name)])
    for (const [owner, name, descriptor] of saved) {
      const counted = descriptor.get
        ? { get() { reads++; return descriptor.get.call(this) } }
        : { value(...args) { reads++; return descriptor.value.apply(this, args) } }
      Object.defineProperty(owner, name, { ...descriptor, ...counted })
    }
    for (let frame = 0; frame < 20; frame++) await new Promise(resolve => requestAnimationFrame(resolve))
    for (const [owner, name, descriptor] of saved) Object.defineProperty(owner, name, descriptor)
    return reads
  })

async function paintedShare(page) {
  const png = (await page.locator('canvas').screenshot()).toString('base64')
  return page.evaluate(async encoded => {
    const bitmap = await createImageBitmap(await (await fetch(`data:image/png;base64,${encoded}`)).blob())
    const canvas = new OffscreenCanvas(bitmap.width, bitmap.height)
    const context = canvas.getContext('2d')
    context.drawImage(bitmap, 0, 0)
    const { data } = context.getImageData(0, 0, bitmap.width, bitmap.height)
    const sky = [data[0], data[1], data[2]]
    let painted = 0
    for (let index = 0; index < data.length; index += 4) {
      if (Math.abs(data[index] - sky[0]) + Math.abs(data[index + 1] - sky[1]) + Math.abs(data[index + 2] - sky[2]) > 24) painted++
    }
    return painted / (data.length / 4)
  }, png)
}

const pixelOf = (view, point, size) => {
  const perUnit = size.height / (2 * view.half)
  const dx = point.x - view.x
  const dz = point.z - view.z
  const y = point.y + 0.37
  return {
    x: size.width / 2 + (perUnit * (dx - dz)) / Math.SQRT2,
    y: size.height / 2 - (perUnit * (2 * y - dx - dz)) / Math.sqrt(6),
  }
}

const settled = page =>
  page.waitForFunction(() => {
    const { view, goal } = window.diorama.state()
    return ['half', 'x', 'z'].every(key => Math.abs(view[key] - goal[key]) < 0.005)
  })

async function checkHumanQueue(browser, url) {
  const { page, errors } = await open(browser, url, { reducedMotion: 'reduce' })
  const fixture = {
    generatedAt: Date.now(), state: 'ok', maintenance: 'running',
    rows: [row(Array.from({ length: 7 }, (_, index) => parcel(`H-${index + 1}`, 'human', { since: 700 - index * 100, tone: 'failed' })))],
  }
  try {
    const geometry = await page.evaluate(async fixture => {
      const THREE = await import('three')
      const { createWorld } = await import('../../scene/page/diorama/world.mjs')
      const { archipelagoOf } = await import('../../scene/page/diorama/model.mjs')
      const add = THREE.Scene.prototype.add
      let scene
      THREE.Scene.prototype.add = function (...objects) {
        scene = this
        return add.apply(this, objects)
      }
      const world = createWorld(document.createElement('canvas'))
      THREE.Scene.prototype.add = add
      try {
        world.build(archipelagoOf(fixture))
        scene.updateMatrixWorld(true)
        const meshes = []
        scene.traverse(node => { if (node.isMesh) meshes.push(node) })
        const platform = meshes.find(mesh => mesh.scale.y === 1.5 && mesh.scale.x > 10)
        const bounds = new THREE.Box3().setFromObject(platform)
        const ground = new THREE.Box3().setFromObject(meshes.find(mesh => mesh.name === 'ground'))
        const containsXZ = (outer, inner) => ['x', 'z'].every(axis => inner.min[axis] >= outer.min[axis] && inner.max[axis] <= outer.max[axis])
        const bodies = [...world.parcels.values()].map(one => new THREE.Box3().setFromObject(one.group.children.find(child => child.userData.key)))
        return { count: bodies.length, onPlatform: bodies.every(body => containsXZ(bounds, body)), platformOnGround: containsXZ(ground, bounds) }
      } finally {
        world.dispose()
      }
    }, fixture)
    assert.equal(geometry.count, 7, 'geometry assertions must include the desk and two full waiting rows')
    assert.ok(geometry.onPlatform, 'every human box must stay inside the mezzanine footprint')
    assert.ok(geometry.platformOnGround, 'the expanded mezzanine must stay inside the island')
    await page.evaluate(fixture => window.diorama.update(fixture), fixture)
    for (const width of [1440, 760]) {
      await page.setViewportSize({ width, height: 900 })
      await frames(page)
      await settled(page)
      const labels = await page.locator('.label.ticket:visible').evaluateAll(nodes => nodes.map(node => ({ id: node.textContent, ...node.getBoundingClientRect().toJSON() })))
      assert.equal(labels.length, 7)
      const current = await state(page)
      const { islandOf } = await import(pathToFileURL(path.join(plugin, 'scene/page/diorama/model.mjs')))
      const slots = islandOf(fixture.rows[0]).parcels
      for (const label of labels) {
        const slot = slots.find(one => one.identifier === label.id).slot
        const pixel = pixelOf(current.view, slot, { width, height: 900 })
        assert.ok(Math.abs(label.x + label.width / 2 - pixel.x) < 1, `${label.id} label must stay horizontally aligned with its box at width ${width}`)
        for (const other of labels.filter(other => other.id !== label.id)) {
          assert.ok(label.right <= other.left || other.right <= label.left || label.bottom <= other.top || other.bottom <= label.top, `${label.id} and ${other.id} labels must not overlap at width ${width}`)
        }
      }
    }
    assert.deepEqual(errors, [])
    console.log('PASS human queue geometry: boxes on platform, platform on island, labels separated and horizontally anchored')
  } finally {
    await page.close()
  }
}

async function checkShoreline(browser, url) {
  const { page, errors } = await open(browser, url, { reducedMotion: 'reduce' })
  try {
    const result = await page.evaluate(async () => {
      const THREE = await import('three')
      const { createWorld } = await import('../../scene/page/diorama/world.mjs')
      const { archipelagoOf } = await import('../../scene/page/diorama/model.mjs')
      const fixture = await (await fetch('./diorama-scene.json')).json()
      const archipelago = archipelagoOf(fixture)
      const canvas = document.createElement('canvas')
      const add = THREE.Scene.prototype.add
      let scene, renderer
      THREE.Scene.prototype.add = function (...objects) {
        scene = this
        return add.apply(this, objects)
      }
      const world = createWorld(canvas)
      THREE.Scene.prototype.add = add
      scene.onBeforeRender = value => { renderer = value }
      const camera = new THREE.PerspectiveCamera()
      let worldDisposed = false
      const meshes = name => {
        const found = []
        scene.traverse(node => { if (node.isMesh && node.name === name) found.push(node) })
        return found
      }
      const counts = () => ['contact-shadow', 'shoreline', 'sea-pattern'].map(name => meshes(name).length)
      try {
        world.build(archipelago)
        world.render(camera)
        const global = counts()
        const contours = archipelago.islands.map(island => ({
          shore: meshes('shoreline').filter(mesh => mesh.position.x === island.origin.x && mesh.position.z === island.origin.z).length,
          shadow: meshes('contact-shadow').filter(mesh => mesh.position.x < island.origin.x && mesh.position.x > island.origin.x - 1 && mesh.position.z < island.origin.z && mesh.position.z > island.origin.z - 1).length,
        }))
        const lands = archipelago.islands.map(island => {
          const land = scene.children.flatMap(node => node.children).find(node => node.userData.island === island.index)
          return { idle: island.idle, colors: Object.fromEntries(land.children.filter(node => ['ground', 'ground-side', 'path', 'pier', 'water'].includes(node.name)).map(node => [node.name, node.material.color.getHex()])) }
        })
        const surface = meshes('sea-pattern')[0]
        const before = surface?.matrix.toArray()
        world.animate(1000, true)
        world.animate(9000, true)
        world.render(camera)
        const still = JSON.stringify(before) === JSON.stringify(surface?.matrix.toArray())
        const skies = ['clear', 'fog', 'overcast', 'dark'].map(kind => {
          world.setSky(kind)
          return { kind, pattern: surface?.material.color.getHex(), shore: meshes('shoreline')[0]?.material.color.getHex(), sea: meshes('sea')[0]?.material.color.getHex() }
        })
        const owned = new Set([...meshes('contact-shadow'), ...meshes('shoreline'), ...meshes('sea-pattern')].map(mesh => mesh.geometry))
        const disposed = new Set()
        for (const geometry of owned) geometry.addEventListener('dispose', () => disposed.add(geometry))
        world.build(archipelagoOf({ ...fixture, rows: [fixture.rows[0]] }))
        world.render(camera)
        const single = counts()
        world.build(archipelago)
        world.render(camera)
        const rebuilt = counts()
        const finalResources = new Set([...meshes('contact-shadow'), ...meshes('shoreline'), ...meshes('sea-pattern')].map(mesh => mesh.geometry))
        for (const mesh of [...meshes('shoreline'), ...meshes('sea-pattern')]) finalResources.add(mesh.material)
        const finalDisposals = new Map()
        for (const resource of finalResources) resource.addEventListener('dispose', () => finalDisposals.set(resource, (finalDisposals.get(resource) ?? 0) + 1))
        world.dispose()
        worldDisposed = true
        return { global, contours, islands: archipelago.islands.length, lands, still, skies, released: disposed.size, owned: owned.size, single, rebuilt, afterDispose: counts(), finalDisposals: [...finalResources].map(resource => finalDisposals.get(resource) ?? 0), shadows: renderer.shadowMap.enabled }
      } finally {
        if (!worldDisposed) world.dispose()
      }
    })
    assert.deepEqual(result.global, [result.islands + 1, result.islands + 1, 1], 'shoreline: every project island and the hub need contact-shadow and shoreline meshes, plus one sea-pattern')
    assert.ok(result.contours.every(contour => contour.shore === 1 && contour.shadow === 1), 'each island must have its own shoreline and a shadow offset away from the sun')
    const idle = result.lands.find(land => land.idle)
    const active = result.lands.find(land => !land.idle)
    assert.ok(idle && active, 'shoreline fixture must contain idle and active islands')
    assert.deepEqual(Object.keys(active.colors).sort(), ['ground', 'ground-side', 'path', 'pier', 'water'], 'the ground color assertion must cover every terrain material')
    assert.equal(active.colors.ground, 0xf0dfc2, 'the ground top must retain WORLD.base')
    assert.deepEqual(idle.colors, active.colors, 'idle ground, paths and waterways must keep the active island colors')
    assert.equal(result.still, true, 'sea-pattern must remain static with reduced motion')
    assert.equal(new Set(result.skies.map(sky => sky.pattern)).size, 4, 'sea-pattern must follow all four sky states')
    assert.equal(new Set(result.skies.map(sky => sky.shore)).size, 4, 'shoreline must follow all four sea colors')
    assert.deepEqual(result.skies.map(sky => sky.sea), [0xb4d6cc, 0xc6d3cf, 0xb3bdba, 0x6b7876], 'the four sea base colors must stay unchanged')
    for (const sky of result.skies) {
      const shoreBrightness = brightness(sky.shore)
      const seaBrightness = brightness(sky.sea)
      assert.ok(shoreBrightness > seaBrightness, `${sky.kind} shoreline brightness ${shoreBrightness} must exceed sea brightness ${seaBrightness}`)
    }
    assert.equal(result.shadows, false, 'contact shadows must not enable renderer shadow maps')
    assert.deepEqual(result.single, [0, 0, 0], 'a single project must have no shoreline, contact-shadow or sea-pattern')
    assert.equal(result.released, result.owned, 'rebuilding must release every owned shoreline geometry')
    assert.deepEqual(result.rebuilt, result.global, 'rebuilding the islands must restore each shoreline once')
    assert.deepEqual(result.afterDispose, [0, 0, 0], 'dispose must leave no shoreline meshes in the scene')
    assert.ok(result.finalDisposals.every(count => count === 1), 'dispose must release every owned coast geometry and shared shoreline material exactly once')
    if (shots) {
      for (const sky of ['clear', 'fog', 'overcast', 'dark']) {
        await page.evaluate(async sky => {
          const fixture = await (await fetch('./diorama-scene.json')).json()
          window.diorama.update({ ...fixture, state: sky === 'dark' ? 'unknown' : sky === 'fog' ? 'stale' : 'ok', maintenance: sky === 'overcast' ? 'paused' : 'running' })
        }, sky)
        await frames(page)
        await page.screenshot({ path: path.join(shots, `of447-global-${sky}.png`) })
      }
      await page.evaluate(async () => {
        const fixture = await (await fetch('./diorama-scene.json')).json()
        window.diorama.update({ ...fixture, rows: [fixture.rows[0]] })
      })
      await frames(page)
      await page.screenshot({ path: path.join(shots, 'of447-single.png') })
    }
    assert.deepEqual(errors, [])
    console.log('PASS shoreline in real Chrome: island and hub contours, idle ground colors, four skies, static sea-pattern, single project and disposal')
  } finally {
    await page.close()
  }
}

async function checkMainPath(browser, url) {
  const { page, errors } = await open(browser, url)
  try {
    const start = await state(page)
    assert.equal(start.stop, 1, 'several projects must open at the global stop')
    assert.ok(await paintedShare(page) > 0.2, 'the global stop must render islands, not a blank sea')
    const { islands } = await page.evaluate(() => window.diorama.archipelago)
    assert.equal(await page.locator('button.plate:visible').count(), islands.length, 'every island must show its name plate')
    assert.equal(await page.locator('.dock').isVisible(), false, 'the project dock must hide at the global stop')
    assert.equal(await page.locator('.stuck > .list button[data-ticket]').count(), 2)

    await page.locator('canvas').focus()
    for (let presses = 0; presses < 12; presses++) {
      if (await page.evaluate(() => document.activeElement?.dataset.ticket)) break
      await page.keyboard.press('Tab')
    }
    const focused = await page.evaluate(() => document.activeElement?.querySelector('b').textContent)
    assert.equal(focused, 'OF-415', 'the stuck list must be reachable with Tab')
    await page.keyboard.press('Enter')
    const entered = await state(page)
    assert.deepEqual([entered.stop, entered.focus, entered.selected], [2, '/home/laoxu/github/orca-factory', 'OF-415'], 'a stuck ticket must open its island and select it')
    assert.equal(await page.locator('.dock').isVisible(), true, 'the project dock must show at the workshop stop')
    await settled(page)
    const reads = await layoutReadsOver20Frames(page)
    assert.ok(reads <= 20, `label placement must not read layout more than once per frame (${reads} reads in 20 frames)`)
    const trip = await page.locator('.trip').innerText()
    for (const text of ['OF-415', '归属 处置Agent', '处置塔前', '二线处理中']) assert.ok(trip.includes(text), `trip bar must show ${text}: ${trip}`)

    await settled(page)
    const before = await state(page)
    const island = islands.find(one => one.path === entered.focus)
    const target = island.parcels.find(one => one.identifier === 'OF-419')
    const size = page.viewportSize()
    const point = pixelOf(before.view, { x: target.slot.x + island.origin.x, y: target.slot.y, z: target.slot.z + island.origin.z }, size)
    await page.mouse.click(point.x, point.y)
    assert.equal((await state(page)).selected, 'OF-419', 'clicking a box must select its ticket')
    assert.ok((await page.locator('.trip').innerText()).includes('OF-419'))

    await page.mouse.move(size.width / 2, size.height / 2)
    await page.mouse.down()
    await page.mouse.move(size.width / 2 + 140, size.height / 2 + 50, { steps: 6 })
    await page.mouse.up()
    const dragged = await state(page)
    assert.ok(dragged.view.x !== before.view.x || dragged.view.z !== before.view.z, 'dragging must pan the camera')
    await page.locator('canvas').focus()
    for (const key of ['ArrowLeft', 'ArrowUp', '3', 'ArrowRight', 'Home']) await page.keyboard.press(key)
    await page.mouse.wheel(0, 400)
    await frames(page)
    assert.equal((await state(page)).stop, 3, 'key 3 must move to the ticket stop')
    await page.keyboard.press('Escape')
    assert.deepEqual([(await state(page)).stop, (await state(page)).selected], [2, 'OF-419'], 'Esc must step back from the ticket to the workshop')
    await page.keyboard.press('Escape')
    assert.deepEqual([(await state(page)).stop, (await state(page)).selected], [2, null], 'Esc must then clear the selection')
    await page.keyboard.press('Escape')
    await frames(page)
    const after = await state(page)
    assert.equal(after.stop, 1, 'Esc must finally step back to the global stop')
    assert.equal(await page.locator('.dock').isVisible(), false)
    assert.deepEqual(after.quaternion, start.quaternion, 'the camera must never rotate')
    assert.deepEqual(errors, [])
    console.log('PASS diorama in real Chrome: global stop, plates, Tab/Enter stuck list opens the island, click pick, Esc back to global, fixed rotation')
  } finally {
    await page.close()
  }
}

async function checkReducedMotion(browser, url) {
  const { page, errors } = await open(browser, url, { reducedMotion: 'reduce' })
  try {
    const first = await state(page)
    assert.equal(first.reduced, true)
    assert.ok(first.marks.length > 0, 'the recorded attention tickets must show markers')
    for (const mark of first.marks) assert.deepEqual(mark, { y: MARK_REST, rotation: 0 }, 'markers must not float when motion is reduced')
    await page.locator('button.plate[data-island="1"]').focus()
    await page.evaluate(async () => {
      const scene = await (await fetch('./diorama-scene.json')).json()
      window.diorama.update(scene)
      window.diorama.update(scene)
    })
    await frames(page)
    assert.equal(await page.evaluate(() => document.activeElement?.dataset.island), '1', 'two Scenes before a frame must keep keyboard focus on the island plate')
    await page.locator('canvas').focus()
    for (let press = 0; press < 8; press++) await page.keyboard.press('ArrowDown')
    await frames(page)
    await page.locator('button.arrow[data-island="0"]').focus()
    await page.evaluate(async () => window.diorama.update(await (await fetch('./diorama-scene.json')).json()))
    await frames(page)
    const arrowFocus = await page.evaluate(() => [document.activeElement?.classList.contains('arrow'), document.activeElement?.dataset.island])
    assert.deepEqual(arrowFocus, [true, '0'], 'a new Scene must keep keyboard focus on the edge arrow, not move it to the plate')
    await page.locator('.stuck > .list button[data-ticket]').first().click()
    await page.locator('canvas').focus()
    await page.keyboard.press('3')
    const moved = await state(page)
    assert.equal(moved.stop, 3)
    assert.deepEqual(moved.view, moved.goal, 'the camera must jump straight to its goal when motion is reduced')
    const reshape = change => page.evaluate(async change => {
      const scene = await (await fetch('./diorama-scene.json')).json()
      const parcels = scene.rows[0].parcels
      if (change === 'move') parcels.find(one => one.identifier === 'OF-415').station = 'gate'
      else scene.rows[0].parcels = parcels.filter(one => one.identifier !== 'OF-415')
      window.diorama.update(scene)
    }, change)
    await reshape('move')
    await frames(page)
    const jumped = await state(page)
    assert.deepEqual([jumped.stop, jumped.selected], [3, 'OF-415'], 'an update must keep the stop and the selection')
    assert.notDeepEqual(jumped.target, moved.target, 'the selected box must get its new station')
    assert.deepEqual(jumped.at, jumped.target, 'a moved box must jump to its new station when motion is reduced')
    await reshape('remove')
    assert.equal((await state(page)).selected, null, 'an update must clear a selection whose ticket left the Scene')
    const refused = await page.evaluate(async () => {
      const before = JSON.stringify(window.diorama.state())
      const scene = await (await fetch('./diorama-scene.json')).json()
      try {
        window.diorama.update({ ...scene, rows: [] })
      } catch (error) {
        return { message: error.message, kept: JSON.stringify(window.diorama.state()) === before }
      }
      return { message: null, kept: JSON.stringify(window.diorama.state()) === before }
    })
    assert.ok(refused.message?.includes('没有项目') && refused.kept, `update must refuse an empty Scene before it changes anything: ${JSON.stringify(refused)}`)
    const released = await page.evaluate(async () => {
      window.diorama.dispose()
      const requested = window.frameRequests
      await new Promise(resolve => setTimeout(resolve, 200))
      return { children: document.querySelector('.diorama').childElementCount, frames: window.frameRequests - requested, lost: window.contexts[0].isContextLost() }
    })
    assert.deepEqual(released, { children: 0, frames: 0, lost: true }, 'dispose must remove the stage content, stop the frame loop and release the WebGL context')
    assert.deepEqual(errors, [])
    console.log('PASS diorama reduced motion and lifecycle: still markers, camera and box jumps, update keeps or clears the selection, dispose releases')
  } finally {
    await page.close()
  }
}

const pushedStatus = live => {
  const sample = fs.readFileSync(path.join(plugin, 'test/sample-recorded.ts'), 'utf8')
  const [barrier, unmapped] = JSON.parse(JSON.parse(sample.match(/RECORDED_DEPLOY_STATUS: string = ("[\s\S]*?")\n/)[1])).tickets
  const ledger = readJSON(path.resolve(plugin, '../../../driver/testdata/recorded/ledger-comments-deployment-retry-workflow.json'))
  const paused = ledger.comments
    .flatMap(comment => comment.body.split('\n'))
    .filter(line => line.startsWith('merge-progress-v1: '))
    .map(line => JSON.parse(line.slice('merge-progress-v1: '.length)).deployment)
    .find(deployment => deployment.state === 'paused')
  const changes = { 'OF-415': { owner: '执行器', phase: 'gate' }, 'OF-419': { title: 'A & B <img src=x onerror="window.__xss=1">' } }
  return {
    ...live,
    state: 'unknown',
    tickets: [
      ...live.tickets.map(ticket => ({ ...ticket, ...changes[ticket.identifier] })),
      { ...barrier, blockedReason: `deployment: ${paused.state}`, deployment: { ...barrier.deployment, state: paused.state, reason: paused.reason } },
      unmapped,
      { ...live.tickets.find(ticket => ticket.identifier === 'OF-415'), repo: TWIN, owner: '合并通道', phase: '已合并' },
      { ...live.tickets.find(ticket => ticket.identifier === 'OF-419'), repo: ANSIBLE, owner: '合并通道', phase: '已合并' },
    ],
  }
}

const fakeDriver = (file, recordedConfig) => (status, { failing = false, config = recordedConfig } = {}) => {
  fs.writeFileSync(file, `#!/usr/bin/env bun
if (process.argv[2] === 'config') console.log(${JSON.stringify(JSON.stringify(config))})
else if (${failing}) { console.error('fixture failure'); process.exit(7) }
else console.log(${JSON.stringify(JSON.stringify(status))})
`)
  fs.chmodSync(file, 0o755)
}

const launchServer = (bun, temp, token, port) => {
  const bunDir = path.isAbsolute(bun) ? `${path.dirname(bun)}:` : ''
  const env = { ...process.env, PATH: `${temp}:${bunDir}${process.env.PATH}`, ZIIKOO_SCENE_TOKEN: token, ZIIKOO_SCENE_BIND: '127.0.0.1', ZIIKOO_SCENE_PORT: String(port) }
  const child = spawn(bun, [path.join(plugin, 'scene/server.ts')], { env, stdio: ['ignore', 'pipe', 'inherit'] })
  const origin = new Promise((resolve, reject) => {
    let output = ''
    const timer = setTimeout(() => reject(new Error('scene server did not listen')), 15000)
    child.on('exit', code => reject(new Error(`scene server exited ${code}`)))
    child.stdout.on('data', bytes => {
      output += bytes
      const match = /listening (http:\/\/[^/]+)/.exec(output)
      if (match) {
        clearTimeout(timer)
        resolve(match[1])
      }
    })
  })
  const halt = () => new Promise(resolve => {
    if (child.exitCode !== null || child.signalCode !== null) return resolve()
    child.once('exit', resolve)
    child.kill('SIGTERM')
  })
  return { origin, halt }
}

async function waitForSnapshot(origin, token) {
  const deadline = Date.now() + 15000
  for (;;) {
    const response = await fetch(`${origin}/snapshot?t=${token}`)
    if (response.status === 200) return response.json()
    assert.equal(response.status, 503, 'the snapshot may only wait with 503')
    assert.ok(Date.now() < deadline, 'the snapshot stayed 503 past the deadline')
    await new Promise(resolve => setTimeout(resolve, 250))
  }
}

const pageFiles = dir =>
  fs.readdirSync(dir, { withFileTypes: true }).flatMap(entry => (entry.isDirectory() ? pageFiles(path.join(dir, entry.name)) : [path.join(dir, entry.name)]))

async function checkTokens(origin, token) {
  const files = pageFiles(pageRoot).filter(file => file !== path.join(pageRoot, 'index.html'))
  const routes = ['/', '/snapshot', '/events', ...files.map(file => `/${path.relative(pageRoot, file)}`)]
  for (const route of routes) {
    assert.equal((await fetch(origin + route)).status, 404, `${route} must refuse a request without the token`)
    for (const response of [await fetch(`${origin}${route}?t=${token}`), await fetch(origin + route, { headers: { 'X-Token': token } })]) {
      assert.ok(response.status === 200 || (route === '/snapshot' && response.status === 503), `${route} must serve with the token (${response.status})`)
      await response.body?.cancel()
    }
    assert.equal((await fetch(`${origin}${route}?t=${token}`, { method: 'POST' })).status, 404, `${route} must refuse POST`)
  }
  for (const retired of ['/scene.js', '/town.mjs', '/components/index.mjs', '/components/shared.css', '/vendor/OrbitControls.js']) {
    assert.equal((await fetch(`${origin}${retired}?t=${token}`)).status, 404, `${retired} must stay retired`)
  }
}

const leadText = page => page.locator('.lead').innerText()
const waitLead = (page, text, present = true) =>
  page.waitForFunction(([text, present]) => document.querySelector('.lead').innerText.includes(text) === present, [text, present], { timeout: 15000 })
const tripHas = async (page, texts) => {
  const trip = await page.locator('.trip').innerText()
  for (const text of texts) assert.ok(trip.includes(text), `trip bar must show ${text}: ${trip}`)
}

async function copied(page, name) {
  await page.evaluate(() => {
    window.copied = undefined
    Object.defineProperty(navigator.clipboard, 'writeText', { configurable: true, value: async text => { window.copied = text } })
  })
  await page.getByRole('button', { name }).click()
  return page.waitForFunction(() => window.copied).then(handle => handle.jsonValue())
}

async function checkLiveUpdates(browser, live) {
  const { page, errors, requests } = await open(browser, `${live.origin()}/?t=${live.token}`)
  try {
    assert.equal((await state(page)).stop, 1, 'several recorded projects must open at the global stop')
    assert.ok(await paintedShare(page) > 0.2, 'the production page must render the islands')
    await waitLead(page, '已连接')
    assert.ok((await leadText(page)).includes('快照过期'), 'the recorded snapshot is days old and must read as stale')
    const recorded = readJSON(path.join(plugin, 'test/recorded/live-status.json'))
    const scanInterval = Date.parse(recorded.nextScanAt) - Date.parse(recorded.generatedAt)
    const generatedAt = Date.now() - 60_000
    live.write({ ...recorded, generatedAt: new Date(generatedAt).toISOString(), nextScanAt: new Date(generatedAt + scanInterval).toISOString() })
    await waitLead(page, '快照过期', false)
    for (const text of ['已连接', 'driver ok']) assert.ok((await leadText(page)).includes(text), `the recorded snapshot within the next scan grace period must show ${text}`)
    if (shots) await page.screenshot({ path: path.join(shots, 'of444-prod-global.png') })
    await page.locator('.stuck > .list button[data-ticket="0/OF-415"]').click()
    await settled(page)
    await tripHas(page, ['OF-415', '处置塔前', '二线处理中', '复制票号', '复制详情'])
    const before = await state(page)
    live.write(live.pushed)
    await page.waitForFunction(() => document.querySelector('.trip').innerText.includes('检验间'), null, { timeout: 15000 })
    const moved = await state(page)
    assert.deepEqual([moved.stop, moved.focus, moved.selected], [2, FACTORY, 'OF-415'], 'a new Scene must keep the island and the selected ticket')
    assert.deepEqual([moved.goal, moved.quaternion], [before.goal, before.quaternion], 'a new Scene must not move or rotate the camera')
    assert.ok(['half', 'x', 'z'].every(key => Math.abs(moved.view[key] - before.view[key]) < 0.01), 'a new Scene must not jump the settled view')
    assert.notDeepEqual(moved.target, before.target, 'the selected box must get its new station')
    assert.notDeepEqual(moved.at, moved.target, 'the box must travel to its new station, not jump')
    await page.waitForFunction(() => { const { at, target } = window.diorama.state(); return at.every((value, index) => value === target[index]) })
    assert.equal(await page.evaluate(() => window.contexts.length), 1, 'a new Scene must reuse the WebGL context')
    await waitLead(page, 'driver unknown')

    await page.locator('.stuck > .list button[data-ticket="0/OF-419"]').click()
    const detail = await copied(page, '复制详情')
    for (const text of ['OF-419 · A & B <img src=x', '项目：orca-factory · /home/laoxu/github/orca-factory', '工位：处置塔前', '真实阶段：Agent处置 / 二线处理中']) {
      assert.ok(detail.includes(text), `copied detail must carry ${text}: ${detail}`)
    }
    assert.equal(await copied(page, '复制票号'), 'OF-419')
    assert.equal(await page.locator('.diorama img').count(), 0, 'Scene text must render as text')
    assert.equal(await page.evaluate(() => window.__xss), undefined)

    live.write(live.pushed, { failing: true })
    await waitLead(page, 'fixture failure')
    live.write(live.pushed)
    await waitLead(page, 'fixture failure', false)
    live.write(live.pushed, { failing: true })
    await waitLead(page, 'fixture failure')
    await live.stop()
    await waitLead(page, '连接断开')
    if (shots) await page.screenshot({ path: path.join(shots, 'of444-prod-offline.png') })
    live.write({ ...live.pushed, state: 'stale' }, { config: { ...live.config, overview: { ...live.config.overview, maintenance: 'paused' } } })
    await live.start()
    await waitLead(page, '已连接')
    await waitLead(page, '维护 paused')
    for (const text of ['维护中', '快照过期']) assert.ok((await leadText(page)).includes(text), `maintenance with a stale snapshot must show ${text}`)
    assert.ok(!(await leadText(page)).includes('fixture failure'), 'a failure that recovered while offline must not survive the reconnect')

    const count = route => requests.filter(url => new URL(url).pathname === route).length
    const [streams, snapshots] = [count('/events'), count('/snapshot')]
    await page.evaluate(() => window.dispatchEvent(new PageTransitionEvent('pagehide', { persisted: true })))
    assert.ok((await leadText(page)).includes('连接断开'), 'pagehide must stop reporting a live connection')
    await page.evaluate(() => window.dispatchEvent(new PageTransitionEvent('pageshow', { persisted: true })))
    await waitLead(page, '已连接')
    assert.ok(count('/events') > streams && count('/snapshot') > snapshots, 'pageshow must reopen the stream and refetch the snapshot')

    live.write({ ...live.pushed, tickets: [] }, { config: { ...live.config, repos: [] } })
    await live.stop()
    await live.start()
    await page.waitForFunction(() => !window.diorama && document.querySelector('.diorama .notice')?.textContent.includes('暂无项目'), null, { timeout: 15000 })
    assert.equal(await page.locator('.diorama canvas').count(), 0, 'an empty Scene must unmount the diorama')
    live.write(live.pushed)
    await live.stop()
    await live.start()
    await page.waitForFunction(() => window.diorama, null, { timeout: 15000 })
    await waitLead(page, '已连接')
    assert.equal(await page.locator('.diorama .notice').count(), 0, 'a Scene with projects must mount the diorama again')

    const refuse = url => new URL(url).pathname === '/events'
    await page.route(refuse, route => route.fulfill({ status: 404 }))
    await live.stop()
    await waitLead(page, '连接已关闭：请从 /ziikoo-driver 面板重新打开。')
    assert.ok(!(await leadText(page)).includes('正在自动重连'), 'a closed stream must not promise a reconnect')
    await page.unroute(refuse)
    await live.start()

    assert.equal(await page.evaluate(() => localStorage.length + sessionStorage.length), 0)
    const own = requests.filter(url => url.startsWith(live.origin()))
    for (const file of ['/live.mjs', '/diorama/overlay.mjs', '/diorama/diorama.css', '/vendor/three.core.min.js']) {
      assert.ok(own.some(url => new URL(url).pathname === file), `the page must load ${file}`)
    }
    assert.ok(own.every(url => new URL(url).searchParams.get('t') === live.token), 'every page request must carry the token')
    assert.deepEqual(errors, [])
    console.log('PASS production page on the real server: render, update keeps camera and selection, box travels, copy, failures, offline, reconnect, maintenance with a stale snapshot, pagehide/pageshow, empty Scene, closed stream, tokens')
  } finally {
    await page.close()
  }
}

async function checkEntries(browser, live, model) {
  const { islands } = model.archipelagoOf(await waitForSnapshot(live.origin(), live.token))
  const { page, errors } = await open(browser, `${live.origin()}/?t=${live.token}&project=${encodeURIComponent(ANSIBLE)}`)
  const where = async () => {
    const { stop, focus, selected } = await state(page)
    return [stop, focus, selected]
  }
  const find = async text => {
    await page.locator('canvas').focus()
    await page.keyboard.press('/')
    await page.keyboard.type(text)
    await page.keyboard.press('Enter')
  }
  try {
    assert.deepEqual(await where(), [2, ANSIBLE, null], '?project= must open that island')
    await page.locator('canvas').focus()
    await page.keyboard.press(']')
    assert.deepEqual(await where(), [2, islands[3].path, null], '] must step to the next island')
    await page.keyboard.press('[')
    assert.deepEqual(await where(), [2, ANSIBLE, null], '[ must step back')
    await page.keyboard.press('1')
    await settled(page)
    const dashboard = islands.find(island => island.path === DASHBOARD)
    const ground = { x: dashboard.origin.x + (dashboard.layout.ground.left + dashboard.layout.ground.right) / 2, y: -0.37, z: dashboard.origin.z - 2.5 }
    const point = pixelOf((await state(page)).view, ground, page.viewportSize())
    await page.mouse.click(point.x, point.y)
    assert.deepEqual((await where()).slice(0, 2), [2, DASHBOARD], 'clicking an island must enter it')

    await find('ZY-365')
    assert.deepEqual(await where(), [2, DASHBOARD, 'ZY-365'], 'search Enter must jump to the first match')
    await tripHas(page, ['闸前水道', '部署暂停', 'deployment: paused'])
    const url = new URL(page.url())
    assert.deepEqual(['project', 'ticket', 't'].map(name => url.searchParams.get(name)), [DASHBOARD, 'ZY-365', live.token], 'the URL must follow the view')
    await page.keyboard.press('3')
    await settled(page)
    if (shots) await page.screenshot({ path: path.join(shots, 'of444-prod-ticket.png') })
    await find('ZY-366')
    await tripHas(page, ['工位未知 · 阶段 准备镜像'])
    await page.locator('canvas').focus()
    await page.keyboard.press('/')
    await page.keyboard.type('OF-415')
    const twins = await page.locator('.results button[data-ticket]').evaluateAll(buttons => buttons.map(button => button.dataset.ticket))
    assert.equal(new Set(twins).size, 2, `the same ticket id in two projects must list twice: ${twins}`)
    assert.deepEqual(errors, [])
  } finally {
    await page.close()
  }
  const twin = await open(browser, `${live.origin()}/?t=${live.token}&project=${encodeURIComponent(TWIN)}&ticket=OF-415`)
  try {
    const { stop, focus, selected } = await state(twin.page)
    assert.deepEqual([stop, focus, selected], [3, TWIN, 'OF-415'], '?project=&ticket= must open the ticket in that project')
    await tripHas(twin.page, ['栈桥 · 部署未知', '部署：未知 · driver 未提供 deployment'])
    assert.ok((await copied(twin.page, '复制详情')).includes('部署：未知 · driver 未提供 deployment'), 'copied detail must carry the unknown deployment')
    assert.ok((await twin.page.locator('.stuck > .list').innerText()).includes('栈桥 · 部署未知'), 'an unknown deployment must list as stuck')
    await settled(twin.page)
    if (shots) await twin.page.screenshot({ path: path.join(shots, 'of444-prod-unknown-deploy.png') })
    assert.deepEqual(twin.errors, [])
  } finally {
    await twin.page.close()
  }
  const plain = await open(browser, `${live.origin()}/?t=${live.token}&project=${encodeURIComponent(ANSIBLE)}&ticket=OF-419`)
  try {
    await tripHas(plain.page, ['栈桥 · 已合并', '部署：不适用 · 未配置部署'])
    assert.ok(!(await plain.page.locator('.trip').innerText()).includes('部署未知'), 'a project without deployment must not say 部署未知')
    assert.ok((await copied(plain.page, '复制详情')).includes('部署：不适用 · 未配置部署'), 'copied detail must say deployment does not apply')
    assert.ok(!(await plain.page.locator('.stuck > .list').innerText()).includes('ansible'), 'a merged ticket without deployment config is not stuck')
    assert.deepEqual(plain.errors, [])
  } finally {
    await plain.page.close()
  }
  console.log('PASS production entries: ?project=, [ and ], island click, search Enter, URL, ?ticket= with a twin id, barrier, unknown station, unknown deployment, no deployment configured')
}

async function checkWithoutWebGL(browser, live) {
  const { page, errors } = await launchPage(browser, `${live.origin()}/?t=${live.token}`, { webgl: false })
  try {
    const failure = page.locator('.diorama .failure')
    await failure.waitFor()
    assert.ok((await failure.innerText()).includes('WebGL'), 'the page must say WebGL is unavailable')
    assert.equal(await page.locator('canvas').count(), 0, 'the page must not leave a blank canvas')
    assert.deepEqual(errors, [])
    console.log('PASS production page without WebGL: clear reason, no blank canvas')
  } finally {
    await page.close()
  }
}

async function checkRecordLinks(browser, live) {
  const url = 'https://records.example.test/base?record=rec123&view=%22grid%22'
  const urls = [url, undefined, 'javascript:window.__xss=1', 'JaVaScRiPt:alert(1)', 'http://records.example.test/', 'data:text/html,unsafe', 'file:///etc/passwd', '//records.example.test/', '/record/123', 'https://']
  const recorded = readJSON(path.join(plugin, 'test/recorded/live-status.json'))
  const template = recorded.tickets.find(ticket => ticket.identifier === 'OF-415')
  const tickets = urls.map((url, index) => ({ ...template, identifier: `RECORD-${index}`, url }))
  live.write({ ...recorded, tickets })
  const { page, errors } = await open(browser, `${live.origin()}/?t=${live.token}`)
  try {
    await page.waitForFunction(() => document.querySelector('.stuck > .list').textContent.includes('RECORD-0'))
    const listLink = page.locator('.stuck > .list').getByRole('link', { name: '打开记录', exact: true })
    assert.equal(await listLink.count(), 1, 'only the HTTPS record may have a stuck-list link')
    await page.locator('.stuck > .list button[data-ticket="0/RECORD-0"]').focus()
    await page.keyboard.press('Tab')
    assert.equal(await listLink.evaluate(link => link === document.activeElement), true, 'Tab must reach the record link next to its ticket')
    const before = await state(page)
    await page.context().route('https://records.example.test/**', route => route.fulfill({ body: 'record' }))
    const popupReady = page.waitForEvent('popup')
    await page.keyboard.press('Enter')
    const popup = await popupReady
    await popup.waitForLoadState()
    assert.equal(popup.url(), url)
    assert.equal(await popup.evaluate(() => window.opener), null, 'the record tab must not access its opener')
    await popup.close()
    assert.equal((await state(page)).selected, before.selected, 'opening a record must not select a different ticket')
    for (const [index, value] of urls.entries()) {
      await page.locator(`.stuck > .list button[data-ticket="0/RECORD-${index}"]`).click()
      const tripLink = page.locator('.trip').getByRole('link', { name: '打开记录', exact: true })
      assert.equal(await tripLink.count(), index === 0 ? 1 : 0, `trip record link for ${value}`)
      if (index !== 0) continue
      for (const link of [listLink, tripLink]) {
        assert.equal(await link.getAttribute('href'), url)
        assert.equal(await link.evaluate(link => link.protocol), 'https:')
        assert.equal(await link.getAttribute('target'), '_blank')
        assert.equal(await link.getAttribute('rel'), 'noopener noreferrer')
        assert.equal(await link.evaluate(link => link.closest('button')), null, 'links must not nest inside a button')
      }
      await page.getByRole('button', { name: '复制详情', exact: true }).focus()
      await page.keyboard.press('Tab')
      assert.equal(await tripLink.evaluate(link => link === document.activeElement), true, 'Tab must reach the trip record link')
      live.write({ ...recorded, tickets: tickets.map(ticket => ({ ...ticket, title: 'updated record' })) })
      await page.waitForFunction(() => document.querySelector('.trip .title').textContent === 'updated record')
      assert.equal(await tripLink.evaluate(link => link === document.activeElement), true, 'a new Scene must preserve focus on the record link')
    }
    assert.equal(await page.evaluate(() => window.__xss), undefined)
    assert.deepEqual(errors, [])
    console.log('PASS record links: HTTPS only, missing and unsafe URLs omitted, safe new tab, Tab and Enter, focus survives updates')
  } finally {
    await page.close()
  }
}

async function checkProduction(browser, bun, model) {
  const tempRoot = path.join(process.cwd(), '.omc/tmp')
  fs.mkdirSync(tempRoot, { recursive: true })
  const temp = fs.mkdtempSync(path.join(tempRoot, 'diorama-'))
  const status = readJSON(path.join(plugin, 'test/recorded/live-status.json'))
  const config = readJSON(path.join(plugin, 'test/recorded/live-config.json'))
  const token = crypto.randomBytes(24).toString('hex')
  const write = fakeDriver(path.join(temp, 'orca-factory-driver'), config)
  write(status)
  let server = launchServer(bun, temp, token, 0)
  let origin = await server.origin
  const live = {
    token,
    config,
    write,
    pushed: pushedStatus(status),
    origin: () => origin,
    stop: () => server.halt(),
    async start() {
      server = launchServer(bun, temp, token, new URL(origin).port)
      origin = await server.origin
    },
  }
  try {
    await checkTokens(origin, token)
    await waitForSnapshot(origin, token)
    console.log('PASS production server: every page resource, snapshot and stream refuse requests without the token')
    await checkLiveUpdates(browser, live)
    await checkEntries(browser, live, model)
    await checkRecordLinks(browser, live)
    await checkWithoutWebGL(browser, live)
  } finally {
    await server.halt()
    fs.rmSync(temp, { recursive: true, force: true })
  }
}

async function main() {
  const { playwright, chrome, bun } = dependencies()
  execFileSync(bun, [path.join(preview, 'diorama-scene.ts')], { stdio: 'inherit' })
  const model = await checkModel()
  const server = await serve()
  const url = `http://127.0.0.1:${server.address().port}/test/design-preview/diorama.html`
  try {
    const browser = await playwright.chromium.launch({
      executablePath: chrome,
      headless: true,
      args: ['--no-sandbox', '--use-angle=swiftshader', '--enable-unsafe-swiftshader'],
    })
    try {
      await checkHumanQueue(browser, url)
      await checkShoreline(browser, url)
      await checkMainPath(browser, url)
      await checkReducedMotion(browser, url)
      await checkProduction(browser, bun, model)
    } finally {
      await browser.close()
    }
  } finally {
    await new Promise(resolve => server.close(resolve))
  }
}

main().catch(error => {
  console.error(error)
  process.exitCode = 1
})
