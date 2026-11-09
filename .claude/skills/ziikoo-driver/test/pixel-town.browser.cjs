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
  assert.deepEqual(model.pointAlong([{ x: 0, y: 0 }, { x: 34, y: 0 }, { x: 68, y: 0 }], 120 / 580, 100, 1), { x: 17, y: 0 }, 'short parked legs must honor their 240ms travel time')
  assert.deepEqual(model.pointAlong([{ x: 0, y: 0 }, { x: 135, y: 0 }, { x: 270, y: 0 }], 650 / 900, 300, 1), { x: 157.5, y: 0 }, 'dwell timing must use milliseconds throughout')
  for (const count of [0, 1, 2, 4, 6, 12, 24, 100, 1000, 1001]) {
    const scene = snapshot(count)
    const layout = model.townLayout(scene.rows)
    assert.equal(layout.projects.size, count)
    if (!count) continue
    const last = layout.projects.get(`/fixture/repo-${count - 1}`)
    assert.ok(last)
    assert.ok(model.visibleProjects(layout, { x: last.x - 150, y: last.y - 120, width: 300, height: 240 }).some(one => one.path === last.path))
    if (count === 1) assert.deepEqual([last.x, last.y], [520, 330])
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
  assert.deepEqual(route, [{x:880,y:285},{x:880,y:442},{x:400,y:442}])
  for (let index = 1; index < route.length; index++) assert.ok(route[index].x === route[index - 1].x || route[index].y === route[index - 1].y)
  assert.ok(model.travelPath(workshop, 'return', 'work').every(point=>point.x===400))
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
  assert.equal(anchored.camera.x, 290)
  anchored.selection = { kind: 'parcel', key: model.parcelKey('/fixture/repo-0', 'OF-1') }
  const reordered = structuredClone(anchored.scene)
  reordered.rows.reverse()
  anchored = model.reconcile(anchored, reordered)
  assert.equal(anchored.project, '/fixture/repo-0')
  assert.equal(anchored.camera.x, 760)
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

  // A dense refill (a packed hall compacting after departures) must fly every
  // drafted parcel to its new slot — no teleporting majority — and no two
  // painted boxes may intersect across the whole schedule. The sweep follows
  // the renderer's truth: an unplanned parcel paints at its new slot from
  // t=0, a planned one waits at the motion's first waypoint until it starts,
  // rides the path, then rests at the destination.
  const sweepSchedule = (state, layout, step) => {
    const end = Math.max(0, ...[...state.motions.values()].map(motion => motion.start + motion.duration))
    let samples = 0
    let min = Infinity
    for (let time = 0; time <= end; time += step) {
      samples++
      const points = [...layout.parcels].map(([key, point]) => {
        const motion = state.motions.get(key)
        if (!motion) return point
        if (time < motion.start) return motion.points[0]
        return model.pointAlong(motion.points, (time - motion.start) / motion.duration, motion.dwell ?? 0, motion.dwellAt ?? 0)
      })
      for (let index = 0; index < points.length; index++) for (const other of points.slice(index + 1)) min = Math.min(min, Math.max(Math.abs(points[index].x - other.x), Math.abs(points[index].y - other.y)))
    }
    assert.ok(samples > 0, `schedule sweep must sample at least once (end ${end})`)
    return min
  }
  const denseRefill = (count, leavers, step) => {
    const hall = snapshot(1)
    hall.rows[0].parcels = Array.from({ length: count }, (_, index) => parcel(`OF-${index}`))
    let hallState = model.reconcile(model.initialState(), hall, { fresh: true })
    const compacted = structuredClone(hall)
    for (let index = 0; index < leavers; index++) compacted.rows[0].parcels[index].station = 'gate'
    hallState = model.reconcile(hallState, compacted, { now: 0 })
    const layout = model.workshopLayout(compacted.rows[0])
    const flying = [...layout.parcels.keys()].filter(key => hallState.motions.has(key)).length
    assert.equal(flying, count, `every drafted parcel in a ${count}-parcel refill must fly, not teleport (${flying}/${count})`)
    for (const [key, destination] of layout.parcels) {
      const endpoint = hallState.motions.get(key).points.at(-1)
      assert.deepEqual({ x: endpoint.x, y: endpoint.y }, { x: destination.x, y: destination.y }, `${key} must finish at its assigned slot`)
    }
    return sweepSchedule(hallState, layout, step)
  }
  const dense26 = denseRefill(26, 2, 1)
  assert.ok(dense26 >= 26, `a 26-parcel refill must never intersect painted boxes (axis min ${dense26.toFixed(1)}px)`)
  const dense170 = denseRefill(170, 1, 8)
  assert.ok(dense170 >= 26, `a 170-parcel refill must never intersect painted boxes (axis min ${dense170.toFixed(1)}px)`)
  // The nine-parcel refill is the teleport hole itself: a dropped motion
  // snaps its parcel to its new slot at t=0 while the parcel queued there has
  // not departed yet, so the two boxes start stacked.
  const dense9 = denseRefill(9, 2, 1)
  assert.ok(dense9 >= 26, `a nine-parcel refill must not stack a teleported parcel on a waiting one (axis min ${dense9.toFixed(1)}px)`)

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
  const upperLane = soloLayout.nodes.get('work').y
  const lowerLane = packLayout.nodes.get('pack').y
  const beltPoints = ['pickup', 'work', 'gate', 'review', 'pack', 'merge', 'dispatch', 'smoke'].map(station => {
    const node = soloLayout.nodes.get(station)
    return { x: node.x, y: node.y }
  })
  const soloSlot = soloLayout.parcels.get(model.parcelKey('/fixture/repo-0', 'OF-solo'))
  const slotOffset = Math.hypot(soloSlot.x - beltPoints[1].x, soloSlot.y - beltPoints[1].y)
  assert.ok(Math.abs(soloSlot.x - beltPoints[1].x) < 26 && Math.abs(soloSlot.y - beltPoints[1].y) < 26, 'fixture slot must sit within a parcel width of its belt lane')
  for (const [index, point] of soloMotion.points.entries()) {
    if (!index) continue
    const previous = soloMotion.points[index - 1]
    if (previous.y === point.y) assert.ok([upperLane, lowerLane].includes(point.y), 'horizontal tween segments must ride the drawn belt lanes')
    else assert.equal(point.x, 948, 'vertical tween segments must ride the approved outer belt corner')
  }
  const beltHalf = 12
  const stray = soloMotion.points.filter(point => !(Math.abs(point.x-948)<=beltHalf || beltPoints.some(belt => Math.abs(point.y - belt.y) <= beltHalf)))
  assert.ok(stray.length === 0, `every tween waypoint must sit on the painted belt polyline (worst ${stray[0] ? Math.round(stray[0].x) + ',' + Math.round(stray[0].y) : ''})`)
  const stageMove = (from, to) => {
    const original = snapshot(1)
    const scene = { ...original, rows: [{ ...original.rows[0], parcels: [parcel('OF-belt', from)] }] }
    const before = model.reconcile(model.initialState(), scene, { now: 0, fresh: true })
    const next = { ...scene, rows: [{ ...scene.rows[0], parcels: [parcel('OF-belt', to)] }] }
    const moving = model.reconcile(before, next, { now: 0 })
    return moving.motions.get(model.parcelKey('/fixture/repo-0', 'OF-belt'))
  }
  for (const [from, to, corner] of [['return', 'work', 400], ['work', 'smoke', 948], ['smoke', 'work', 948], ['repair', 'work', 400], ['work', 'human', 400]]) {
    const motion = stageMove(from, to)
    assert.ok(motion, `${from}->${to} must schedule a belt motion`)
    for (let index = 1; index < motion.points.length; index++) {
      assert.ok(motion.points[index].x === motion.points[index - 1].x || motion.points[index].y === motion.points[index - 1].y, `${from}->${to} must follow orthogonal belt segments (segment ${index})`)
    }
    assert.ok(motion.points.some(point => point.x === corner), `${from}->${to} must round the visible belt corner x=${corner}, not a cross-workshop shortcut`)
  }
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
    if (!['index.html', 'scene.js', 'town.mjs', ...fs.readdirSync(path.join(pageRoot,'components')).map(name=>'components/'+name)].includes(name)) { response.writeHead(404).end(); return }
    response.setHeader('Content-Type', name.endsWith('.html') ? 'text/html' : name.endsWith('.css') ? 'text/css' : 'text/javascript')
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
      if (!count) { await assertVisible(page.locator('.empty-state').first()); assert.equal(await page.locator('[data-view="project"]').isDisabled(), true); continue }
      await page.locator('#search').fill(`/fixture/repo-${count - 1}`)
      await page.locator(`#search-results button[data-project="/fixture/repo-${count - 1}"]`).click()
      assert.equal(await page.evaluate(() => window.pixelTown.state.project), `/fixture/repo-${count - 1}`, `last project reachable at count ${count}; page errors: ${errors.join(', ')}`)
      await page.locator('[data-view="project"]').click()
      await page.waitForFunction(() => window.pixelTown.state.view === 'workshop')
      await page.locator('#map-labels button[data-ticket]').first().click()
      assert.ok((await page.locator('#detail').innerText()).includes('长中文标题'.repeat(12)))
      await page.locator('[data-view="town"]').click()
      const position = await page.evaluate(() => ({ camera: window.pixelTown.state.camera, project: window.pixelTown.layout.projects.get(window.pixelTown.state.project) }))
      const halfHeight = (await page.locator('#world').boundingBox()).height / (2 * position.camera.zoom)
      const townHeight = await page.evaluate(() => window.pixelTown.layout.bounds.height)
      const focusY = halfHeight * 2 >= townHeight ? townHeight / 2 : Math.max(halfHeight, Math.min(townHeight - halfHeight, position.project.y + 40))
      assert.equal(position.camera.x, 520, 'returning to town on desktop must center the town axis')
      assert.ok(Math.abs(position.camera.y - focusY) < 1e-6, `returning to town must focus the selected courtyard (${position.camera.y} vs ${focusY})`)
      await page.locator('#search').fill('')
      if (count === 6 && process.env.PIXEL_TOWN_SCREENSHOT) await page.screenshot({ path: process.env.PIXEL_TOWN_SCREENSHOT })
    }
    await emit(snapshot(1))
    const building = await page.evaluate(() => window.pixelTown.selectedScreenPoint())
    await page.mouse.click(building.x, building.y)
    assert.equal(await page.evaluate(() => window.pixelTown.state.view), 'workshop', 'building hit area shares the derived layout')
    assert.equal(await page.locator('#map-labels [data-station]').count(), 11, 'unknown phases must not add a twelfth workstation')
    for (const station of Object.keys(model.stations).filter(station=>station!=='unknown')) {
      await page.locator(`#map-labels button[data-station="${station}"]`).click()
      assert.ok((await page.locator('#detail').innerText()).includes(model.stations[station][1]))
    }
    if (process.env.PIXEL_TOWN_SCREENSHOT) {
      await page.locator('#detail button').first().click()
      await page.locator('#zoom-out').click()
      await page.locator('#fit-map').click()
      await page.screenshot({ path: process.env.PIXEL_TOWN_SCREENSHOT.replace(/\.png$/, '-workshop.png') })
    }
    await page.locator('[data-view="town"]').click()
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
    await page.locator('#repo-list button').last().click()
    await page.locator('[data-view="project"]').click()
    await page.locator('#map-labels button[data-ticket]').first().click()
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
    await page.locator('#map-labels button[data-ticket]').first().click()
    const mobile = await page.evaluate(() => {
      const point = window.pixelTown.selectedScreenPoint()
      const canvas = document.querySelector('#world').getBoundingClientRect()
      const panel = document.querySelector('#detail-panel')
      const detail = panel.getBoundingClientRect()
      return { point, canvas: { left: canvas.left, top: canvas.top, right: canvas.right, bottom: canvas.bottom }, detail: { top: detail.top, bottom: detail.bottom, drawer: panel.classList.contains('is-drawer') && getComputedStyle(panel).position === 'fixed' } }
    })
    assert.ok(mobile.point.x >= mobile.canvas.left && mobile.point.x <= mobile.canvas.right)
    assert.ok(mobile.point.y >= mobile.canvas.top && mobile.point.y <= mobile.canvas.bottom)
    assert.ok(mobile.detail.drawer && mobile.detail.bottom <= 844 + 1, 'the approved demo opens mobile details as a bottom drawer over the map')
    assert.ok(mobile.point.y + 20 <= mobile.detail.top, 'the selected parcel must stay visible above the mobile detail drawer')
    if (process.env.PIXEL_TOWN_SCREENSHOT) await page.screenshot({ path: process.env.PIXEL_TOWN_SCREENSHOT.replace(/\.png$/, '-mobile.png') })
    await assertVisible(page.locator('#status'))
    await page.locator('[data-close]').click()
    await page.locator('[data-view="town"]').click()
    await emit(snapshot(1))
    assert.equal(await page.evaluate(() => window.pixelTown.state.project), '/fixture/repo-0')
    assert.equal(await page.evaluate(() => window.pixelTown.state.selection), null)
    assert.ok(!new URL(page.url()).searchParams.has('parcel'))
    const exceptional = snapshot(1)
    exceptional.state = 'stale'
    exceptional.maintenance = 'paused'
    exceptional.rows[0].parcels = [{ ...parcel('OF-unknown', 'alien'), unknownDeploy: true, deployment: 'paused', reason: '部署凭据错误', tone: 'failed' }]
    await emit(exceptional)
    await page.locator('[data-view="project"]').click()
    await page.locator('#map-labels button[data-ticket]').click()
    assert.ok((await page.locator('#detail').innerText()).includes('未知阶段'))
    assert.ok((await page.locator('#detail').innerText()).includes('paused'))
    assert.ok((await page.locator('#status').innerText()).includes('过期'))
    assert.ok((await page.locator('#status').innerText()).includes('paused'))
    await page.locator('[data-close]').click()
    await page.locator('[data-view="attention"]').click()
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
    // BFCache 恢复时不重跑模块，关闭的流与旧连接标记会随页面恢复。
    // 两轮受控事件用于锁定离开、恢复及重新取快照的处理行为。
    // Playwright 默认禁用 BFCache，这里直接派发 persisted 事件。
    // 该回归验证事件处理器，不证明浏览器实际命中 BFCache。
    // 实际命中的证据需要浏览器自身触发 pageshow.persisted=true。
    for (const count of [2, 4]) {
      const priorStreams = requests.filter(request => request.pathname === '/events').length
      await page.evaluate(() => window.dispatchEvent(new PageTransitionEvent('pagehide', { persisted: true })))
      assert.ok((await page.locator('#status').innerText()).includes('断开'), 'pagehide must stop reporting a live connection')
      scene = snapshot(count)
      await page.evaluate(() => window.dispatchEvent(new PageTransitionEvent('pageshow', { persisted: true })))
      await page.waitForFunction(count => window.pixelTown.state.scene?.rows.length === count, count, { timeout: 5000 })
      assert.ok(requests.filter(request => request.pathname === '/events').length > priorStreams, 'pageshow must create a new authenticated stream')
      assert.equal(await page.evaluate(() => window.pixelTown.state.motions.size), 0, 'restoration must snap to current state')
    }
    await page.locator('[data-view="town"]').click()
    const undeployed = snapshot(1)
    undeployed.rows[0].deploy = false
    undeployed.rows[0].parcels[0].unknownDeploy = true
    await emit(undeployed)
    await page.locator('[data-view="project"]').click()
    await page.locator('#map-labels button[data-ticket]').click()
    assert.ok((await page.locator('#detail').innerText()).includes('不适用 · 未配置部署'))
    assert.ok((await page.locator('#detail').innerText()).includes('部署字段：未知'))
    await page.keyboard.press('Escape')
    await assertVisible(page.locator('#status'))
    await page.setViewportSize({ width: 1440, height: 1000 })
    await page.locator('[data-view="town"]').click()
    const reviewed = snapshot(1)
    reviewed.rows[0].parcels = [
      ...Array.from({ length: 90 }, (_, index) => parcel(`OF-crowd-${index}`)),
      { ...parcel('OF-xss', 'alien'), phase: '<img src=x onerror="window.__xss=1">', title: 'A & B' },
      { ...parcel('OF-nodeploy', 'smoke'), unknownDeploy: true },
      { ...parcel('OF-shipped', 'smoke'), tone: 'done', deployment: 'passed' },
    ]
    await emit(reviewed)
    assert.equal(await page.locator('#attention-count').innerText(), '2', 'attention must keep the real Scene rule: unknown stage and unknown deployment count, a passed deployment does not')
    await page.locator('#search').fill('OF-xss')
    assert.equal(await page.locator('#search-results img').count(), 0, 'a Scene phase must render as text in search results')
    await page.locator('#search-results button[data-ticket]').click()
    assert.equal(await page.locator('#detail img').count(), 0, 'a Scene phase must render as text in details')
    await page.evaluate(() => Object.defineProperty(navigator.clipboard, 'writeText', { configurable: true, value: async text => { window.copiedDetail = text } }))
    await page.locator('[data-copy="detail"]').click()
    const copied = await page.waitForFunction(() => window.copiedDetail).then(handle => handle.jsonValue())
    assert.ok(copied.includes('A & B') && copied.includes('<img src=x') && !copied.includes('演示'), `copied detail must carry the real Scene text: ${copied}`)
    assert.equal(await page.evaluate(() => window.__xss), undefined)
    await page.locator('#search').fill('OF-crowd-89')
    await page.locator('#search-results button[data-ticket]').click()
    await page.locator('#world').focus()
    for (let index = 0; index < 30; index++) await page.keyboard.press('ArrowDown')
    const reach = await page.evaluate(() => ({ point: window.pixelTown.selectedScreenPoint(), canvas: document.querySelector('#world').getBoundingClientRect().toJSON() }))
    assert.ok(reach.point.y >= reach.canvas.top && reach.point.y <= reach.canvas.bottom, `the last crowded parcel must be reachable by keyboard panning (${reach.point.y} outside ${reach.canvas.top}-${reach.canvas.bottom})`)
    const zoomText = await page.locator('#zoom-text').innerText()
    await page.keyboard.press('+')
    assert.notEqual(await page.locator('#zoom-text').innerText(), zoomText, 'the focused map must zoom with +')
    await page.locator('[data-close]').click()
    await page.locator('[data-view="attention"]').click()
    reviewed.rows[0].parcels[0].duration = '3 分钟'
    await emit(reviewed)
    assert.ok(await page.locator('[data-view="attention"]').evaluate(button => button.classList.contains('active')), 'a routine SSE update must keep the attention view')
    await page.locator('[data-view="town"]').focus()
    await page.keyboard.press('Enter')
    assert.equal(await page.evaluate(() => window.pixelTown.state.view), 'town')
    assert.equal(await page.evaluate(() => localStorage.length + sessionStorage.length), 0)
    assert.ok(requests.filter(one => one.pathname.endsWith('.mjs')).length >= 2)
    assert.ok(requests.every(one => one.authorized), 'all resource and event requests must keep authentication')
    assert.deepEqual(errors, [])
    console.log('PASS real Chrome: counts, search, identity, camera, viewport caches, mobile, accessibility, SSE and errors')
  } catch (error) {
    await browser.close()
    throw error
  } finally {
    for (const response of streams) response.end()
    await new Promise(resolve => server.close(resolve))
  }
  try {
    await checkProductionServer(browser)
  } finally {
    await browser.close()
  }
}

async function assertVisible(locator) { assert.equal(await locator.isVisible(), true) }

// listening 先于首轮 status/config 读取完成，不能作为快照就绪信号。
// 启动后的初始 503 允许等待，其他状态立即失败。
// 截止时间内仍未产生首个 Scene 时，测试必须失败。
// 该等待只处理测试启动时序，不改变生产服务契约。
async function waitForSnapshot(origin, token, timeout = 15000) {
  const deadline = Date.now() + timeout
  for (;;) {
    const response = await fetch(`${origin}/snapshot?t=${token}`)
    if (response.status === 200) return response
    if (response.status !== 503) assert.fail(`snapshot returned ${response.status}`)
    if (Date.now() > deadline) assert.fail('snapshot stayed 503 past the deadline')
    await new Promise(resolve => setTimeout(resolve, 250))
  }
}

async function checkProductionServer(browser) {
  const tempRoot = path.join(process.cwd(), '.omc/tmp')
  fs.mkdirSync(tempRoot, { recursive: true })
  const temp = fs.mkdtempSync(path.join(tempRoot, 'pixel-town-'))
  const token = crypto.randomBytes(24).toString('hex')
  const fake = path.join(temp, 'orca-factory-driver')
  // 原始录制链与派生异常夹具分开运行，避免将测试构造误称为生产录制。
  // 原始 status --json 与 config --json 文件位于插件 test/recorded/：
  // live-status.json 与 live-config.json，录制于 2026-11-06。
  // status 只有处于 Agent处置 的 OF-415 与 OF-419，没有下面的异常票。
  // 异常夹具在同一 status 基底上追加两张票，并保留原始文件不变。
  // ZY-365 的 paused 部署字段取自真实 merge-progress-v1 账本录制。
  // ZY-366 的 since 取自真实转人收据，阶段另选历史字面值“等人批”。
  // “等人批”不是从这份 CLI 或转人收据读取的阶段，不是异常原始录制。
  // 两条链都经过 scene/server.ts 的 parseRuntime、parseConfig、sceneOf。
  // 派生夹具用于验证部署暂停与未知阶段的页面分支。
  const liveStatusText = fs.readFileSync(path.join(plugin, 'test/recorded/live-status.json'), 'utf8')
  const liveConfigText = fs.readFileSync(path.join(plugin, 'test/recorded/live-config.json'), 'utf8')
  const liveStatus = JSON.parse(liveStatusText)
  const liveConfig = JSON.parse(liveConfigText)
  for (const ticket of liveStatus.tickets) {
    assert.equal(ticket.phase, 'Agent处置', 'the live recording must be the raw CLI capture, not a derived fixture')
    assert.equal(ticket.deployment, undefined, 'the live recording must not carry derived deployment fields')
  }
  const ledger = JSON.parse(fs.readFileSync(path.resolve(plugin, '../../../driver/testdata/recorded/ledger-comments-deployment-retry-workflow.json'), 'utf8'))
  const progressLines = ledger.comments.flatMap(comment => comment.body.split('\n')).filter(line => line.startsWith('merge-progress-v1: '))
  const pausedLine = progressLines.map(line => JSON.parse(line.slice('merge-progress-v1: '.length))).find(progress => progress.deployment?.state === 'paused')
  assert.ok(pausedLine, 'recorded ledger must contain a paused merge-progress record')
  const handoffLine = ledger.comments.flatMap(comment => comment.body.split('\n')).find(line => line.startsWith('merge-handoff-v1: '))
  const frozen = JSON.parse(handoffLine.slice('merge-handoff-v1: '.length))
  const humanLedger = JSON.parse(fs.readFileSync(path.resolve(plugin, '../../../driver/testdata/recorded/ledger-comments-human-release.json'), 'utf8'))
  const turnover = humanLedger.comments.filter(comment => comment.body.startsWith('转人: ZY-366')).at(-1)
  assert.ok(turnover, 'recorded human-release ledger must carry the ZY-366 turnover receipt')
  const replayStatus = { ...liveStatus, tickets: [...liveStatus.tickets, {
    repo: '/home/laoxu/github/zhiyun-ai-dashboard',
    identifier: 'ZY-365',
    owner: '处置Agent',
    phase: '已合并',
    backend: 'feishu',
    queuePosition: 1,
    blockedReason: `deployment: ${pausedLine.deployment.state}`,
    since: turnover.createdAt,
    pushedAt: turnover.createdAt,
    url: 'https://github.com/vmxmy/zhiyun-ai-dashboard/pull/2143',
    title: 'fix(dashboard): 修复图表在空数据下的崩溃',
    deployment: { queueId: pausedLine.queue_id, sha: pausedLine.head, workflow: pausedLine.deployment.workflow, state: pausedLine.deployment.state, reason: pausedLine.deployment.reason },
  }, {
    repo: '/home/laoxu/github/zhiyun-ai-dashboard',
    identifier: 'ZY-366',
    owner: '人',
    phase: '等人批',
    backend: 'feishu',
    since: turnover.createdAt,
    pushedAt: turnover.createdAt,
    url: 'https://qcnqhljeakpl.feishu.cn/base/VeQObWd9VaV66tsiM7GcN0Avnph?table=tblmC0IWUj4QJ3b6&record=reczz28VPL7ISrNs',
    title: 'chore(content): 退役 Workers AI 嵌入通道与语义检索路由',
  }] }
  const barrierTicket = replayStatus.tickets.find(ticket => ticket.deployment?.queueId === frozen.queue_id)
  assert.ok(barrierTicket, 'the derived replay status must carry the paused barrier ticket')
  assert.equal(barrierTicket.deployment.state, pausedLine.deployment.state)
  assert.equal(barrierTicket.deployment.sha, pausedLine.head)
  assert.equal(barrierTicket.since, turnover.createdAt)
  assert.equal(barrierTicket.blockedReason, `deployment: ${pausedLine.deployment.state}`)
  const unknownTicket = replayStatus.tickets.find(ticket => ticket.identifier === 'ZY-366')
  assert.ok(unknownTicket, 'the derived replay status must carry the ZY-366 turnover ticket')
  assert.equal(unknownTicket.phase, '等人批')
  assert.equal(unknownTicket.since, turnover.createdAt)
  assert.ok(liveConfig.repos.some(repo => repo.path === barrierTicket.repo), 'the recorded config must list the dashboard repo')
  assert.ok(!liveStatus.tickets.some(ticket => ticket.identifier === 'ZY-365' || ticket.identifier === 'ZY-366'), 'the live recording must not already contain the derived tickets')
  const fakeStatus = status => {
    fs.writeFileSync(fake, `#!/usr/bin/env bun
const status = JSON.parse(JSON.stringify(${JSON.stringify(status)}));
const config = JSON.parse(${JSON.stringify(liveConfigText)});
if (process.argv[2] === 'config') console.log(JSON.stringify(config));
else console.log(JSON.stringify(status));
`)
    fs.chmodSync(fake, 0o755)
  }
  fakeStatus(liveStatus)
  const spawnServer = () => spawn(process.env.BUN_PATH || 'bun', [path.join(plugin, 'scene/server.ts')], { env: { ...process.env, PATH: `${temp}:${process.env.PATH}`, ZIIKOO_SCENE_TOKEN: token, ZIIKOO_SCENE_BIND: '127.0.0.1', ZIIKOO_SCENE_PORT: '0' }, stdio: ['ignore', 'pipe', 'pipe'] })
  const child = spawnServer()
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
    for (const resource of ['/', '/scene.js', '/town.mjs', ...fs.readdirSync(path.join(pageRoot,'components')).map(name=>'/components/'+name), '/snapshot', '/events']) {
      assert.equal((await fetch(origin + resource)).status, 404)
      const authenticated = await fetch(`${origin}${resource}?t=${token}`)
      assert.ok(authenticated.status === 200 || (resource === '/snapshot' && authenticated.status === 503))
      await authenticated.body.cancel()
      const header = await fetch(origin + resource, { headers: { 'X-Token': token } })
      assert.ok(header.status === 200 || (resource === '/snapshot' && header.status === 503))
      await header.body.cancel()
      assert.equal((await fetch(`${origin}${resource}?t=${token}`, { method: 'POST' })).status, 404)
    }
    const snapshotResponse = await waitForSnapshot(origin, token)
    const actualScene = await snapshotResponse.json()
    assert.equal(actualScene.state, liveStatus.state, 'the live recorded status state must survive the parse chain')
    assert.equal(actualScene.maintenance, liveConfig.overview.maintenance, 'the recorded config maintenance must survive the parse chain')
    assert.equal(actualScene.rows.length, liveConfig.repos.length, 'the recorded config repo list must shape the rows')
    assert.equal(actualScene.rows[0].name, 'orca-factory')
    assert.equal(typeof actualScene.generatedAt, 'number')
    // 原始录制只有处于 Agent处置 的 OF-415 与 OF-419。
    // 这两张票应落在修理架，原始链不能混入派生的异常票。
    // 部署暂停与未知阶段由后面的独立派生夹具覆盖。
    const factoryRow = actualScene.rows.find(row => row.name === 'orca-factory')
    const liveParcels = factoryRow?.parcels.map(one => one.identifier)
    assert.deepEqual(liveParcels, ['OF-415', 'OF-419'], 'the live recording must survive the parse chain as recorded')
    for (const one of factoryRow?.parcels ?? []) {
      assert.equal(one.station, 'repair', `the recorded Agent处置 phase must drive the repair rack (${one.identifier})`)
      assert.equal(one.tone, 'attention')
      assert.equal(one.deployment, undefined)
      assert.equal(one.unknownDeploy, false)
    }
    assert.equal((await fetch(`${origin}/vendor/OrbitControls.js?t=${token}`)).status, 404)
    console.log('PASS production Bun service on raw live recordings: GET, header/query auth, all modules and retired routes')
    await driveLivePage(browser, origin, token)
  } finally {
    child.kill('SIGTERM')
    await new Promise(resolve => child.exitCode !== null || child.signalCode !== null ? resolve() : child.once('exit', resolve))
  }
  fakeStatus(replayStatus)
  const exceptionChild = spawnServer()
  try {
    const exceptionOrigin = await new Promise((resolve, reject) => {
      let output = ''
      const timer = setTimeout(() => reject(new Error('Bun fixture server did not listen')), 15000)
      exceptionChild.on('error', reject)
      exceptionChild.on('exit', code => { clearTimeout(timer); reject(new Error(`Bun fixture server exited ${code}`)) })
      exceptionChild.stdout.on('data', bytes => {
        output += bytes
        const match = /listening (http:\/\/[^/]+)/.exec(output)
        if (match) { clearTimeout(timer); resolve(match[1]) }
      })
    })
    const snapshotResponse = await waitForSnapshot(exceptionOrigin, token)
    const actualScene = await snapshotResponse.json()
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
    assert.equal(unmapped?.station, 'unknown', 'the 等人批 phase must reach the unknown-station branch')
    assert.equal(unmapped?.tone, 'attention')
    assert.equal(unmapped?.label, '等人批')
    console.log('PASS production Bun service on the derived exception fixture')
    await driveExceptionPage(browser, exceptionOrigin, token, barrierTicket, unknownTicket)
  } finally {
    exceptionChild.kill('SIGTERM')
    await new Promise(resolve => exceptionChild.exitCode !== null || exceptionChild.signalCode !== null ? resolve() : exceptionChild.once('exit', resolve))
    fs.rmSync(temp, { recursive: true, force: true })
  }
}

async function driveLivePage(browser, origin, token) {
  const page = await browser.newPage({ viewport: { width: 1440, height: 960 } })
  const pageErrors = []
  page.on('pageerror', error => pageErrors.push(error.message))
  try {
    await page.goto(`${origin}/?t=${token}`)
    await page.waitForFunction(() => window.pixelTown?.state.scene)
    await page.locator('#search').fill('orca-factory')
    await page.locator('#repo-list button').first().click()
    await page.locator('[data-view="project"]').click()
    await page.waitForFunction(() => window.pixelTown.state.view === 'workshop')
    await page.locator('#map-labels button[data-ticket]').first().click()
    const detail = await page.locator('#detail').innerText()
    assert.ok(detail.includes('OF-415'), `the recorded live ticket must open from the replayed scene (${detail.slice(0, 80)})`)
    assert.ok(detail.includes('修理'), 'the recorded Agent处置 phase must drive the page repair-station branch')
    assert.ok(detail.includes('二线处理中'), 'the page must keep the recorded label')
    assert.deepEqual(pageErrors, [])
    console.log('PASS production page in real Chrome on the raw live recording')
  } finally {
    await page.close()
  }
}

async function driveExceptionPage(browser, origin, token, barrierTicket, unknownTicket) {
  const page = await browser.newPage({ viewport: { width: 1440, height: 960 } })
  const pageErrors = []
  page.on('pageerror', error => pageErrors.push(error.message))
  try {
    await page.goto(`${origin}/?t=${token}`)
    await page.waitForFunction(() => window.pixelTown?.state.scene)
    await page.locator('#search').fill('zhiyun')
    await page.locator('#search-results button[data-project]').first().click()
    await page.locator('[data-view="project"]').click()
    await page.waitForFunction(() => window.pixelTown.state.view === 'workshop')
    await page.locator('#map-labels button[data-ticket]').first().click()
    const detail = await page.locator('#detail').innerText()
    assert.ok(detail.includes(barrierTicket.identifier), `the paused barrier ticket must open from the derived scene (${detail.slice(0, 80)})`)
    assert.ok(detail.includes('部署暂停'), 'the page must surface the paused deployment state')
    assert.ok(detail.includes('deployment: paused'), 'the page must surface the ledger-derived blocked reason')
    await page.locator('[data-view="attention"]').click()
    await page.locator('#detail button').last().click()
    const unknownDetail = await page.locator('#detail').innerText()
    assert.ok(unknownDetail.includes(unknownTicket.identifier), `the turnover ticket must be reachable from the attention list (${unknownDetail.slice(0, 80)})`)
    assert.ok(unknownDetail.includes('未知阶段'), 'the 等人批 phase must drive the page unknown-stage branch')
    assert.ok(unknownDetail.includes('等人批'), 'the page must keep the original phase text next to the unknown station')
    assert.deepEqual(pageErrors, [])
    console.log('PASS production page in real Chrome: derived barrier and unknown-phase branches')
  } finally {
    await page.close()
  }
}

main().catch(error => { console.error(error); process.exitCode = 1 })
