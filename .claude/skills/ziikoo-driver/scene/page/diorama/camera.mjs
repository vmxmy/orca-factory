import * as THREE from 'three'

const ROOT2 = Math.SQRT2
const ROOT6 = Math.sqrt(6)
const DIRECTION = new THREE.Vector3(1, 1, 1).normalize()
const DISTANCE = 200
const NEIGHBORHOOD_MARGIN = 0.18
const MIN_HALF = 3.4
const ZOOM_OUT_LIMIT = 1.25
const FIT_PADDING = 1.04
const BOUNDS_MARGIN = 8
const EASE = 0.14

const screenOf = point => ({
  sx: (point.x - point.z) / ROOT2,
  sy: (2 * point.y - point.x - point.z) / ROOT6,
})

const groundAt = (sx, sy) => ({
  x: (sx * ROOT2 - sy * ROOT6) / 2,
  z: (-sx * ROOT2 - sy * ROOT6) / 2,
})

const corners = ({ left, right, back, front }) =>
  [left, right].flatMap(x => [back, front].flatMap(z => [-1.4, 0].map(y => ({ x, y, z }))))

const extremesOf = layout => [
  ...[layout.ground, layout.water, layout.yard].filter(Boolean).flatMap(corners),
  ...layout.booths.map(({ x, z }) => ({ x, y: 1.7, z: z - 0.75 })),
  { x: layout.tower - 0.95, y: 2.4, z: -0.95 },
  { x: layout.tower + 0.95, y: 2.4, z: 0.95 },
  { x: -1, y: 5.2, z: -6.3 },
  { x: 4.2, y: 4.3, z: -6.6 },
  { x: 16.2, y: 3.8, z: 6.6 },
]

const HUB_TOP = 6

const hubBox = hub => ({ left: hub.x - hub.half, right: hub.x + hub.half, back: hub.z - hub.half, front: hub.z + hub.half })

const islandBox = ({ layout, origin }) => ({
  left: origin.x + layout.ground.left,
  right: origin.x + layout.water.right,
  back: origin.z + Math.min(layout.water.back, layout.yard?.back ?? Infinity),
  front: origin.z + layout.water.front,
})

const boundsOf = ({ islands, hub }) => {
  const boxes = [...islands.map(islandBox), ...(hub ? [hubBox(hub)] : [])]
  return {
    left: Math.min(...boxes.map(box => box.left)) - BOUNDS_MARGIN,
    right: Math.max(...boxes.map(box => box.right)) + BOUNDS_MARGIN,
    back: Math.min(...boxes.map(box => box.back)) - BOUNDS_MARGIN,
    front: Math.max(...boxes.map(box => box.front)) + BOUNDS_MARGIN,
  }
}

const islandExtremes = ({ layout, origin }) => extremesOf(layout).map(point => ({ ...point, x: point.x + origin.x, z: point.z + origin.z }))

const hubExtremes = hub => [...corners(hubBox(hub)), { x: hub.x, y: HUB_TOP, z: hub.z - 1 }]

const extentsOf = archipelago => {
  const islands = archipelago.islands.map(islandExtremes)
  return {
    islands,
    everything: [...islands.flat(), ...(archipelago.hub ? hubExtremes(archipelago.hub) : [])],
    bounds: boundsOf(archipelago),
  }
}

export const createCamera = archipelago => {
  const camera = new THREE.OrthographicCamera(-1, 1, 1, -1, 0.1, 600)
  camera.position.copy(DIRECTION).multiplyScalar(DISTANCE)
  camera.lookAt(0, 0, 0)
  let { islands, everything, bounds } = extentsOf(archipelago)
  let viewport = { width: 1, height: 1, margins: { top: 0, right: 0, bottom: 0, left: 0 } }
  let farthest = 10
  const view = { half: farthest, x: 0, z: 0 }
  const goal = { ...view }

  const clamp = () => {
    goal.half = Math.min(farthest, Math.max(MIN_HALF, goal.half))
    goal.x = Math.min(bounds.right, Math.max(bounds.left, goal.x))
    goal.z = Math.min(bounds.front, Math.max(bounds.back, goal.z))
  }

  const targetFor = ({ sx, sy }, half, margins) => {
    const { height } = viewport
    const perUnit = height / (2 * half)
    const offsetX = (margins.left - margins.right) / 2
    const offsetY = (margins.top - margins.bottom) / 2
    return groundAt(sx - offsetX / perUnit, sy + offsetY / perUnit)
  }

  const fit = (extremes, margins = viewport.margins) => {
    const points = extremes.map(screenOf)
    const xs = points.map(point => point.sx)
    const ys = points.map(point => point.sy)
    const { width, height } = viewport
    const usableWidth = Math.max(1, width - margins.left - margins.right)
    const usableHeight = Math.max(1, height - margins.top - margins.bottom)
    const spanX = Math.max(...xs) - Math.min(...xs)
    const spanY = Math.max(...ys) - Math.min(...ys)
    const half = FIT_PADDING * Math.max((spanY * height) / (2 * usableHeight), (spanX * height) / (2 * usableWidth))
    const center = { sx: (Math.max(...xs) + Math.min(...xs)) / 2, sy: (Math.max(...ys) + Math.min(...ys)) / 2 }
    return { half, ...targetFor(center, half, margins) }
  }

  const panScreen = (sx, sy) => {
    const delta = groundAt(sx, sy)
    goal.x += delta.x
    goal.z += delta.z
    clamp()
  }

  return {
    camera,
    view,
    goal,
    resize(width, height, margins) {
      if (width <= 0 || height <= 0) return
      viewport = { width, height, margins }
      farthest = fit(everything).half * ZOOM_OUT_LIMIT
    },
    retarget(next) {
      ;({ islands, everything, bounds } = extentsOf(next))
      farthest = fit(everything).half * ZOOM_OUT_LIMIT
      clamp()
    },
    frameAll() {
      Object.assign(goal, fit(everything))
      clamp()
    },
    frameIsland(index) {
      Object.assign(goal, fit(islands[index]))
      clamp()
    },
    frameNeighborhood(points) {
      const { width, height, margins } = viewport
      const horizontal = (width - margins.left - margins.right) * NEIGHBORHOOD_MARGIN
      const vertical = (height - margins.top - margins.bottom) * NEIGHBORHOOD_MARGIN
      Object.assign(goal, fit(points, {
        left: Math.max(width * NEIGHBORHOOD_MARGIN, margins.left + horizontal),
        right: Math.max(width * NEIGHBORHOOD_MARGIN, margins.right + horizontal),
        top: Math.max(height * NEIGHBORHOOD_MARGIN, margins.top + vertical),
        bottom: Math.max(height * NEIGHBORHOOD_MARGIN, margins.bottom + vertical),
      }))
      farthest = Math.max(farthest, goal.half)
      clamp()
    },
    drag(dxPixels, dyPixels) {
      const unitsPerPixel = (2 * view.half) / viewport.height
      panScreen(-dxPixels * unitsPerPixel, dyPixels * unitsPerPixel)
      view.x = goal.x
      view.z = goal.z
    },
    nudge(right, up) {
      const step = view.half * 0.12
      panScreen(right * step, up * step)
    },
    zoom(factor) {
      goal.half *= factor
      clamp()
    },
    snap() {
      Object.assign(view, goal)
    },
    step(reduced) {
      const ease = reduced ? 1 : EASE
      view.half += (goal.half - view.half) * ease
      view.x += (goal.x - view.x) * ease
      view.z += (goal.z - view.z) * ease
      const aspect = viewport.width / viewport.height
      camera.left = -view.half * aspect
      camera.right = view.half * aspect
      camera.top = view.half
      camera.bottom = -view.half
      camera.position.set(view.x, 0, view.z).addScaledVector(DIRECTION, DISTANCE)
      camera.updateProjectionMatrix()
      camera.updateMatrixWorld()
    },
  }
}
