const assert = require('node:assert/strict')
const fs = require('node:fs')
const http = require('node:http')
const os = require('node:os')
const path = require('node:path')
const { execFileSync } = require('node:child_process')
const { pathToFileURL } = require('node:url')

const plugin = path.resolve(__dirname, '..')
const preview = path.join(plugin, 'test/design-preview')
const servedRoots = [path.join(plugin, 'scene/page') + path.sep, preview + path.sep]
const types = { '.html': 'text/html', '.mjs': 'text/javascript', '.js': 'text/javascript', '.css': 'text/css', '.json': 'application/json' }
const STATIONS = ['pickup', 'work', 'return', 'gate', 'review', 'pack', 'merge', 'dispatch', 'smoke', 'repair', 'human']
const MARK_REST = 1.45

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
  console.log('PASS diorama view model: station mapping, booths, queue order, missing position, unknown station')
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

async function open(browser, url, options = {}) {
  const page = await browser.newPage({ viewport: { width: 1440, height: 900 }, ...options })
  const errors = []
  page.on('pageerror', error => errors.push(error.message))
  await page.goto(url)
  await page.waitForFunction(() => window.diorama)
  await frames(page)
  return { page, errors }
}

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

const pixelOf = (view, slot, size) => {
  const perUnit = size.height / (2 * view.half)
  const dx = slot.x - view.x
  const dz = slot.z - view.z
  const y = slot.y + 0.37
  return {
    x: size.width / 2 + (perUnit * (dx - dz)) / Math.SQRT2,
    y: size.height / 2 - (perUnit * (2 * y - dx - dz)) / Math.sqrt(6),
  }
}

async function checkMainPath(browser, url) {
  const { page, errors } = await open(browser, url)
  try {
    assert.ok(await paintedShare(page) > 0.2, 'the diorama must render geometry, not a blank sky')
    const island = await page.evaluate(() => window.diorama.island)
    assert.equal(island.path, '/home/laoxu/github/orca-factory', 'the default island must be the first project with tickets')
    assert.equal(await page.locator('.stuck > .list button[data-ticket]').count(), 2)

    await page.locator('canvas').focus()
    for (let presses = 0; presses < 12; presses++) {
      if (await page.evaluate(() => document.activeElement?.dataset.ticket)) break
      await page.keyboard.press('Tab')
    }
    const focused = await page.evaluate(() => document.activeElement?.dataset.ticket)
    assert.equal(focused, 'OF-415', 'the stuck list must be reachable with Tab')
    await page.keyboard.press('Enter')
    const trip = await page.locator('.trip').innerText()
    for (const text of ['OF-415', '归属 处置Agent', '处置塔前', '二线处理中']) assert.ok(trip.includes(text), `trip bar must show ${text}: ${trip}`)

    const before = await state(page)
    const target = island.parcels.find(one => one.identifier === 'OF-419')
    const size = page.viewportSize()
    const point = pixelOf(before.view, target.slot, size)
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
    await frames(page)
    const after = await state(page)
    assert.equal(after.stop, 2, 'Esc must step back to the workshop stop')
    assert.deepEqual(after.quaternion, before.quaternion, 'the camera must never rotate')
    assert.deepEqual(errors, [])
    console.log('PASS diorama in real Chrome: painted island, Tab/Enter list, trip bar stage and owner, click pick, fixed rotation')
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
    await page.locator('.stuck > .list button[data-ticket]').first().click()
    await page.locator('canvas').focus()
    await page.keyboard.press('3')
    const moved = await state(page)
    assert.equal(moved.stop, 3)
    assert.deepEqual(moved.view, moved.goal, 'the camera must jump straight to its goal when motion is reduced')
    assert.deepEqual(errors, [])
    console.log('PASS diorama reduced motion: still markers, camera jumps to the goal')
  } finally {
    await page.close()
  }
}

async function main() {
  await checkModel()
  const { playwright, chrome, bun } = dependencies()
  execFileSync(bun, [path.join(preview, 'diorama-scene.ts')], { stdio: 'inherit' })
  const server = await serve()
  const url = `http://127.0.0.1:${server.address().port}/test/design-preview/diorama.html`
  try {
    const browser = await playwright.chromium.launch({
      executablePath: chrome,
      headless: true,
      args: ['--no-sandbox', '--use-angle=swiftshader', '--enable-unsafe-swiftshader'],
    })
    try {
      await checkMainPath(browser, url)
      await checkReducedMotion(browser, url)
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
