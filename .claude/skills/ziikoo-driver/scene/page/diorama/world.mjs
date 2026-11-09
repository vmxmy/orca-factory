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

const LOCK_Y = { closed: -0.6, open: -1.75 }
const MARK_Y = 1.45

const material = (color, options = {}) => new THREE.MeshLambertMaterial({ color, emissiveIntensity: 0.5, ...options })

const box = (parent, [w, h, d], color, [x, y, z], options) => {
  const mesh = new THREE.Mesh(new THREE.BoxGeometry(w, h, d), material(color, options))
  mesh.position.set(x, y + h / 2, z)
  parent.add(mesh)
  return mesh
}

const cone = (parent, radius, height, color, [x, y, z]) => {
  const mesh = new THREE.Mesh(new THREE.ConeGeometry(radius, height, 4), material(color))
  mesh.position.set(x, y, z)
  mesh.rotation.y = Math.PI / 4
  parent.add(mesh)
}

const glow = (mesh, color) => {
  mesh.material.color.setHex(color)
  mesh.material.emissive.setHex(color)
}

const buildGround = (group, layout) => {
  const { ground, water } = layout
  const pathLeft = layout.tower + 1.2
  for (const block of [ground, layout.yard].filter(Boolean)) {
    box(group, [block.right - block.left, 1.4, block.front - block.back], WORLD.base, [(block.left + block.right) / 2, -1.4, (block.back + block.front) / 2])
  }
  box(group, [ground.right - pathLeft, 0.22, 1.7], WORLD.path, [(pathLeft + ground.right) / 2, 0, 0])
  box(group, [4.6, 0.3, 1.3], WORLD.wood, [9.3, 0, 0])
  box(group, [3.6, 0.9, water.front - water.back], WORLD.water, [13.2, -1.4, (water.back + water.front) / 2], { transparent: true, opacity: 0.92 })
  box(group, [water.right - 15, 1.4, water.front - water.back], WORLD.base, [(15 + water.right) / 2, -1.4, (water.back + water.front) / 2])
}

const buildMainline = (group, layout) => {
  const tiers = [[1.9, 0.5, WORLD.tower], [1.6, 0.5, WORLD.cream], [1.9, 0.5, WORLD.tower], [1.6, 0.5, WORLD.cream], [1.9, 0.4, WORLD.tower]]
  tiers.reduce((y, [width, height, color]) => {
    box(group, [width, height, width], color, [layout.tower, y, 0])
    return y + height
  }, 0)
  const doors = layout.booths.map(({ x, z }) => {
    box(group, [1.5, 1.5, 1.3], WORLD.mezzanine, [x, 0, z])
    box(group, [1.7, 0.2, 1.5], WORLD.roof, [x, 1.5, z])
    return box(group, [0.7, 0.95, 0.06], WORLD.dark, [x, 0.22, z + 0.67], { emissive: 0x000000 })
  })
  box(group, [1.7, 1.7, 1.7], WORLD.glass, [-1, 0.22, 0], { transparent: true, opacity: 0.34 })
  for (const [x, z] of [[-1.8, -0.8], [-1.8, 0.8], [-0.2, -0.8], [-0.2, 0.8]]) box(group, [0.12, 1.8, 0.12], WORLD.cream, [x, 0.22, z])
  box(group, [1.84, 0.12, 1.84], WORLD.cream, [-1, 1.96, 0])
  box(group, [0.3, 2, 0.3], WORLD.tower, [2.5, 0.22, -0.95])
  box(group, [0.3, 2, 0.3], WORLD.tower, [2.5, 0.22, 0.95])
  box(group, [0.5, 0.35, 2.5], WORLD.tower, [2.5, 2.2, 0])
  box(group, [1.5, 0.22, 1.5], WORLD.mezzanine, [5.5, 0.22, 0])
  return doors
}

const buildMezzanine = group => {
  box(group, [13, 1.5, 3], WORLD.mezzanine, [-1.5, 0, -5.6])
  for (let step = 0; step < 5; step++) box(group, [1.3, 0.3 * (step + 1), 0.62], WORLD.mezzanine, [-1, 0, -1.2 - step * 0.62])
  box(group, [1, 0.14, 2.4], WORLD.mezzanine, [-6.5, 1.05, -3.3]).rotation.x = -0.42
  box(group, [1.5, 2.6, 1.5], WORLD.tower, [-1, 1.5, -6.3])
  cone(group, 1.2, 1.1, WORLD.roof, [-1, 4.65, -6.3])
  box(group, [1.8, 0.2, 1.6], WORLD.cream, [3, 1.5, -5.6])
  box(group, [0.12, 2.3, 0.12], WORLD.dark, [4.2, 1.5, -6.6])
  return box(group, [0.42, 0.5, 0.42], WORLD.unlit, [4.2, 3.8, -6.6], { emissive: 0x000000 })
}

const buildWaterfront = group => {
  box(group, [0.4, 1.5, 0.5], WORLD.tower, [11.2, 0, 3])
  box(group, [0.4, 2.9, 0.5], WORLD.tower, [15.2, -1.4, 3])
  const lock = box(group, [3.6, 1.1, 0.3], WORLD.dark, [13.2, LOCK_Y.closed, 3])
  box(group, [0.9, 3, 0.9], WORLD.cream, [16.2, 0, 6.6])
  box(group, [1.1, 0.2, 1.1], WORLD.tower, [16.2, 3, 6.6])
  const beacon = box(group, [0.6, 0.6, 0.6], GLOW.ok, [16.2, 3.2, 6.6], { emissive: GLOW.ok })
  return { lock, beacon }
}

const buildParcel = (scene, view) => {
  const group = new THREE.Group()
  const body = box(group, [0.74, 0.74, 0.74], view.color, [0, 0, 0])
  body.userData.identifier = view.identifier
  box(group, [0.78, 0.16, 0.78], WORLD.ink, [0, 0.3, 0]).visible = view.band
  box(group, [0.3, 0.06, 0.3], WORLD.stamp, [0, 0.74, 0]).visible = view.stamp
  box(group, [1.25, 0.12, 1.25], WORLD.wood, [0, -0.12, 0]).visible = view.raft
  const markColor = GLOW[view.alert] ?? GLOW.warn
  const mark = new THREE.Mesh(new THREE.OctahedronGeometry(0.3), material(markColor, { emissive: markColor }))
  mark.position.y = MARK_Y
  mark.visible = view.alert !== null
  group.add(mark)
  const ring = box(group, [1.2, 0.05, 1.2], WORLD.ink, [0, -0.02, 0])
  ring.visible = false
  group.position.set(view.slot.x, view.slot.y, view.slot.z)
  scene.add(group)
  return { group, body, mark, ring, view }
}

const dressIsland = (parts, island) => {
  parts.doors.forEach((door, index) => door.material.emissive.setHex(index < island.busyBooths ? GLOW.door : 0x000000))
  parts.lock.position.y = LOCK_Y[island.lock] + 0.55
  if (island.beacon === 'off') {
    parts.beacon.material.color.setHex(WORLD.unlit)
    parts.beacon.material.emissive.setHex(0x000000)
  } else {
    glow(parts.beacon, GLOW[island.beacon])
  }
  if (island.lantern) glow(parts.lantern, GLOW.lantern)
}

export const createWorld = (canvas, island) => {
  const renderer = new THREE.WebGLRenderer({ canvas, antialias: true })
  renderer.outputColorSpace = THREE.LinearSRGBColorSpace
  renderer.setPixelRatio(Math.min(devicePixelRatio || 1, 2))
  const scene = new THREE.Scene()
  scene.add(new THREE.AmbientLight(0xffffff, 0.62 * LIGHT_SCALE))
  const sun = new THREE.DirectionalLight(0xffffff, 0.62 * LIGHT_SCALE)
  sun.position.set(0.35, 1, 0.8)
  scene.add(sun)
  const land = new THREE.Group()
  scene.add(land)
  buildGround(land, island.layout)
  const parts = {
    doors: buildMainline(land, island.layout),
    lantern: buildMezzanine(land),
    ...buildWaterfront(land),
  }
  dressIsland(parts, island)
  if (island.idle) land.traverse(mesh => mesh.material?.color.lerp(new THREE.Color(SKIES.clear), 0.55))
  const parcels = new Map(island.parcels.map(view => [view.identifier, buildParcel(scene, view)]))
  const raycaster = new THREE.Raycaster()
  const bodies = [...parcels.values()].map(parcel => parcel.body)

  return {
    parcels,
    setSky(kind) {
      renderer.setClearColor(SKIES[kind])
    },
    resize(width, height) {
      renderer.setSize(width, height, false)
    },
    select(identifier) {
      for (const parcel of parcels.values()) parcel.ring.visible = parcel.view.identifier === identifier
    },
    pick(ndc, camera) {
      raycaster.setFromCamera(ndc, camera)
      return raycaster.intersectObjects(bodies)[0]?.object.userData.identifier ?? null
    },
    animate(time, reduced) {
      for (const parcel of parcels.values()) {
        const { mark, group } = parcel
        mark.position.y = reduced ? MARK_Y : MARK_Y + Math.sin(time / 500 + group.position.x) * 0.08
        mark.rotation.y = reduced ? 0 : time / 1400
      }
    },
    render(camera) {
      renderer.render(scene, camera)
    },
  }
}
