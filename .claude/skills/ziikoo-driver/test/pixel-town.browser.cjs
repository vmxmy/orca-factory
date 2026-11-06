const assert = require('node:assert/strict')
const fs = require('node:fs')
const path = require('node:path')
const { pathToFileURL } = require('node:url')
const { execFileSync, spawn } = require('node:child_process')
const http = require('node:http')
const crypto = require('node:crypto')

const plugin = path.resolve(__dirname, '..')
const pageRoot = path.join(plugin, 'scene/page')
const parcel = (identifier, station = 'work') => ({ identifier, title: '长中文标题'.repeat(12), phase: 'worker 运行中', label: '施工', station, duration: '2 分钟', tone: 'current', unknownDeploy: false })
const snapshot = count => ({ generatedAt: Date.now(), state: 'ok', maintenance: 'running', rows: Array.from({ length: count }, (_, index) => ({ name: '同名中文工坊'.repeat(8), path: `/fixture/repo-${index}`, workers: 4, deploy: index % 2 === 0, parcels: [parcel('OF-1')] })) })

async function main() {
  const model = await import(pathToFileURL(path.join(pageRoot, 'town.mjs')))
  for (const count of [0, 1, 2, 4, 6, 12, 24, 100, 1000, 1001]) {
    const scene = snapshot(count)
    const layout = model.townLayout(scene.rows)
    assert.equal(layout.projects.size, count)
    if (!count) continue
    const last = layout.projects.get(`/fixture/repo-${count - 1}`)
    assert.ok(last)
    assert.ok(model.visibleProjects(layout, { x: last.x - 150, y: last.y - 120, width: 300, height: 240 }).some(one => one.path === last.path))
    if (count === 1) assert.deepEqual([last.x, last.y], [0, 0])
    if (count > 1) assert.ok(last.y >= Math.floor((count - 1) / 2) * 240)
    assert.ok(model.visibleProjects(layout, { x: -400, y: -120, width: 800, height: 500 }).length <= 8)
  }
  const crowded = snapshot(1).rows[0]
  crowded.parcels = Array.from({ length: 90 }, (_, index) => parcel(`OF-${index}`))
  const workshop = model.workshopLayout(crowded)
  assert.equal(new Set([...workshop.parcels.values()].map(point => `${point.x},${point.y}`)).size, 90)
  const positions = [...workshop.parcels.values()]
  for (const [index, point] of positions.entries()) for (const other of positions.slice(index + 1)) assert.ok(Math.abs(point.x - other.x) >= 34 || Math.abs(point.y - other.y) >= 43, 'crowded parcel hit areas must not overlap')
  assert.equal(model.stationOf('new-backend-stage'), 'unknown')
  const route = model.travelPath(workshop, 'review', 'return')
  assert.ok(route.length >= 4)
  for (let index = 1; index < route.length; index++) assert.ok(route[index].x === route[index - 1].x || route[index].y === route[index - 1].y)
  assert.ok(model.travelPath(workshop, 'return', 'work').length >= 4)
  for (const source of Object.keys(model.stations)) for (const target of Object.keys(model.stations)) {
    const points = model.travelPath(workshop, source, target)
    for (let index = 1; index < points.length; index++) assert.ok(points[index].x === points[index - 1].x || points[index].y === points[index - 1].y, `${source} to ${target} must follow orthogonal belts`)
  }
  let state = model.reconcile(model.initialState(), snapshot(2), { now: 0, fresh: true })
  state.project = '/fixture/repo-1'
  state.selection = { kind: 'parcel', key: model.parcelKey(state.project, 'OF-1') }
  state.camera = { x: 321, y: 654, zoom: 1.2 }
  const identical = model.reconcile(state, state.scene, { now: 10 })
  assert.equal(identical, state)
  const changed = structuredClone(state.scene)
  changed.rows[1].parcels[0].station = 'gate'
  state = model.reconcile(state, changed, { now: 20 })
  assert.equal(state.project, '/fixture/repo-1')
  assert.equal(state.camera.x, 321)
  assert.equal(state.motions.size, 1)
  const reconnected = model.reconcile(state, changed, { now: 30, fresh: true })
  assert.equal(reconnected.motions.size, 0)
  const removed = model.reconcile(state, snapshot(0), { now: 40 })
  assert.equal(removed.project, null)
  assert.equal(removed.selection, null)
  assert.equal(removed.motions.size, 0)
  let anchored = model.reconcile(model.initialState(), snapshot(1), { fresh: true })
  anchored = model.reconcile(anchored, snapshot(2))
  assert.equal(anchored.camera.x, -175)
  anchored.selection = { kind: 'parcel', key: model.parcelKey('/fixture/repo-0', 'OF-1') }
  const reordered = structuredClone(anchored.scene)
  reordered.rows.reverse()
  anchored = model.reconcile(anchored, reordered)
  assert.equal(anchored.project, '/fixture/repo-0')
  assert.equal(anchored.camera.x, 175)
  assert.ok(anchored.selection)
  const vanished = structuredClone(anchored.scene)
  vanished.rows.find(row => row.path === anchored.project).parcels = []
  anchored = model.reconcile(anchored, vanished)
  assert.equal(anchored.selection, null)
  assert.equal(anchored.motions.size, 0)
  const convoy = snapshot(1)
  convoy.rows[0].parcels = Array.from({ length: 4 }, (_, index) => parcel(`OF-${index}`))
  let convoyState = model.reconcile(model.initialState(), convoy, { fresh: true })
  const arriving = structuredClone(convoy)
  arriving.rows[0].parcels.forEach(one => { one.station = 'gate' })
  convoyState = model.reconcile(convoyState, arriving, { now: 0 })
  const midway = [...convoyState.motions.values()].map(motion => model.pointAlong(motion.points, .5))
  for (const [index, point] of midway.entries()) for (const other of midway.slice(index + 1)) assert.ok(Math.abs(point.x - other.x) >= 33 || Math.abs(point.y - other.y) >= 43, 'simultaneous parcels must keep separate belt lanes')

  const movingOnes = snapshot(1)
  movingOnes.rows[0].parcels = [parcel('OF-leaver'), parcel('OF-stayer')]
  let mixedState = model.reconcile(model.initialState(), movingOnes, { fresh: true })
  const leaverRoute = structuredClone(movingOnes)
  leaverRoute.rows[0].parcels[0].station = 'gate'
  mixedState = model.reconcile(mixedState, leaverRoute, { now: 0 })
  const leaver = model.pointAlong(mixedState.motions.get(model.parcelKey('/fixture/repo-0', 'OF-leaver')).points, 0)
  const stayer = mixedState.motions.get(model.parcelKey('/fixture/repo-0', 'OF-stayer'))
  assert.ok(stayer, 'a remaining parcel whose slot is taken over must animate, not teleport')
  const stayerPoint = model.pointAlong(stayer.points, 0)
  assert.ok(Math.abs(leaver.x - stayerPoint.x) >= 27 || Math.abs(leaver.y - stayerPoint.y) >= 27, 'a leaving parcel must not overlap the parcel taking its slot')
  const sweep = (motions, step = 5) => {
    const end = Math.max(...motions.map(motion => motion.start + motion.duration))
    assert.ok(Number.isFinite(end), `motion schedule must be finite numbers (end ${end})`)
    let min = Infinity
    let samples = 0
    for (let time = 0; time <= end; time += step) {
      samples++
      const points = motions.map(motion => model.pointAlong(motion.points, (time - motion.start) / motion.duration))
      for (let index = 0; index < points.length; index++) for (const other of points.slice(index + 1)) min = Math.min(min, Math.hypot(points[index].x - other.x, points[index].y - other.y))
    }
    assert.ok(samples > 0, `sweep must sample the tween at least once (end ${end})`)
    return min
  }
  // draw.mjs paints each parcel as a 26x26 box, so two parcels overlap when
  // their centers are under 26px apart on either axis. The sweep above only
  // measures the euclidean distance, which can stay above 26 while the boxes
  // already intersect on a 45° crossing.
  const sweepBoxes = (motions, step = 5) => {
    const end = Math.max(...motions.map(motion => motion.start + motion.duration))
    assert.ok(Number.isFinite(end), `motion schedule must be finite numbers (end ${end})`)
    let min = Infinity
    let samples = 0
    for (let time = 0; time <= end; time += step) {
      samples++
      const points = motions.map(motion => model.pointAlong(motion.points, (time - motion.start) / motion.duration))
      for (let index = 0; index < points.length; index++) for (const other of points.slice(index + 1)) min = Math.min(min, Math.max(Math.abs(points[index].x - other.x), Math.abs(points[index].y - other.y)))
    }
    assert.ok(samples > 0, `box sweep must sample the tween at least once (end ${end})`)
    return min
  }
  for (const motion of mixedState.motions.values()) {
    assert.ok(Number.isFinite(motion.start) && Number.isFinite(motion.duration), 'motion start must be a timestamp, not a screen position')
    assert.ok(model.pointAlong(motion.points, 0.5).x > 0 || model.pointAlong(motion.points, 0.5).y > 0, 'mid-tween position must stay finite')
  }
  const swapMotions = [...mixedState.motions.values()]
  assert.ok(sweep(swapMotions) >= 26, `leaver and the parcel taking its slot must never overlap mid-tween (min ${sweep(swapMotions).toFixed(1)}px)`)
  assert.ok(sweepBoxes(swapMotions, 1) >= 26, `painted 26x26 parcels must never intersect on the leaver/stayer tween (axis min ${sweepBoxes(swapMotions, 1).toFixed(1)}px)`)

  const fiveAtWork = snapshot(1)
  fiveAtWork.rows[0].parcels = Array.from({ length: 5 }, (_, index) => parcel(`OF-${index}`))
  let fiveState = model.reconcile(model.initialState(), fiveAtWork, { fresh: true })
  const fiveMove = structuredClone(fiveAtWork)
  fiveMove.rows[0].parcels.forEach(one => { one.station = 'pack' })
  fiveState = model.reconcile(fiveState, fiveMove, { now: 0 })
  const fiveMin = sweep([...fiveState.motions.values()])
  assert.ok(fiveMin >= 26, `a cross-belt convoy across slot rows must keep clearance mid-tween (min ${fiveMin.toFixed(1)}px)`)

  // A convoy leaving one station while a displaced parcel waits on their lane:
  // every mover dips whole to the service row around the waiting parcel's old
  // slot, so no painted box crosses a waiting box at any point of the tween.
  const convLeaver = snapshot(1)
  convLeaver.rows[0].parcels = [parcel('OF-A'), parcel('OF-B'), parcel('OF-C')]
  let convState = model.reconcile(model.initialState(), convLeaver, { fresh: true })
  const convMove = structuredClone(convLeaver)
  convMove.rows[0].parcels[0].station = 'gate'
  convMove.rows[0].parcels[1].station = 'gate'
  convState = model.reconcile(convState, convMove, { now: 0 })
  const convMin = sweepBoxes([...convState.motions.values()], 1)
  assert.ok(convMin >= 26, `a convoy must clear a waiting displaced parcel without box overlap (axis min ${convMin.toFixed(1)}px)`)

  // The service-row dive must also clear parcels waiting on the second slot
  // row: the dive column is the mover's old slot column, which can be exactly
  // where a deeper-row waiter idles. Waiters stay put until movers finish, so
  // the dive has to route around (not through) every waiting box.
  const refill = snapshot(1)
  refill.rows[0].parcels = Array.from({ length: 5 }, (_, index) => parcel(`OF-${index}`))
  let refillState = model.reconcile(model.initialState(), refill, { fresh: true })
  const refillMove = structuredClone(refill)
  refillMove.rows[0].parcels[0].station = 'gate'
  refillMove.rows[0].parcels[1].station = 'gate'
  refillState = model.reconcile(refillState, refillMove, { now: 0 })
  const refillLayout = model.workshopLayout(refillMove.rows[0])
  const refillEnd = Math.max(...[...refillState.motions.values()].map(motion => motion.start + motion.duration))
  let refillMin = Infinity
  for (let time = 0; time <= refillEnd; time++) {
    const points = [...refillLayout.parcels].map(([key, point]) => {
      const motion = refillState.motions.get(key)
      return motion ? model.pointAlong(motion.points, (time - motion.start) / motion.duration) : point
    })
    for (let index = 0; index < points.length; index++) for (const other of points.slice(index + 1)) refillMin = Math.min(refillMin, Math.max(Math.abs(points[index].x - other.x), Math.abs(points[index].y - other.y)))
  }
  assert.ok(refillMin >= 26, `a service-row dive must clear second-row waiters too (axis min ${refillMin.toFixed(1)}px)`)

  // A mid-tween reconcile feeds fractional coordinates back into path
  // drafting; blocked-column detection must stay interval-based instead of
  // stepping pixel by pixel toward an integer endpoint (which never arrives).
  const crowd = snapshot(1)
  crowd.rows[0].parcels = Array.from({ length: 170 }, (_, index) => parcel(`OF-${index}`))
  let crowdState = model.reconcile(model.initialState(), crowd, { fresh: true })
  const crowdMove = structuredClone(crowd)
  crowdMove.rows[0].parcels[0].station = 'pack'
  crowdState = model.reconcile(crowdState, crowdMove, { now: 0 })
  const reRoute = structuredClone(crowdMove)
  reRoute.rows[0].parcels[0].station = 'review'
  const reState = model.reconcile(crowdState, reRoute, { now: 5000 })
  const reMotion = reState.motions.get(model.parcelKey('/fixture/repo-0', 'OF-0'))
  assert.ok(reMotion, 'a mid-tween reconcile must reschedule the moving parcel')
  for (let index = 1; index < reMotion.points.length; index++) {
    assert.ok(reMotion.points[index].x === reMotion.points[index - 1].x || reMotion.points[index].y === reMotion.points[index - 1].y, `a rescheduled route must stay orthogonal (segment ${index})`)
  }

  const backward = snapshot(1)
  backward.rows[0].parcels = [parcel('OF-back', 'pack')]
  let backwardState = model.reconcile(model.initialState(), backward, { fresh: true })
  const backwardMove = structuredClone(backward)
  backwardMove.rows[0].parcels[0].station = 'work'
  backwardState = model.reconcile(backwardState, backwardMove, { now: 0 })
  const backwardMotion = backwardState.motions.get(model.parcelKey('/fixture/repo-0', 'OF-back'))
  for (let index = 1; index < backwardMotion.points.length; index++) {
    assert.ok(backwardMotion.points[index].x === backwardMotion.points[index - 1].x || backwardMotion.points[index].y === backwardMotion.points[index - 1].y, `a backward mainline move must follow orthogonal belt segments (segment ${index})`)
  }
  // A backward move rides the drawn U-shaped belt: it climbs its own station
  // column to the upper lane, rides that lane left, and only the dock steps
  // leave the painted belt between lane and slot row.
  const backwardBelt = model.travelPath(model.workshopLayout(backwardMove.rows[0]), 'pack', 'work')
  const backwardColumns = [...new Set(backwardBelt.map(point => point.x))]
  const backwardLanes = [...new Set(backwardBelt.map(point => point.y))]
  const backwardClimb = backwardMotion.points.some(point => point.x === backwardColumns[0] && point.y <= Math.min(...backwardLanes))
  assert.ok(backwardClimb, 'a backward move must first ascend its own belt corner column, not cross vertically through the dispatch/work column')
  for (const [index, point] of backwardMotion.points.entries()) {
    if (!index) continue
    const previous = backwardMotion.points[index - 1]
    if (previous.y === point.y) assert.ok(backwardLanes.includes(point.y), `a backward horizontal segment must ride a drawn belt lane (y ${point.y})`)
    else assert.ok(backwardColumns.includes(point.x), `a backward vertical segment must ride a belt corner column (x ${point.x})`)
  }

  const one = snapshot(1)
  one.rows[0].parcels = [parcel('OF-solo')]
  let soloState = model.reconcile(model.initialState(), one, { fresh: true })
  const soloMove = structuredClone(one)
  soloMove.rows[0].parcels[0].station = 'pack'
  soloState = model.reconcile(soloState, soloMove, { now: 0 })
  const soloMotion = soloState.motions.get(model.parcelKey('/fixture/repo-0', 'OF-solo'))
  const horizontals = soloMotion.points.filter((point, index) => index > 0 && soloMotion.points[index - 1].y === point.y)
  const verticals = soloMotion.points.filter((point, index) => index > 0 && soloMotion.points[index - 1].x === point.x)
  assert.ok(horizontals.length >= 1 && verticals.length >= 1, 'a mid-belt move must keep both a horizontal and a vertical segment')
  const soloLayout = model.workshopLayout(one.rows[0])
  const packLayout = model.workshopLayout(soloMove.rows[0])
  const upperLane = soloLayout.nodes.get('work').y + 85
  const lowerLane = packLayout.nodes.get('pack').y + 85
  const beltPoints = ['pickup', 'work', 'gate', 'review', 'pack', 'merge', 'dispatch', 'smoke'].map(station => {
    const node = soloLayout.nodes.get(station)
    return { x: node.x, y: node.y + 85 }
  })
  const soloSlot = soloLayout.parcels.get(model.parcelKey('/fixture/repo-0', 'OF-solo'))
  const slotOffset = Math.hypot(soloSlot.x - beltPoints[1].x, soloSlot.y - beltPoints[1].y)
  assert.ok(Math.abs(soloSlot.x - beltPoints[1].x) < 26 && Math.abs(soloSlot.y - beltPoints[1].y) < 26, 'fixture slot must sit within a parcel width of its belt lane')
  for (const [index, point] of soloMotion.points.entries()) {
    if (!index) continue
    const previous = soloMotion.points[index - 1]
    if (previous.y === point.y) assert.ok([upperLane, lowerLane].includes(point.y), 'horizontal tween segments must ride the drawn belt lanes')
    else assert.ok(beltPoints.some(belt => Math.abs(point.x - belt.x) <= 12), 'vertical tween segments must ride the drawn belt corner columns')
  }
  const beltHalf = 12
  const stray = soloMotion.points.filter(point => !beltPoints.some(belt => Math.abs(point.x - belt.x) <= beltHalf || Math.abs(point.y - belt.y) <= beltHalf))
  assert.ok(stray.length === 0, `every tween waypoint must sit on the painted belt polyline (worst ${stray[0] ? Math.round(stray[0].x) + ',' + Math.round(stray[0].y) : ''})`)
  // The embedded deploy-barrier sample must be the repository's real ledger
  // recording, not hand-written data: compare against the recorded file.
  const recordedLedger = fs.readFileSync(path.resolve(plugin, '../../../driver/testdata/recorded/ledger-comments-deployment-retry-workflow.json'), 'utf8')
  const sampleSource = fs.readFileSync(path.join(plugin, 'test/sample-recorded.ts'), 'utf8')
  const embeddedText = sampleSource.match(/RECORDED_LEDGER_DEPLOYMENT: string = String\.raw`([\s\S]*?)`/)?.[1]
  assert.ok(embeddedText, 'sample-recorded.ts must embed RECORDED_LEDGER_DEPLOYMENT via String.raw')
  const ledger = JSON.parse(embeddedText)
  assert.deepEqual(ledger, JSON.parse(recordedLedger))
  const handoffLine = ledger.comments.flatMap(comment => comment.body.split('\n')).find(line => line.startsWith('merge-handoff-v1: '))
  const progressLine = ledger.comments.flatMap(comment => comment.body.split('\n')).find(line => line.startsWith('merge-progress-v1: ') && line.includes('"state":"pending"'))
  const frozen = JSON.parse(handoffLine.slice('merge-handoff-v1: '.length))
  const pending = JSON.parse(progressLine.slice('merge-progress-v1: '.length))
  const barrierStatus = JSON.parse(JSON.parse(sampleSource.match(/RECORDED_DEPLOY_STATUS: string = ("[\s\S]*?")\n/)?.[1]))
  const barrierTicket = barrierStatus.tickets.find(ticket => ticket.identifier === 'ZY-365')
  assert.equal(barrierTicket.deployment.queueId, frozen.queue_id)
  assert.equal(barrierTicket.deployment.sha, frozen.head)
  assert.equal(barrierTicket.deployment.workflow, pending.deployment.workflow)
  assert.equal(barrierTicket.deployment.state, pending.deployment.state)
  assert.equal(barrierTicket.blockedReason, `deployment: ${pending.deployment.state}`)
  console.log('PASS pure layout, identity, routing, reconciliation and unbounded project counts')

  let playwright
  if (process.env.PLAYWRIGHT_PATH) playwright = require(process.env.PLAYWRIGHT_PATH)
  else {
    try { playwright = require('playwright') }
    catch (error) {
      if (error.code !== 'MODULE_NOT_FOUND') throw error
      const globalRoot = execFileSync('npm', ['root', '-g'], { encoding: 'utf8' }).trim()
      playwright = require(path.join(globalRoot, 'playwright'))
    }
  }
  const chrome = process.env.CHROME_PATH || ['/usr/bin/google-chrome', '/usr/bin/chromium', '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome'].find(file => fs.existsSync(file))
  assert.ok(chrome && fs.existsSync(chrome), 'Chrome missing: set CHROME_PATH; browser tests cannot be skipped')
  const token = crypto.randomBytes(24).toString('hex')
  let scene = snapshot(1)
  const streams = new Set()
  const requests = []
  const server = http.createServer((request, response) => {
    const url = new URL(request.url, 'http://localhost')
    requests.push({ pathname: url.pathname, authorized: url.searchParams.get('t') === token })
    if (request.method !== 'GET' || url.searchParams.get('t') !== token) { response.writeHead(404).end(); return }
    response.setHeader('Cache-Control', 'no-store')
    if (url.pathname === '/events') {
      response.writeHead(200, { 'Content-Type': 'text/event-stream' })
      response.write(`event: scene\ndata: ${JSON.stringify(scene)}\n\n`)
      streams.add(response)
      request.on('close', () => streams.delete(response))
      return
    }
    if (url.pathname === '/snapshot') { response.setHeader('Content-Type', 'application/json'); response.end(JSON.stringify(scene)); return }
    const name = url.pathname === '/' ? 'index.html' : url.pathname.slice(1)
    if (!['index.html', 'scene.js', 'town.mjs', 'draw.mjs'].includes(name)) { response.writeHead(404).end(); return }
    response.setHeader('Content-Type', name.endsWith('.html') ? 'text/html' : 'text/javascript')
    response.end(fs.readFileSync(path.join(pageRoot, name), 'utf8').replaceAll('__TOKEN__', encodeURIComponent(token)))
  })
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve))
  const origin = `http://127.0.0.1:${server.address().port}`
  let browser
  try { browser = await playwright.chromium.launch({ executablePath: chrome, headless: true, args: ['--no-sandbox'] }) }
  catch (error) { await new Promise(resolve => server.close(resolve)); throw error }
  const page = await browser.newPage({ viewport: { width: 1440, height: 960 } })
  const errors = []
  page.on('pageerror', error => errors.push(error.message))
  const emit = async next => {
    scene = next
    for (const response of streams) response.write(`event: scene\ndata: ${JSON.stringify(scene)}\n\n`)
    await page.waitForFunction(signature => window.pixelTown?.state.signature === signature, JSON.stringify(scene))
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))))
  }
  try {
    await page.goto(`${origin}/?t=${token}`)
    await page.waitForFunction(() => window.pixelTown?.state.scene)
    for (const count of [0, 1, 2, 4, 6, 12, 24, 100, 1000, 1001]) {
      await emit(snapshot(count))
      if (!count) { await assertVisible(page.locator('#empty')); assert.equal(await page.locator('#enter').isDisabled(), true); continue }
      await page.locator('#search').fill(`/fixture/repo-${count - 1}`)
      await page.locator('#projects button').last().click()
      assert.equal(await page.evaluate(() => window.pixelTown.state.project), `/fixture/repo-${count - 1}`, `last project reachable at count ${count}; page errors: ${errors.join(', ')}`)
      await page.locator('#enter').click()
      await page.waitForFunction(() => window.pixelTown.state.view === 'workshop')
      await page.locator('#parcels button').first().click()
      assert.ok((await page.locator('#detail').innerText()).includes('长中文标题'.repeat(12)))
      await page.locator('#overview').click()
      const position = await page.evaluate(() => ({ camera: window.pixelTown.state.camera, project: window.pixelTown.layout.projects.get(window.pixelTown.state.project) }))
      assert.equal(position.camera.x, position.project.x)
      assert.equal(position.camera.y, position.project.y)
      await page.locator('#search').fill('')
      if (count === 6 && process.env.PIXEL_TOWN_SCREENSHOT) await page.screenshot({ path: process.env.PIXEL_TOWN_SCREENSHOT })
    }
    await emit(snapshot(1))
    const building = await page.evaluate(() => window.pixelTown.selectedScreenPoint())
    await page.mouse.click(building.x, building.y)
    assert.equal(await page.evaluate(() => window.pixelTown.state.view), 'workshop', 'building hit area shares the derived layout')
    for (const station of Object.keys(model.stations)) {
      await page.locator(`#nodes button[data-key="${station}"]`).click()
      assert.ok((await page.locator('#detail').innerText()).includes(model.stations[station][1]))
    }
    if (process.env.PIXEL_TOWN_SCREENSHOT) {
      await page.locator('#detail button').first().click()
      await page.locator('#zoom-out').click()
      await page.locator('#reset').click()
      await page.screenshot({ path: process.env.PIXEL_TOWN_SCREENSHOT.replace(/\.png$/, '-workshop.png') })
    }
    await page.locator('#overview').click()
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))))
    const idleBaseDraws = await page.evaluate(() => window.pixelTown.metrics.baseDraws)
    await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))))
    assert.ok(await page.evaluate(() => window.pixelTown.metrics.baseDraws) <= idleBaseDraws + 1, 'device animation cannot dirty the static cache')
    const sizes = await page.evaluate(() => [...document.querySelectorAll('canvas')].map(canvas => [canvas.width, canvas.height]))
    assert.ok(sizes.every(([width, height]) => width <= 2880 && height <= 1920), 'canvas must stay viewport-sized')
    const before = await page.evaluate(() => window.pixelTown.metrics.resizes)
    await page.locator('#zoom-in').click()
    await page.mouse.move(800, 300)
    await page.mouse.down()
    await page.mouse.move(850, 350)
    await page.mouse.up()
    assert.equal(await page.evaluate(() => window.pixelTown.metrics.resizes), before, 'pan/zoom must not resize canvas')
    await emit(snapshot(2))
    await page.locator('#projects button').last().click()
    await page.locator('#enter').click()
    await page.locator('#parcels button').first().click()
    const key = await page.evaluate(() => window.pixelTown.state.selection.key)
    const changed = structuredClone(scene)
    changed.rows[1].parcels[0].station = 'return'
    changed.rows[1].parcels[0].tone = 'attention'
    await emit(changed)
    await page.waitForFunction(() => window.pixelTown.state.motions.size === 1)
    assert.equal(await page.evaluate(() => window.pixelTown.state.selection.key), key)
    const camera = await page.evaluate(() => window.pixelTown.state.camera)
    await emit(changed)
    assert.deepEqual(await page.evaluate(() => window.pixelTown.state.camera), camera)
    await page.emulateMedia({ reducedMotion: 'reduce' })
    changed.rows[1].parcels[0].station = 'work'
    await emit(changed)
    await page.waitForFunction(() => window.pixelTown.state.motions.size === 0)
    await page.setViewportSize({ width: 390, height: 844 })
    await page.locator('#parcels button').first().click()
    const mobile = await page.evaluate(() => {
      const point = window.pixelTown.selectedScreenPoint()
      const canvas = document.querySelector('#map').getBoundingClientRect()
      const detail = document.querySelector('#detail').getBoundingClientRect()
      return { point, canvas: { left: canvas.left, top: canvas.top, right: canvas.right, bottom: canvas.bottom }, detail: { top: detail.top } }
    })
    assert.ok(mobile.point.x >= mobile.canvas.left && mobile.point.x <= mobile.canvas.right)
    assert.ok(mobile.point.y >= mobile.canvas.top && mobile.point.y <= mobile.canvas.bottom)
    assert.ok(mobile.canvas.bottom <= mobile.detail.top + 1)
    if (process.env.PIXEL_TOWN_SCREENSHOT) await page.screenshot({ path: process.env.PIXEL_TOWN_SCREENSHOT.replace(/\.png$/, '-mobile.png') })
    await page.locator('#overview').click()
    await emit(snapshot(1))
    assert.equal(await page.evaluate(() => window.pixelTown.state.project), '/fixture/repo-0')
    assert.equal(await page.evaluate(() => window.pixelTown.state.selection), null)
    assert.ok(!new URL(page.url()).searchParams.has('parcel'))
    const exceptional = snapshot(1)
    exceptional.state = 'stale'
    exceptional.maintenance = 'paused'
    exceptional.rows[0].parcels = [{ ...parcel('OF-unknown', 'alien'), unknownDeploy: true, deployment: 'paused', reason: '部署凭据错误', tone: 'failed' }]
    await emit(exceptional)
    await page.locator('#enter').click()
    await page.locator('#parcels button').click()
    assert.ok((await page.locator('#detail').innerText()).includes('未知阶段'))
    assert.ok((await page.locator('#detail').innerText()).includes('paused'))
    assert.ok((await page.locator('#status').innerText()).includes('过期'))
    assert.ok((await page.locator('#status').innerText()).includes('paused'))
    await page.locator('#attention').click()
    await page.locator('#detail button').last().click()
    assert.ok((await page.locator('#detail').innerText()).includes('部署凭据错误'))
    for (const response of streams) response.write('event: error\ndata: {"command":"status","reason":"fixture failure","exitCode":7}\n\n')
    await page.waitForFunction(() => document.querySelector('#status').textContent.includes('fixture failure'))
    for (const response of streams) response.write('event: error\ndata: {"command":"status","recovered":true}\n\n')
    await page.waitForFunction(() => !document.querySelector('#status').textContent.includes('fixture failure'))
    for (const response of streams) response.write('event: error\ndata: {"command":"config","reason":"offline failure","exitCode":9}\n\n')
    await page.waitForFunction(() => document.querySelector('#status').textContent.includes('offline failure'))
    // The config poll recovers while offline; its recovered event is not
    // replayed on reconnect, so the page must not keep showing the error once
    // the stream is healthy again. This asserts the ordering the executor's
    // reviewer flagged: fail -> disconnect -> recover offline -> reconnect.
    for (const response of streams) response.end()
    await page.waitForFunction(() => document.querySelector('#status').textContent.includes('断开'))
    await page.waitForFunction(() => document.querySelector('#status').textContent.includes('已连接'), { timeout: 15000 })
    const statusAfterReconnect = await page.locator('#status').innerText()
    assert.ok(!statusAfterReconnect.includes('offline failure'), 'a command error that recovered during a disconnect must not survive the reconnect')
    assert.equal(await page.evaluate(() => window.pixelTown.state.motions.size), 0)
    await page.locator('#overview').click()
    const undeployed = snapshot(1)
    undeployed.rows[0].deploy = false
    undeployed.rows[0].parcels[0].unknownDeploy = true
    await emit(undeployed)
    await page.locator('#enter').click()
    await page.locator('#parcels button').click()
    assert.ok((await page.locator('#detail').innerText()).includes('不适用 · 未配置部署'))
    assert.ok((await page.locator('#detail').innerText()).includes('部署字段：未知'))
    await page.locator('#overview').focus()
    await page.keyboard.press('Enter')
    assert.equal(await page.evaluate(() => window.pixelTown.state.view), 'town')
    assert.equal(await page.evaluate(() => localStorage.length + sessionStorage.length), 0)
    assert.ok(requests.filter(one => one.pathname.endsWith('.mjs')).length >= 2)
    assert.ok(requests.every(one => one.authorized), 'all resource and event requests must keep authentication')
    assert.deepEqual(errors, [])
    console.log('PASS real Chrome: counts, search, identity, camera, viewport caches, mobile, accessibility, SSE and errors')
  } finally {
    for (const response of streams) response.end()
    await new Promise(resolve => server.close(resolve))
  }
  await checkProductionServer(browser)
  await browser.close()
}

async function assertVisible(locator) { assert.equal(await locator.isVisible(), true) }

async function checkProductionServer(browser) {
  const tempRoot = path.join(process.cwd(), '.omc/tmp')
  fs.mkdirSync(tempRoot, { recursive: true })
  const temp = fs.mkdtempSync(path.join(tempRoot, 'pixel-town-'))
  const token = crypto.randomBytes(24).toString('hex')
  const fake = path.join(temp, 'orca-factory-driver')
  // The fake driver replays real recordings only: the status is the recorded
  // `status --json` output extended with the two ledger-derived tickets (the
  // paused ZY-365 deployment barrier and the 等人批 ZY-366 turnover), and the
  // config is the recorded `config --json` output verbatim — no hand-written
  // SAMPLE_CONFIG on this path. The real parseRuntime → parseConfig → sceneOf
  // chain in scene/server.ts turns both tickets into the page's deploy-paused
  // and unknown-stage branches.
  const sampleSource = fs.readFileSync(path.join(plugin, 'test/sample-recorded.ts'), 'utf8')
  const replayStatus = JSON.parse(JSON.parse(sampleSource.match(/RECORDED_REPLAY_STATUS: string = ("[\s\S]*?")\n/)?.[1]))
  const replayConfig = JSON.parse(JSON.parse(sampleSource.match(/RECORDED_CONFIG: string = ("[\s\S]*?")\n/)?.[1]))
  const ledger = JSON.parse(fs.readFileSync(path.resolve(plugin, '../../../driver/testdata/recorded/ledger-comments-deployment-retry-workflow.json'), 'utf8'))
  const progressLines = ledger.comments.flatMap(comment => comment.body.split('\n')).filter(line => line.startsWith('merge-progress-v1: '))
  const pausedLine = progressLines.map(line => JSON.parse(line.slice('merge-progress-v1: '.length))).find(progress => progress.deployment?.state === 'paused')
  assert.ok(pausedLine, 'recorded ledger must contain a paused merge-progress record')
  const handoffLine = ledger.comments.flatMap(comment => comment.body.split('\n')).find(line => line.startsWith('merge-handoff-v1: '))
  const frozen = JSON.parse(handoffLine.slice('merge-handoff-v1: '.length))
  const humanLedger = JSON.parse(fs.readFileSync(path.resolve(plugin, '../../../driver/testdata/recorded/ledger-comments-human-release.json'), 'utf8'))
  const turnover = humanLedger.comments.filter(comment => comment.body.startsWith('转人: ZY-366')).at(-1)
  assert.ok(turnover, 'recorded human-release ledger must carry the ZY-366 turnover receipt')
  const barrierTicket = replayStatus.tickets.find(ticket => ticket.deployment?.queueId === frozen.queue_id)
  assert.ok(barrierTicket, 'replay status must carry the paused barrier ticket')
  assert.equal(barrierTicket.deployment.state, pausedLine.deployment.state)
  assert.equal(barrierTicket.deployment.sha, pausedLine.head)
  const unknownTicket = replayStatus.tickets.find(ticket => ticket.identifier === 'ZY-366')
  assert.ok(unknownTicket, 'replay status must carry the ZY-366 turnover ticket')
  assert.equal(unknownTicket.phase, '等人批')
  assert.equal(unknownTicket.since, turnover.createdAt)
  assert.ok(replayConfig.repos.some(repo => repo.path === barrierTicket.repo), 'replay config must list the dashboard repo')
  fs.writeFileSync(fake, `#!/usr/bin/env bun
const status = ${JSON.stringify(replayStatus)};
const config = ${JSON.stringify(replayConfig)};
if (process.argv[2] === 'config') console.log(JSON.stringify(config));
else console.log(JSON.stringify(status));
`)
  fs.chmodSync(fake, 0o755)
  const child = spawn(process.env.BUN_PATH || 'bun', [path.join(plugin, 'scene/server.ts')], { env: { ...process.env, PATH: `${temp}:${process.env.PATH}`, ZIIKOO_SCENE_TOKEN: token, ZIIKOO_SCENE_BIND: '127.0.0.1', ZIIKOO_SCENE_PORT: '0' }, stdio: ['ignore', 'pipe', 'pipe'] })
  try {
    const origin = await new Promise((resolve, reject) => {
      let output = ''
      const timer = setTimeout(() => reject(new Error('Bun fixture server did not listen')), 15000)
      child.on('error', reject)
      child.on('exit', code => { clearTimeout(timer); reject(new Error(`Bun fixture server exited ${code}`)) })
      child.stdout.on('data', bytes => {
        output += bytes
        const match = /listening (http:\/\/[^/]+)/.exec(output)
        if (match) { clearTimeout(timer); resolve(match[1]) }
      })
    })
    for (const resource of ['/', '/scene.js', '/town.mjs', '/draw.mjs', '/snapshot', '/events']) {
      assert.equal((await fetch(origin + resource)).status, 404)
      const authenticated = await fetch(`${origin}${resource}?t=${token}`)
      assert.ok(authenticated.status === 200 || (resource === '/snapshot' && authenticated.status === 503))
      await authenticated.body.cancel()
      const header = await fetch(origin + resource, { headers: { 'X-Token': token } })
      assert.ok(header.status === 200 || (resource === '/snapshot' && header.status === 503))
      await header.body.cancel()
      assert.equal((await fetch(`${origin}${resource}?t=${token}`, { method: 'POST' })).status, 404)
    }
    const snapshotResponse = await fetch(`${origin}/snapshot?t=${token}`)
    assert.equal(snapshotResponse.status, 200, 'the isolated driver status/config chain must produce a real Scene')
    const actualScene = await snapshotResponse.json()
    assert.equal(actualScene.state, replayStatus.state, 'the recorded status state must survive the parse chain')
    assert.equal(actualScene.maintenance, replayConfig.overview.maintenance, 'the recorded config maintenance must survive the parse chain')
    assert.equal(actualScene.rows.length, replayConfig.repos.length, 'the recorded config repo list must shape the rows')
    assert.equal(actualScene.rows[0].name, 'orca-factory')
    assert.equal(typeof actualScene.generatedAt, 'number')
    // The paused deployment barrier from the real ledger recording drives the
    // dispatch-station failed branch, and the untracked 等人批 phase drives
    // the unknown-station branch — both through the production
    // parseRuntime → parseConfig → sceneOf chain, not a hand-written Scene.
    const dashboard = actualScene.rows.find(row => row.path === barrierTicket.repo)
    const barrier = dashboard?.parcels.find(one => one.identifier === barrierTicket.identifier)
    assert.equal(barrier?.station, 'dispatch')
    assert.equal(barrier?.tone, 'failed')
    assert.equal(barrier?.label, '部署暂停')
    assert.equal(barrier?.deployment, 'paused')
    assert.equal(barrier?.blocker, 'deployment: paused')
    assert.equal(barrier?.reason, 'deployment: paused')
    const unmapped = dashboard?.parcels.find(one => one.identifier === unknownTicket.identifier)
    assert.equal(unmapped?.station, 'unknown', 'the recorded 等人批 phase must reach the unknown-station branch')
    assert.equal(unmapped?.tone, 'attention')
    assert.equal(unmapped?.label, '等人批')
    assert.equal((await fetch(`${origin}/vendor/OrbitControls.js?t=${token}`)).status, 404)
    console.log('PASS production Bun service: GET, header/query auth, all modules and retired routes')
    // The recorded chain must also drive the real page: load the production
    // page against this server and walk both exception branches in Chrome,
    // before the server goes away.
    const page = await browser.newPage({ viewport: { width: 1440, height: 960 } })
    const pageErrors = []
    page.on('pageerror', error => pageErrors.push(error.message))
    try {
      await page.goto(`${origin}/?t=${token}`)
      await page.waitForFunction(() => window.pixelTown?.state.scene)
      await page.locator('#search').fill('zhiyun')
      await page.locator('#projects button').first().click()
      await page.locator('#enter').click()
      await page.waitForFunction(() => window.pixelTown.state.view === 'workshop')
      await page.locator('#parcels button').first().click()
      const detail = await page.locator('#detail').innerText()
      assert.ok(detail.includes('ZY-365'), `the paused barrier ticket must open from the replayed scene (${detail.slice(0, 80)})`)
      assert.ok(detail.includes('部署暂停'), 'the page must surface the paused deployment state')
      assert.ok(detail.includes('deployment: paused'), 'the page must surface the ledger-derived blocked reason')
      await page.locator('#attention').click()
      await page.locator('#detail button').last().click()
      const unknownDetail = await page.locator('#detail').innerText()
      assert.ok(unknownDetail.includes('ZY-366'), `the turnover ticket must be reachable from the attention list (${unknownDetail.slice(0, 80)})`)
      assert.ok(unknownDetail.includes('未知阶段'), 'the 等人批 phase must drive the page unknown-stage branch')
      assert.ok(unknownDetail.includes('等人批'), 'the page must keep the original phase text next to the unknown station')
      assert.deepEqual(pageErrors, [])
      console.log('PASS production page in real Chrome: recorded barrier and unknown-phase branches')
    } finally {
      await page.close()
    }
  } finally {
    child.kill('SIGTERM')
    await new Promise(resolve => child.exitCode !== null || child.signalCode !== null ? resolve() : child.once('exit', resolve))
    fs.rmSync(temp, { recursive: true, force: true })
  }
}

main().catch(error => { console.error(error); process.exitCode = 1 })
