import * as THREE from 'three'
import { baselineColor, STAGE_BASELINE_MINUTES, stationNeighborhood, slotRules } from './model.mjs'

THREE.ColorManagement.enabled = false

const LIGHT_SCALE = Math.PI

const WORLD = {
  base: 0xf0dfc2,
  path: 0xfbf1dc,
  mezzanine: 0xe9b9a2,
  tower: 0xd98b78,
  wood: 0xb9a58c,
  water: 0x6fb9b4,
  dark: 0x5b4a52,
  cream: 0xfff6e3,
  roof: 0xc9766a,
  glass: 0xbfe3e0,
  unlit: 0x6d5a60,
  ink: 0x2b2a33,
  stamp: 0x2f9a6a,
}

const GLOW = {
  door: 0xffc76a,
  ok: 0x2f9a6a,
  waiting: 0xeea61b,
  failed: 0xe0442e,
  unknown: 0x7f8fa0,
  lantern: 0xe0442e,
  warn: 0xeea61b,
  bad: 0xe0442e,
}

export const SKIES = {
  clear: 0xf4dccb,
  fog: 0xe4ded8,
  overcast: 0xd3cdc8,
  dark: 0x8f8682,
}

const SEAS = {
  clear: 0xb4d6cc,
  fog: 0xc6d3cf,
  overcast: 0xb3bdba,
  dark: 0x6b7876,
}

const TAPE_SPAN = 0.66
const LOCK_Y = { closed: -0.6, open: -1.75 }
const MARK_Y = 1.45
const IDLE_FADE = 0.55
const SEA_Y = -0.7
const SEA_SIZE = 4000
const MOVE_MS = 1200
const UNIT_BOX = new THREE.BoxGeometry(1, 1, 1)
const UNIT_CONE = new THREE.ConeGeometry(1, 1, 4)
const MARK = new THREE.OctahedronGeometry(0.3)
const BASE_SIDE = new THREE.Color(WORLD.base).multiplyScalar(0.72).getHex()

const material = (color, options = {}) => new THREE.MeshLambertMaterial({ color, emissiveIntensity: 0.5, ...options })

const palette = new Map()
const shared = (color, options = {}) => {
  const key = `${color}/${options.opacity ?? 1}/${options.emissive ?? ''}`
  if (!palette.has(key)) palette.set(key, material(color, options))
  return palette.get(key)
}

const SKY_TONE = new THREE.Color(SKIES.clear)
const faded = color => new THREE.Color(color).lerp(SKY_TONE, IDLE_FADE).getHex()
const plain = color => color

const kitOf = (parent, tone = plain) => ({
  station(name) {
    const group = new THREE.Group()
    group.userData.station = name
    parent.add(group)
    return kitOf(group, tone)
  },
  box([w, h, d], color, [x, y, z], options = {}) {
    const paint = tone(color)
    const mesh = new THREE.Mesh(UNIT_BOX, 'emissive' in options ? material(paint, options) : shared(paint, options))
    mesh.scale.set(w, h, d)
    mesh.position.set(x, y + h / 2, z)
    parent.add(mesh)
    return mesh
  },
  cone(radius, height, color, [x, y, z]) {
    const mesh = new THREE.Mesh(UNIT_CONE, shared(tone(color)))
    mesh.scale.set(radius, height, radius)
    mesh.position.set(x, y, z)
    mesh.rotation.y = Math.PI / 4
    parent.add(mesh)
  },
})

const glow = (mesh, color) => {
  mesh.material.color.setHex(color)
  mesh.material.emissive.setHex(color)
}

const groundBlock = (kit, block) => {
  const width = block.right - block.left
  const depth = block.front - block.back
  const x = (block.left + block.right) / 2
  const z = (block.back + block.front) / 2
  kit.box([width, 2.02, depth], BASE_SIDE, [x, -2.2, z]).name = 'ground-side'
  kit.box([width, 0.18, depth], WORLD.base, [x, -0.18, z]).name = 'ground'
}

const buildGround = (kit, layout) => {
  const { ground, water } = layout
  const pathLeft = layout.tower + 1.2
  for (const block of [ground, layout.yard].filter(Boolean)) {
    groundBlock(kit, block)
  }
  kit.box([ground.right - pathLeft, 0.22, 1.7], WORLD.path, [(pathLeft + ground.right) / 2, 0, 0]).name = 'path'
  kit.box([4.6, 0.3, 1.3], WORLD.wood, [9.3, 0, 0]).name = 'pier'
  kit.box([3.6, 0.9, water.front - water.back], WORLD.water, [13.2, -1.4, (water.back + water.front) / 2], { transparent: true, opacity: 0.92 }).name = 'water'
  groundBlock(kit, { ...water, left: 15 })
}

const buildMainline = (islandKit, layout) => {
  let kit = islandKit.station('pickup')
  const tiers = [[1.9, 0.5, WORLD.tower], [1.6, 0.5, WORLD.cream], [1.9, 0.5, WORLD.tower], [1.6, 0.5, WORLD.cream], [1.9, 0.4, WORLD.tower]]
  tiers.reduce((y, [width, height, color]) => {
    kit.box([width, height, width], color, [layout.tower, y, 0])
    return y + height
  }, 0)
  kit = islandKit.station('work')
  const doors = layout.booths.map(({ x, z }) => {
    kit.box([1.5, 1.5, 1.3], WORLD.mezzanine, [x, 0, z])
    kit.box([1.7, 0.2, 1.5], WORLD.roof, [x, 1.5, z])
    return kit.box([0.7, 0.95, 0.06], WORLD.dark, [x, 0.22, z + 0.67], { emissive: 0x000000 })
  })
  kit = islandKit.station('gate')
  kit.box([1.7, 1.7, 1.7], WORLD.glass, [-1, 0.22, 0], { transparent: true, opacity: 0.34 })
  for (const [x, z] of [[-1.8, -0.8], [-1.8, 0.8], [-0.2, -0.8], [-0.2, 0.8]]) kit.box([0.12, 1.8, 0.12], WORLD.cream, [x, 0.22, z])
  kit.box([1.84, 0.12, 1.84], WORLD.cream, [-1, 1.96, 0])
  kit = islandKit.station('review')
  kit.box([0.3, 2, 0.3], WORLD.tower, [2.5, 0.22, -0.95])
  kit.box([0.3, 2, 0.3], WORLD.tower, [2.5, 0.22, 0.95])
  kit.box([0.5, 0.35, 2.5], WORLD.tower, [2.5, 2.2, 0])
  islandKit.station('pack').box([1.5, 0.22, 1.5], WORLD.mezzanine, [5.5, 0.22, 0])
  return doors
}

const buildMezzanine = islandKit => {
  let kit = islandKit
  kit.box([15.5, 1.5, 3], WORLD.mezzanine, [-0.25, 0, -5.6])
  for (let step = 0; step < 5; step++) kit.box([1.3, 0.3 * (step + 1), 0.62], WORLD.mezzanine, [-1, 0, -1.2 - step * 0.62])
  kit = islandKit.station('return')
  kit.box([1, 0.14, 2.4], WORLD.mezzanine, [-6.5, 1.05, -3.3]).rotation.x = -0.42
  kit = islandKit.station('repair')
  kit.box([1.5, 2.6, 1.5], WORLD.tower, [-1, 1.5, -6.3])
  kit.cone(1.2, 1.1, WORLD.roof, [-1, 4.65, -6.3])
  kit = islandKit.station('human')
  kit.box([1.8, 0.2, 1.6], WORLD.cream, [3, 1.5, -5.6])
  kit.box([0.12, 2.3, 0.12], WORLD.dark, [4.2, 1.5, -6.6])
  return kit.box([0.42, 0.5, 0.42], WORLD.unlit, [4.2, 3.8, -6.6], { emissive: 0x000000 })
}

const buildWaterfront = islandKit => {
  let kit = islandKit.station('dispatch')
  kit.box([0.4, 1.5, 0.5], WORLD.tower, [11.2, 0, 3])
  kit.box([0.4, 2.9, 0.5], WORLD.tower, [15.2, -1.4, 3])
  const lock = kit.box([3.6, 1.1, 0.3], WORLD.dark, [13.2, LOCK_Y.closed, 3])
  kit = islandKit.station('smoke')
  kit.box([0.9, 3, 0.9], WORLD.cream, [16.2, 0, 6.6])
  kit.box([1.1, 0.2, 1.1], WORLD.tower, [16.2, 3, 6.6])
  const beacon = kit.box([0.6, 0.6, 0.6], WORLD.unlit, [16.2, 3.2, 6.6], { emissive: 0x000000 })
  const halo = kit.box([0.8, 0.1, 0.8], GLOW.warn, [16.2, 3.45, 6.6], { emissive: GLOW.warn })
  return { lock, beacon, halo }
}

const buildHub = (scene, hub) => {
  const group = new THREE.Group()
  group.position.set(hub.x, 0, hub.z)
  scene.add(group)
  const kit = kitOf(group)
  groundBlock(kit, { left: -hub.half, right: hub.half, back: -hub.half, front: hub.half })
  kit.box([1.1, 4.2, 1.1], WORLD.tower, [0, 0, -1])
  kit.cone(0.95, 1.2, WORLD.roof, [0, 4.8, -1])
  kit.box([0.6, 0.6, 0.06], WORLD.cream, [0, 3.1, -0.42])
}

const dressIsland = (parts, island) => {
  parts.doors.forEach((door, index) => door.material.emissive.setHex(index < island.working ? GLOW.door : 0x000000))
  parts.lock.position.y = LOCK_Y[island.lock] + 0.55
  if (island.beacon !== 'off') glow(parts.beacon, GLOW[island.beacon])
  parts.halo.visible = island.beacon === 'unknown'
  if (island.lantern) glow(parts.lantern, GLOW.lantern)
}

const buildIsland = (scene, island, fade) => {
  const land = new THREE.Group()
  land.position.set(island.origin.x, 0, island.origin.z)
  land.userData.island = island.index
  scene.add(land)
  buildGround(kitOf(land), island.layout)
  const kit = kitOf(land, fade ? faded : plain)
  dressIsland({ doors: buildMainline(kit, island.layout), lantern: buildMezzanine(kit), ...buildWaterfront(kit) }, island)
  const rules = slotRules(island.layout)
  for (const station of Object.keys(STAGE_BASELINE_MINUTES)) {
    const count = station === 'work' ? island.layout.booths.length : 1
    for (let index = 0; index < count; index++) {
      const slot = rules[station](index)
      const tile = kitOf(land).box([1.7, 0.025, 1.5], baselineColor(station), [slot.x, slot.y + 0.005, slot.z])
      tile.material = new THREE.MeshBasicMaterial({ color: baselineColor(station) })
      tile.name = 'baseline-tile'
      tile.visible = false
    }
  }
  return land
}

const buildParcel = (scene, view, island) => {
  const group = new THREE.Group()
  const kit = kitOf(group)
  const body = kit.box([0.74, 0.74, 0.74], view.color, [0, 0, 0])
  body.userData.key = view.key
  kit.box([0.78, 0.16, 0.78], WORLD.ink, [0, 0.3, 0]).visible = view.band
  kit.box([0.3, 0.06, 0.3], WORLD.stamp, [0, 0.74, 0]).visible = view.stamp
  kit.box([1.25, 0.12, 1.25], WORLD.wood, [0, -0.12, 0]).visible = view.raft
  const tape = shared(view.tapesSpent ? WORLD.ink : WORLD.cream)
  for (let index = 0; index < view.tapes; index++) {
    const strip = new THREE.Mesh(UNIT_BOX, tape)
    strip.scale.set(0.07, 0.62, 0.04)
    strip.position.set(((index + 0.5) / view.tapes - 0.5) * TAPE_SPAN, 0.37, 0.39)
    group.add(strip)
  }
  const markColor = GLOW[view.alert] ?? GLOW.warn
  const mark = new THREE.Mesh(MARK, shared(markColor, { emissive: markColor }))
  mark.position.y = MARK_Y
  mark.visible = view.alert !== null
  group.add(mark)
  const ring = kit.box([1.2, 0.05, 1.2], WORLD.ink, [0, -0.02, 0])
  ring.visible = false
  const overdueRing = new THREE.Mesh(new THREE.RingGeometry(0.65, 0.78, 48), shared(GLOW[view.overdue] ?? GLOW.warn))
  overdueRing.name = 'overdue-ring'
  overdueRing.rotation.x = -Math.PI / 2
  overdueRing.position.y = 0.05
  overdueRing.visible = Boolean(view.overdue)
  group.add(overdueRing)
  const target = new THREE.Vector3(view.slot.x + island.origin.x, view.slot.y, view.slot.z + island.origin.z)
  group.position.copy(target)
  scene.add(group)
  return { id: `${island.path}\0${view.identifier}`, group, body, mark, ring, view, target, from: null, start: null }
}

const buildSea = scene => {
  const sea = new THREE.Mesh(new THREE.PlaneGeometry(SEA_SIZE, SEA_SIZE), material(SEAS.clear))
  sea.rotation.x = -Math.PI / 2
  sea.position.y = SEA_Y
  sea.name = 'sea'
  scene.add(sea)
  const vertices = []
  for (let z = -SEA_SIZE / 2; z < SEA_SIZE / 2; z += 5) {
    const left = -SEA_SIZE / 2, right = SEA_SIZE / 2, y = SEA_Y + 0.005
    vertices.push(left, y, z, left, y, z + 0.09, right, y, z, right, y, z, left, y, z + 0.09, right, y, z + 0.09)
  }
  const geometry = new THREE.BufferGeometry()
  geometry.setAttribute('position', new THREE.Float32BufferAttribute(vertices, 3))
  geometry.computeVertexNormals()
  const pattern = new THREE.Mesh(geometry, material(SEAS.clear, { transparent: true, opacity: 0.16, depthWrite: false }))
  pattern.name = 'sea-pattern'
  scene.add(pattern)
  return sea
}

const outline = ({ ground, yard, water }, margin) => {
  const points = [[ground.left - margin, ground.front + margin]]
  if (water) points.push(
    [water.left - margin, ground.front + margin], [water.left - margin, water.front + margin],
    [water.right + margin, water.front + margin], [water.right + margin, water.back - margin],
    [water.left - margin, water.back - margin], [water.left - margin, ground.back - margin],
  )
  else points.push([ground.right + margin, ground.front + margin], [ground.right + margin, ground.back - margin])
  if (yard) points.push([yard.right + margin, ground.back - margin], [yard.right + margin, yard.back - margin], [yard.left - margin, yard.back - margin])
  points.push([ground.left - margin, ground.back - margin])
  return points.map(([x, z]) => new THREE.Vector2(x, -z))
}

const buildShore = (content, layout, origin, shorelinePaint, contours) => {
  const key = JSON.stringify([layout.ground, layout.yard, layout.water])
  if (!contours.has(key)) {
    const rim = new THREE.Shape(outline(layout, 0.22))
    rim.holes.push(new THREE.Path(outline(layout, 0)))
    contours.set(key, {
      shadow: new THREE.ShapeGeometry(new THREE.Shape(outline(layout, 0.7))),
      shoreline: new THREE.ShapeGeometry(rim),
    })
  }
  const geometry = contours.get(key)
  const shadow = new THREE.Mesh(geometry.shadow, shared(WORLD.dark, { transparent: true, opacity: 0.23, depthWrite: false }))
  shadow.name = 'contact-shadow'
  shadow.rotation.x = -Math.PI / 2
  shadow.position.set(origin.x - 0.25, SEA_Y + 0.015, origin.z - 0.55)
  content.add(shadow)
  const shoreline = new THREE.Mesh(geometry.shoreline, shorelinePaint)
  shoreline.name = 'shoreline'
  shoreline.rotation.x = -Math.PI / 2
  shoreline.position.set(origin.x, SEA_Y + 0.025, origin.z)
  content.add(shoreline)
}

const tintSea = (built, sky) => {
  if (!built.sea) return
  const color = new THREE.Color(SEAS[sky])
  built.sea.material.color.copy(color)
  built.content.getObjectByName('sea-pattern').material.color.copy(color).lerp(new THREE.Color(0xffffff), 0.28)
  built.content.getObjectByName('shoreline').material.color.copy(color).lerp(new THREE.Color(0xffffff), 0.48)
}

const SHARED_GEOMETRY = new Set([UNIT_BOX, UNIT_CONE, MARK])

const release = group => {
  const kept = new Set(palette.values())
  const released = new Set()
  group.traverse(node => {
    if (!node.isMesh) return
    if (!SHARED_GEOMETRY.has(node.geometry) && !released.has(node.geometry)) {
      node.geometry.dispose()
      released.add(node.geometry)
    }
    if (!kept.has(node.material) && !released.has(node.material)) {
      node.material.dispose()
      released.add(node.material)
    }
  })
  group.removeFromParent()
}

const travel = (parcel, time, reduced) => {
  parcel.start ??= time
  const progress = reduced ? 1 : Math.min(1, (time - parcel.start) / MOVE_MS)
  if (progress === 1) {
    parcel.group.position.copy(parcel.target)
    parcel.from = null
    return
  }
  parcel.group.position.lerpVectors(parcel.from, parcel.target, progress * progress * (3 - 2 * progress))
}

const rendererOf = canvas => {
  try {
    return new THREE.WebGLRenderer({ canvas, antialias: true })
  } catch (error) {
    throw new Error(`浏览器无法创建 WebGL2 上下文，工坊沙盘无法绘制。请在浏览器设置里开启硬件加速或 WebGL 后刷新页面。（${error.message}）`, { cause: error })
  }
}

const buildContent = (scene, archipelago, previous) => {
  const content = new THREE.Group()
  scene.add(content)
  const sea = archipelago.global ? buildSea(content) : null
  if (sea) {
    const shorelinePaint = material(SEAS.clear)
    const contours = new Map()
    for (const island of archipelago.islands) buildShore(content, island.layout, island.origin, shorelinePaint, contours)
    const hub = archipelago.hub
    buildShore(content, { ground: { left: -hub.half, right: hub.half, back: -hub.half, front: hub.half } }, hub, shorelinePaint, contours)
  }
  if (archipelago.hub) buildHub(content, archipelago.hub)
  const lands = archipelago.islands.map(island => buildIsland(content, island, archipelago.global && island.idle))
  const parcels = new Map(archipelago.islands.flatMap(island => island.parcels.map(view => {
    const parcel = buildParcel(content, view, island)
    const from = previous.get(parcel.id)
    if (from && !from.equals(parcel.target)) {
      parcel.from = from
      parcel.group.position.copy(from)
    }
    return [view.key, parcel]
  })))
  return { content, sea, parcels, lands, targets: [...lands, ...[...parcels.values()].map(parcel => parcel.body)] }
}

export const createWorld = canvas => {
  const renderer = rendererOf(canvas)
  renderer.outputColorSpace = THREE.LinearSRGBColorSpace
  renderer.setPixelRatio(Math.min(devicePixelRatio || 1, 2))
  const scene = new THREE.Scene()
  scene.add(new THREE.AmbientLight(0xffffff, 0.62 * LIGHT_SCALE))
  const sun = new THREE.DirectionalLight(0xffffff, 0.62 * LIGHT_SCALE)
  sun.position.set(0.35, 1, 0.8)
  scene.add(sun)
  const raycaster = new THREE.Raycaster()
  let sky = 'clear'
  let built = { content: null, sea: null, parcels: new Map(), targets: [] }
  let islands = []

  return {
    get parcels() {
      return built.parcels
    },
    build(archipelago) {
      islands = archipelago.islands
      const previous = new Map([...built.parcels.values()].map(parcel => [parcel.id, parcel.group.position.clone()]))
      if (built.content) release(built.content)
      built = buildContent(scene, archipelago, previous)
      tintSea(built, sky)
    },
    neighborhood(key) {
      const selected = built.parcels.get(key)
      const island = islands.find(one => one.parcels.some(parcel => parcel.key === key))
      const { station } = selected.view
      const rules = slotRules(island.layout)
      const bounds = new THREE.Box3()
      for (const name of stationNeighborhood(station)) {
        const slot = rules[name](0)
        const point = new THREE.Vector3(slot.x + island.origin.x, slot.y, slot.z + island.origin.z)
        bounds.expandByPoint(point.clone().addScalar(-0.4))
        bounds.expandByPoint(point.clone().add(new THREE.Vector3(0.4, 2, 0.4)))
      }
      const land = built.lands[island.index]
      land.updateWorldMatrix(true, true)
      land.traverse(node => {
        if (node.userData.station === (station === 'unknown' ? 'pickup' : station) || station === 'merge' && node.name === 'pier') bounds.expandByObject(node)
      })
      for (const parcel of built.parcels.values()) {
        if (parcel.view.station !== station || !island.parcels.some(one => one.key === parcel.view.key)) continue
        bounds.expandByPoint(parcel.target.clone().addScalar(-0.4))
        bounds.expandByPoint(parcel.target.clone().add(new THREE.Vector3(0.4, 2, 0.4)))
      }
      return [bounds.min.x, bounds.max.x].flatMap(x => [bounds.min.y, bounds.max.y].flatMap(y => [bounds.min.z, bounds.max.z].map(z => ({ x, y, z }))))
    },
    setSky(kind) {
      sky = kind
      renderer.setClearColor(SKIES[kind])
      tintSea(built, kind)
    },
    resize(width, height) {
      renderer.setSize(width, height, false)
    },
    select(key) {
      for (const parcel of built.parcels.values()) parcel.ring.visible = parcel.view.key === key
    },
    focus(stop, index) {
      for (const land of built.lands) land.traverse(node => {
        if (node.name === 'baseline-tile') node.visible = stop !== 1 && land.userData.island === index
      })
    },
    pick(ndc, camera) {
      raycaster.setFromCamera(ndc, camera)
      const hit = raycaster.intersectObjects(built.targets, true)[0]?.object
      if (!hit) return null
      if (hit.userData.key) return { key: hit.userData.key }
      let land = hit.parent
      while (land.userData.island === undefined) land = land.parent
      return { island: land.userData.island }
    },
    animate(time, reduced) {
      for (const parcel of built.parcels.values()) {
        const { mark, group } = parcel
        if (parcel.from) travel(parcel, time, reduced)
        mark.position.y = reduced ? MARK_Y : MARK_Y + Math.sin(time / 500 + group.position.x) * 0.08
        mark.rotation.y = reduced ? 0 : time / 1400
      }
    },
    render(camera) {
      renderer.render(scene, camera)
    },
    dispose() {
      if (built.content) release(built.content)
      renderer.dispose()
      renderer.forceContextLoss()
    },
  }
}
