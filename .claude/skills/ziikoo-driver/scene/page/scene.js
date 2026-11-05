import * as THREE from 'three'
import { OrbitControls } from './vendor/OrbitControls.js'

const colors = { done: 0x50c98b, current: 0x5d9fff, attention: 0xf6c75c, failed: 0xed676b, idle: 0x596879 }
const positions = { pickup: 0, work: 6, gate: 12, review: 17, pack: 22, merge: 27, dispatch: 34, smoke: 40, repair: 12, human: 22, return: 9, unknown: 0 }
const labels = { pickup: '领取台', work: '施工台', gate: '门禁', review: '审查台', pack: '打包台', merge: '合并装车道', dispatch: '出厂门', smoke: '目的地检查站' }
const world = new THREE.Scene()
world.background = new THREE.Color(0x101923)
world.fog = new THREE.Fog(0x101923, 65, 170)
const camera = new THREE.PerspectiveCamera(45, innerWidth / innerHeight, 0.1, 300)
camera.position.set(43, 35, 46)
const renderer = new THREE.WebGLRenderer({ antialias: true })
renderer.setPixelRatio(Math.min(devicePixelRatio, 2))
renderer.setSize(innerWidth, innerHeight)
renderer.shadowMap.enabled = true
renderer.outputColorSpace = THREE.SRGBColorSpace
renderer.domElement.setAttribute('aria-label', 'Driver 流水线三维车间，箱子代表票据')
document.body.append(renderer.domElement)
const controls = new OrbitControls(camera, renderer.domElement)
controls.enableDamping = true
controls.maxPolarAngle = Math.PI / 2.1
controls.maxDistance = 150
controls.target.set(18, 0, 5)
world.add(new THREE.HemisphereLight(0xcce5ff, 0x17222f, 2.4))
const sun = new THREE.DirectionalLight(0xffffff, 3)
sun.position.set(10, 35, 12)
sun.castShadow = true
sun.shadow.mapSize.set(2048, 2048)
sun.shadow.camera.left = -50
sun.shadow.camera.right = 65
sun.shadow.camera.top = 65
sun.shadow.camera.bottom = -50
world.add(sun)
const topLight = new THREE.PointLight(colors.attention, 35, 90)
topLight.position.set(20, 12, 10)
world.add(topLight)
const floor = new THREE.Mesh(new THREE.PlaneGeometry(230, 230), new THREE.MeshStandardMaterial({ color: 0x202f3e, roughness: 0.95 }))
floor.rotation.x = -Math.PI / 2
floor.position.y = -0.25
floor.receiveShadow = true
world.add(floor)
world.add(new THREE.GridHelper(180, 90, 0x3d5266, 0x2a3c4e))

const fixtures = new THREE.Group()
world.add(fixtures)
const parcels = new Map()
let machines = []
let trucks = []
let lamps = []
let doors = []
let layoutKey = ''
let snapshot
let connected = false
const failures = new Map()
const status = document.querySelector('#status')
const snapshotTime = document.querySelector('#snapshot')
const tooltip = document.querySelector('#tooltip')

function block(parent, width, height, depth, color, positionX = 0, positionY = 0, rowZ = 0) {
  const mesh = new THREE.Mesh(new THREE.BoxGeometry(width, height, depth), new THREE.MeshStandardMaterial({ color, roughness: 0.65 }))
  mesh.position.set(positionX, positionY, rowZ)
  mesh.castShadow = true
  mesh.receiveShadow = true
  parent.add(mesh)
  return mesh
}

function label(parent, text, positionX, positionY, rowZ, width = 3.5, color = '#e5edf6') {
  const canvas = document.createElement('canvas')
  canvas.width = 768
  canvas.height = 160
  const context = canvas.getContext('2d')
  context.fillStyle = '#142231'
  context.fillRect(0, 0, canvas.width, canvas.height)
  context.fillStyle = color
  context.font = 'bold 64px system-ui, sans-serif'
  context.textAlign = 'center'
  context.textBaseline = 'middle'
  context.fillText(text, 384, 80, 740)
  const texture = new THREE.CanvasTexture(canvas)
  texture.colorSpace = THREE.SRGBColorSpace
  const sprite = new THREE.Sprite(new THREE.SpriteMaterial({ map: texture }))
  sprite.position.set(positionX, positionY, rowZ)
  sprite.scale.set(width, width * 160 / 768, 1)
  parent.add(sprite)
  return sprite
}

function dispose(group) {
  group.traverse(object => {
    object.geometry?.dispose()
    const materials = Array.isArray(object.material) ? object.material : object.material ? [object.material] : []
    for (const material of materials) {
      material.map?.dispose()
      material.dispose()
    }
  })
  group.removeFromParent()
}

function belt(positionX, rowZ, width, depth = 1.7, height = 0.35) {
  block(fixtures, width, 0.3, depth, 0x394b5b, positionX + width / 2, height, rowZ)
  for (let offset = 0; offset < width; offset += 0.65) block(fixtures, 0.12, 0.03, depth, 0x607387, positionX + offset, height + 0.17, rowZ)
}

function wheel(parent, positionX, rowZ) {
  const mesh = new THREE.Mesh(new THREE.CylinderGeometry(0.28, 0.28, 0.19, 12), new THREE.MeshStandardMaterial({ color: 0x14202c }))
  mesh.rotation.x = Math.PI / 2
  mesh.position.set(positionX, 0.25, rowZ)
  parent.add(mesh)
}

function forklift(rowZ) {
  const group = new THREE.Group()
  group.position.set(28.5, 0, rowZ - 2.4)
  block(group, 1.5, 0.6, 0.9, colors.attention, 0, 0.7)
  block(group, 0.65, 0.7, 0.75, 0x344c60, 0.2, 1.2)
  block(group, 0.12, 1.8, 0.12, 0x98aec0, -0.9, 1.1, -0.4)
  block(group, 0.12, 1.8, 0.12, 0x98aec0, -0.9, 1.1, 0.4)
  block(group, 1.2, 0.1, 0.15, 0xc4d2de, -1.3, 0.35, -0.35)
  block(group, 1.2, 0.1, 0.15, 0xc4d2de, -1.3, 0.35, 0.35)
  for (const positionX of [-0.55, 0.55]) for (const side of [-0.55, 0.55]) wheel(group, positionX, side)
  fixtures.add(group)
}

function truck(rowZ, parcel) {
  const group = new THREE.Group()
  block(group, 2.8, 1.6, 1.3, 0xb5c6d5, 0, 1.3)
  block(group, 1, 1.1, 1.3, colors.current, 1.9, 1)
  block(group, 0.55, 0.4, 1.32, 0x233b50, 2, 1.5)
  for (const positionX of [-0.9, 0.9, 2]) for (const side of [-0.75, 0.75]) wheel(group, positionX, side)
  const beacon = block(group, 0.25, 0.2, 0.25, colors.attention, 1.9, 1.7)
  const paused = parcel.deployment === 'paused'
  group.position.set(paused ? 32 : positions[parcel.station], 0, rowZ)
  if (paused) {
    block(group, 0.2, 1.6, 0.2, colors.failed, 3.2, 0.8, -1)
    block(group, 0.15, 0.16, 2.2, colors.failed, 3.2, 1.6)
  }
  label(group, parcel.identifier, 0, 2.5, 0, 2.5)
  fixtures.add(group)
  trucks.push({ group, beacon, paused })
}

function makeRow(row, rowZ) {
  label(fixtures, row.name, -4, 2, rowZ, 5)
  belt(0, rowZ, row.deploy ? 40 : 28)
  belt(5, rowZ + 3, 12, 1.2, -0.05)
  label(fixtures, 'correction 回流带', 9, 0.65, rowZ + 3, 4, '#f6c75c')
  for (const [station, title] of Object.entries(labels)) {
    if (!row.deploy && (station === 'dispatch' || station === 'smoke')) continue
    const positionX = positions[station]
    label(fixtures, station === 'work' ? `${title} ×${row.workers}` : title, positionX, 3.7, rowZ, 3.6)
    const lamp = block(fixtures, 0.3, 0.15, 0.3, colors.idle, positionX, 3.1, rowZ)
    lamps.push({ lamp, row: row.path, station })
  }
  block(fixtures, 2.3, 0.7, 2, 0x445c70, 0, 0.75, rowZ)
  for (let slot = 0; slot < row.workers; slot++) {
    const offset = (slot - (row.workers - 1) / 2) * 1.8
    block(fixtures, 2.5, 0.7, 1.5, 0x445c70, 6, 0.75, rowZ + offset)
    const arm = new THREE.Group()
    arm.position.set(5, 1.1, rowZ + offset)
    block(arm, 0.4, 0.7, 0.4, colors.idle, 0, 0.35)
    const boom = block(arm, 1.25, 0.23, 0.25, colors.idle, 0.4, 0.8)
    block(arm, 0.2, 0.5, 0.25, 0xc3d3df, 0.95, 0.5)
    fixtures.add(arm)
    machines.push({ arm, boom, row: row.path, slot, active: false })
  }
  for (const positionX of [10.7, 13.3]) block(fixtures, 0.25, 2.6, 2.4, 0x48647c, positionX, 1.3, rowZ)
  block(fixtures, 2.9, 0.25, 2.4, 0x48647c, 12, 2.6, rowZ)
  const scanner = block(fixtures, 2.3, 0.06, 0.08, colors.done, 12, 1.7, rowZ)
  scanner.userData.scanner = true
  lamps.push({ lamp: scanner, row: row.path, station: 'gate' })
  block(fixtures, 2.7, 0.7, 2, 0x445c70, 17, 0.75, rowZ)
  const glass = new THREE.Mesh(new THREE.TorusGeometry(0.55, 0.09, 8, 24), new THREE.MeshStandardMaterial({ color: 0xc0d6e8 }))
  glass.position.set(17, 2.1, rowZ - 0.35)
  fixtures.add(glass)
  block(fixtures, 0.12, 0.7, 0.12, 0xc0d6e8, 17.45, 1.5, rowZ - 0.35)
  block(fixtures, 2.6, 0.7, 2, 0x445c70, 22, 0.75, rowZ)
  block(fixtures, 0.25, 0.2, 2, colors.attention, 22, 1.18, rowZ)
  forklift(rowZ)
  for (const [station, title, positionX, color] of [['repair', '维修间 · 扳手', 12, colors.attention], ['human', '人工柜台', 22, colors.failed]]) {
    block(fixtures, 4, 0.5, 2, 0x445c70, positionX, 0.6, rowZ + 5.2)
    block(fixtures, 0.2, 2.5, 0.2, color, positionX - 1.9, 1.5, rowZ + 6)
    block(fixtures, 0.2, 2.5, 0.2, color, positionX + 1.9, 1.5, rowZ + 6)
    label(fixtures, title, positionX, 3.3, rowZ + 5.2, 4.5)
    label(fixtures, station === 'repair' ? '🔧' : '人工', positionX, 2.4, rowZ + 5.2, 1.2)
  }
  if (row.deploy) {
    for (const positionX of [32.5, 35.5]) block(fixtures, 0.3, 3, 0.3, 0x63839f, positionX, 1.5, rowZ)
    block(fixtures, 3.3, 0.3, 0.3, 0x63839f, 34, 3, rowZ)
    block(fixtures, 0.3, 2.4, 0.3, 0x63839f, 40, 1.2, rowZ - 1.2)
    label(fixtures, '✓ smoke', 40, 2.7, rowZ, 2.8)
  }
}

function layout(scene) {
  const key = JSON.stringify(scene.rows.map(row => [row.path, row.deploy, row.workers, row.parcels.length]))
  const changed = key !== layoutKey
  const offsets = new Map()
  let rowZ = 0
  for (const row of scene.rows) {
    offsets.set(row.path, rowZ)
    rowZ += Math.max(12, row.workers * 1.8 + 7, row.parcels.length * 1.2 + 8)
  }
  if (!changed) return offsets
  layoutKey = key
  for (const child of [...fixtures.children]) dispose(child)
  machines = []
  lamps = []
  doors = []
  trucks = []
  for (const row of scene.rows) makeRow(row, offsets.get(row.path))
  for (const at of [-2.7, Math.max(7, rowZ - 5)]) {
    block(fixtures, 0.45, 5, 0.45, 0x60768b, -7, 2.5, at)
  }
  const door = block(fixtures, 0.2, 4.5, rowZ + 1.5, colors.idle, -7, 5.2, (rowZ - 3) / 2)
  doors.push(door)
  label(fixtures, '厂房入口', -7, 6, (rowZ - 3) / 2, 5)
  door.userData.lock = label(fixtures, '🔒 维护冻结', -7, 3.5, (rowZ - 3) / 2, 4, '#ed676b')
  if (!snapshot) {
    controls.target.set(18, 0, Math.max(0, (rowZ - 12) / 2))
    camera.position.set(43, Math.max(30, rowZ + 12), rowZ + 28)
  }
  return offsets
}

function applyScene(scene) {
  const offsets = layout(scene)
  const alive = new Set()
  for (const truckState of trucks) dispose(truckState.group)
  trucks = []
  for (const row of scene.rows) {
    const rowZ = offsets.get(row.path)
    const occupied = new Map()
    let lane = 0
    for (const parcel of row.parcels) {
      const key = `${row.path}\0${parcel.identifier}`
      alive.add(key)
      let state = parcels.get(key)
      if (!state) {
        const group = new THREE.Group()
        const box = block(group, 0.85, 0.8, 0.85, colors[parcel.tone], 0, 0.4)
        label(group, parcel.identifier, 0, 1.05, 0, 2.2)
        group.position.set(0, 1.15, rowZ)
        world.add(group)
        state = { group, box, target: new THREE.Vector3(), waypoints: [], parcel, row: row.path }
        parcels.set(key, state)
      }
      const previous = state.initialized ? state.parcel.station : undefined
      state.initialized = true
      state.parcel = parcel
      state.box.material.color.setHex(colors[parcel.tone])
      const slot = occupied.get(parcel.station) ?? 0
      occupied.set(parcel.station, slot + 1)
      let targetZ = rowZ + slot * 1.15
      let targetY = 1.15
      if (parcel.station === 'work') {
        targetZ = rowZ + (slot - (row.workers - 1) / 2) * 1.8
      } else if (parcel.station === 'human' || parcel.station === 'repair') {
        targetZ = rowZ + 5.2
        targetY = 1.2 + Math.floor(slot / 3) * 1.1
      } else if (parcel.station === 'return') {
        targetZ = rowZ + 3
        targetY = 0.15
      }
      const sideSlot = parcel.station === 'repair' || parcel.station === 'human' ? (slot % 3 - 1) * 1.2 : parcel.station === 'return' ? slot * 1.15 : 0
      state.target.set(positions[parcel.station] + sideSlot, targetY, targetZ)
      if (previous !== parcel.station) {
        state.rejected = previous === 'gate' && parcel.station === 'return'
        if (parcel.station === 'return' || previous === 'return') {
          const height = 2.2 + lane++ * 1.3
          state.waypoints = [
            new THREE.Vector3(state.group.position.x, height, state.group.position.z),
            new THREE.Vector3(state.target.x, height, state.target.z),
          ]
        } else {
          state.waypoints = parcel.station === 'repair' || parcel.station === 'human'
            ? [new THREE.Vector3(state.group.position.x, 3.7, rowZ), new THREE.Vector3(state.target.x, 3.7, targetZ)]
            : []
        }
      }
      if (state.blocker) { dispose(state.blocker); state.blocker = undefined }
      if (parcel.blocker) state.blocker = label(state.group, '⛓ 锁定', 0, 1.65, 0, 1.8, '#f6c75c')
      if (row.deploy && parcel.deployment && ['dispatch', 'smoke'].includes(parcel.station)) truck(targetZ, parcel)
    }
    for (const machine of machines.filter(one => one.row === row.path)) {
      machine.active = machine.slot < (occupied.get('work') ?? 0)
      machine.boom.material.color.setHex(machine.active ? colors.current : colors.idle)
    }
    for (const fixture of lamps.filter(one => one.row === row.path && !one.lamp.userData.scanner)) {
      const here = row.parcels.find(parcel => parcel.station === fixture.station)
      const done = row.parcels.some(parcel => !['repair', 'human', 'return', 'unknown'].includes(parcel.station) && positions[parcel.station] > positions[fixture.station])
      const color = here ? colors[here.tone] : done ? colors.done : colors.idle
      fixture.lamp.material.color.setHex(color)
    }
  }
  for (const [key, state] of parcels) {
    if (!alive.has(key)) { dispose(state.group); parcels.delete(key) }
  }
  for (const door of doors) {
    door.userData.lock.visible = scene.maintenance === 'frozen'
    door.position.y = scene.maintenance === 'frozen' ? 2.1 : scene.maintenance === 'draining' ? 3.7 : 6.7
    door.material.color.setHex(scene.maintenance === 'running' ? colors.done : colors.attention)
  }
  snapshot = scene
  snapshotTime.textContent = `快照 ${new Date(scene.generatedAt).toLocaleString()}`
  updateStatus()
}

function updateStatus() {
  const stale = snapshot && (snapshot.state !== 'ok' || Date.now() - snapshot.generatedAt > 30_000)
  const error = !connected || failures.size > 0
  const tone = error ? 'failed' : stale ? 'attention' : 'done'
  topLight.color.setHex(colors[tone])
  status.style.color = `#${colors[tone].toString(16).padStart(6, '0')}`
  status.textContent = !connected ? '连接中断，正在重连…' : failures.size > 0
    ? [...failures.values()].join('\n')
    : !snapshot ? '等待 driver 快照…'
      : `${stale ? `快照过期（${snapshot.state}）` : '快照正常'} · ${snapshot.maintenance} · ${snapshot.rows.reduce((sum, row) => sum + row.parcels.length, 0)} 张票`
}

const raycaster = new THREE.Raycaster()
const pointer = new THREE.Vector2(-2, -2)
let cursor = { x: 0, y: 0 }
renderer.domElement.addEventListener('pointermove', event => {
  pointer.set(event.clientX / innerWidth * 2 - 1, -(event.clientY / innerHeight) * 2 + 1)
  cursor = { x: event.clientX, y: event.clientY }
})
renderer.domElement.addEventListener('pointerleave', () => pointer.set(-2, -2))
function hover() {
  raycaster.setFromCamera(pointer, camera)
  const states = [...parcels.values()]
  const hit = raycaster.intersectObjects(states.map(state => state.box))[0]
  const state = hit && states.find(one => one.box === hit.object)
  tooltip.style.display = state ? 'block' : 'none'
  if (!state) return
  const parcel = state.parcel
  const seconds = parcel.since === undefined ? undefined : Math.max(0, Math.floor((Date.now() - parcel.since) / 1000))
  const elapsed = seconds === undefined ? parcel.duration : seconds < 60 ? `${seconds} 秒` : `${Math.floor(seconds / 60)} 分钟`
  tooltip.textContent = `${parcel.identifier} · ${parcel.title}\n${parcel.phase} · ${parcel.label}\n停留 ${elapsed}${parcel.reason ? `\n${parcel.reason}` : ''}${parcel.unknownDeploy ? '\n部署状态需 driver 补字段' : ''}`
  tooltip.style.left = `${Math.min(cursor.x + 16, innerWidth - tooltip.offsetWidth - 12)}px`
  tooltip.style.top = `${Math.min(cursor.y + 16, innerHeight - tooltip.offsetHeight - 12)}px`
}

const token = new URLSearchParams(location.search).get('t') ?? ''
const events = new EventSource(`/events?t=${encodeURIComponent(token)}`)
events.addEventListener('open', () => {
  failures.clear()
  connected = true
  updateStatus()
})
events.addEventListener('scene', event => { connected = true; applyScene(JSON.parse(event.data)) })
events.addEventListener('error', event => {
  if (typeof event.data === 'string') {
    const failure = JSON.parse(event.data)
    if (failure.recovered) failures.delete(failure.command)
    else failures.set(failure.command, `${failure.command} 失败 · 退出码 ${failure.exitCode ?? '未知'} · ${failure.reason}`)
  } else connected = false
  updateStatus()
})
addEventListener('beforeunload', () => events.close())
addEventListener('resize', () => {
  camera.aspect = innerWidth / innerHeight
  camera.updateProjectionMatrix()
  renderer.setSize(innerWidth, innerHeight)
})
setInterval(updateStatus, 1000)
const clock = new THREE.Clock()
function animate() {
  const delta = Math.min(clock.getDelta(), 0.1)
  const time = clock.elapsedTime
  for (const state of parcels.values()) {
    const target = state.waypoints[0] ?? state.target
    state.group.position.lerp(target, 1 - Math.exp(-3 * delta))
    if (state.waypoints.length && state.group.position.distanceTo(target) < 0.1) state.waypoints.shift()
  }
  for (const fixture of lamps.filter(one => one.lamp.userData.scanner)) {
    const rejecting = [...parcels.values()].some(state => state.row === fixture.row && state.rejected && state.waypoints.length > 0)
    fixture.lamp.material.color.setHex(rejecting ? colors.failed : colors.done)
  }
  for (const machine of machines) machine.arm.rotation.y = machine.active ? Math.sin(time * 2.5 + machine.slot) * 0.6 : 0
  for (const state of trucks) state.beacon.material.color.setHex(state.paused && Math.sin(time * 6) > 0 ? colors.failed : colors.attention)
  controls.update()
  hover()
  renderer.render(world, camera)
  requestAnimationFrame(animate)
}
animate()
