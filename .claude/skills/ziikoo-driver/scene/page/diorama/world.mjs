import * as THREE from 'three'

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

const LOCK_Y = { closed: -0.6, open: -1.75 }
const MARK_Y = 1.45
const IDLE_FADE = 0.55
const SEA_Y = -0.7
const SEA_SIZE = 4000
const MOVE_MS = 1200
const UNIT_BOX = new THREE.BoxGeometry(1, 1, 1)
const UNIT_CONE = new THREE.ConeGeometry(1, 1, 4)
const MARK = new THREE.OctahedronGeometry(0.3)

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

const buildGround = (kit, layout) => {
  const { ground, water } = layout
  const pathLeft = layout.tower + 1.2
  for (const block of [ground, layout.yard].filter(Boolean)) {
    kit.box([block.right - block.left, 1.4, block.front - block.back], WORLD.base, [(block.left + block.right) / 2, -1.4, (block.back + block.front) / 2])
  }
  kit.box([ground.right - pathLeft, 0.22, 1.7], WORLD.path, [(pathLeft + ground.right) / 2, 0, 0])
  kit.box([4.6, 0.3, 1.3], WORLD.wood, [9.3, 0, 0])
  kit.box([3.6, 0.9, water.front - water.back], WORLD.water, [13.2, -1.4, (water.back + water.front) / 2], { transparent: true, opacity: 0.92 })
  kit.box([water.right - 15, 1.4, water.front - water.back], WORLD.base, [(15 + water.right) / 2, -1.4, (water.back + water.front) / 2])
}

const buildMainline = (kit, layout) => {
  const tiers = [[1.9, 0.5, WORLD.tower], [1.6, 0.5, WORLD.cream], [1.9, 0.5, WORLD.tower], [1.6, 0.5, WORLD.cream], [1.9, 0.4, WORLD.tower]]
  tiers.reduce((y, [width, height, color]) => {
    kit.box([width, height, width], color, [layout.tower, y, 0])
    return y + height
  }, 0)
  const doors = layout.booths.map(({ x, z }) => {
    kit.box([1.5, 1.5, 1.3], WORLD.mezzanine, [x, 0, z])
    kit.box([1.7, 0.2, 1.5], WORLD.roof, [x, 1.5, z])
    return kit.box([0.7, 0.95, 0.06], WORLD.dark, [x, 0.22, z + 0.67], { emissive: 0x000000 })
  })
  kit.box([1.7, 1.7, 1.7], WORLD.glass, [-1, 0.22, 0], { transparent: true, opacity: 0.34 })
  for (const [x, z] of [[-1.8, -0.8], [-1.8, 0.8], [-0.2, -0.8], [-0.2, 0.8]]) kit.box([0.12, 1.8, 0.12], WORLD.cream, [x, 0.22, z])
  kit.box([1.84, 0.12, 1.84], WORLD.cream, [-1, 1.96, 0])
  kit.box([0.3, 2, 0.3], WORLD.tower, [2.5, 0.22, -0.95])
  kit.box([0.3, 2, 0.3], WORLD.tower, [2.5, 0.22, 0.95])
  kit.box([0.5, 0.35, 2.5], WORLD.tower, [2.5, 2.2, 0])
  kit.box([1.5, 0.22, 1.5], WORLD.mezzanine, [5.5, 0.22, 0])
  return doors
}

const buildMezzanine = kit => {
  kit.box([13, 1.5, 3], WORLD.mezzanine, [-1.5, 0, -5.6])
  for (let step = 0; step < 5; step++) kit.box([1.3, 0.3 * (step + 1), 0.62], WORLD.mezzanine, [-1, 0, -1.2 - step * 0.62])
  kit.box([1, 0.14, 2.4], WORLD.mezzanine, [-6.5, 1.05, -3.3]).rotation.x = -0.42
  kit.box([1.5, 2.6, 1.5], WORLD.tower, [-1, 1.5, -6.3])
  kit.cone(1.2, 1.1, WORLD.roof, [-1, 4.65, -6.3])
  kit.box([1.8, 0.2, 1.6], WORLD.cream, [3, 1.5, -5.6])
  kit.box([0.12, 2.3, 0.12], WORLD.dark, [4.2, 1.5, -6.6])
  return kit.box([0.42, 0.5, 0.42], WORLD.unlit, [4.2, 3.8, -6.6], { emissive: 0x000000 })
}

const buildWaterfront = kit => {
  kit.box([0.4, 1.5, 0.5], WORLD.tower, [11.2, 0, 3])
  kit.box([0.4, 2.9, 0.5], WORLD.tower, [15.2, -1.4, 3])
  const lock = kit.box([3.6, 1.1, 0.3], WORLD.dark, [13.2, LOCK_Y.closed, 3])
  kit.box([0.9, 3, 0.9], WORLD.cream, [16.2, 0, 6.6])
  kit.box([1.1, 0.2, 1.1], WORLD.tower, [16.2, 3, 6.6])
  const beacon = kit.box([0.6, 0.6, 0.6], WORLD.unlit, [16.2, 3.2, 6.6], { emissive: 0x000000 })
  return { lock, beacon }
}

const buildHub = (scene, hub) => {
  const group = new THREE.Group()
  group.position.set(hub.x, 0, hub.z)
  scene.add(group)
  const kit = kitOf(group)
  kit.box([hub.half * 2, 1.4, hub.half * 2], WORLD.base, [0, -1.4, 0])
  kit.box([1.1, 4.2, 1.1], WORLD.tower, [0, 0, -1])
  kit.cone(0.95, 1.2, WORLD.roof, [0, 4.8, -1])
  kit.box([0.6, 0.6, 0.06], WORLD.cream, [0, 3.1, -0.42])
}

const dressIsland = (parts, island) => {
  parts.doors.forEach((door, index) => door.material.emissive.setHex(index < island.working ? GLOW.door : 0x000000))
  parts.lock.position.y = LOCK_Y[island.lock] + 0.55
  if (island.beacon !== 'off') glow(parts.beacon, GLOW[island.beacon])
  if (island.lantern) glow(parts.lantern, GLOW.lantern)
}

const buildIsland = (scene, island, fade) => {
  const land = new THREE.Group()
  land.position.set(island.origin.x, 0, island.origin.z)
  land.userData.island = island.index
  scene.add(land)
  const kit = kitOf(land, fade ? faded : plain)
  buildGround(kit, island.layout)
  dressIsland({ doors: buildMainline(kit, island.layout), lantern: buildMezzanine(kit), ...buildWaterfront(kit) }, island)
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
  const markColor = GLOW[view.alert] ?? GLOW.warn
  const mark = new THREE.Mesh(MARK, shared(markColor, { emissive: markColor }))
  mark.position.y = MARK_Y
  mark.visible = view.alert !== null
  group.add(mark)
  const ring = kit.box([1.2, 0.05, 1.2], WORLD.ink, [0, -0.02, 0])
  ring.visible = false
  const target = new THREE.Vector3(view.slot.x + island.origin.x, view.slot.y, view.slot.z + island.origin.z)
  group.position.copy(target)
  scene.add(group)
  return { id: `${island.path}\0${view.identifier}`, group, body, mark, ring, view, target, from: null, start: null }
}

const buildSea = scene => {
  const sea = new THREE.Mesh(new THREE.PlaneGeometry(SEA_SIZE, SEA_SIZE), material(SEAS.clear))
  sea.rotation.x = -Math.PI / 2
  sea.position.y = SEA_Y
  scene.add(sea)
  return sea
}

const SHARED_GEOMETRY = new Set([UNIT_BOX, UNIT_CONE, MARK])

const release = group => {
  const kept = new Set(palette.values())
  group.traverse(node => {
    if (!node.isMesh) return
    if (!SHARED_GEOMETRY.has(node.geometry)) node.geometry.dispose()
    if (!kept.has(node.material)) node.material.dispose()
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
  return { content, sea, parcels, targets: [...lands, ...[...parcels.values()].map(parcel => parcel.body)] }
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

  return {
    get parcels() {
      return built.parcels
    },
    build(archipelago) {
      const previous = new Map([...built.parcels.values()].map(parcel => [parcel.id, parcel.group.position.clone()]))
      if (built.content) release(built.content)
      built = buildContent(scene, archipelago, previous)
      built.sea?.material.color.setHex(SEAS[sky])
    },
    setSky(kind) {
      sky = kind
      renderer.setClearColor(SKIES[kind])
      built.sea?.material.color.setHex(SEAS[kind])
    },
    resize(width, height) {
      renderer.setSize(width, height, false)
    },
    select(key) {
      for (const parcel of built.parcels.values()) parcel.ring.visible = parcel.view.key === key
    },
    pick(ndc, camera) {
      raycaster.setFromCamera(ndc, camera)
      const hit = raycaster.intersectObjects(built.targets, true)[0]?.object
      if (!hit) return null
      if (hit.userData.key) return { key: hit.userData.key }
      return { island: hit.parent.userData.island }
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
