import * as THREE from 'three'

const ROOT2 = Math.SQRT2
const ROOT6 = Math.sqrt(6)
const DIRECTION = new THREE.Vector3(1, 1, 1).normalize()
const DISTANCE = 200
const TICKET_HALF = 4.2
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

export const createCamera = layout => {
  const camera = new THREE.OrthographicCamera(-1, 1, 1, -1, 0.1, 600)
  camera.position.copy(DIRECTION).multiplyScalar(DISTANCE)
  camera.lookAt(0, 0, 0)
  const extremes = extremesOf(layout)
  const bounds = {
    left: layout.ground.left - BOUNDS_MARGIN,
    right: layout.water.right + BOUNDS_MARGIN,
    back: Math.min(layout.water.back, layout.yard?.back ?? Infinity) - BOUNDS_MARGIN,
    front: layout.water.front + BOUNDS_MARGIN,
  }
  let viewport = { width: 1, height: 1, margins: { top: 0, right: 0, bottom: 0, left: 0 } }
  let islandHalf = 10
  const view = { half: islandHalf, x: 0, z: 0 }
  const goal = { ...view }

  const clamp = () => {
    goal.half = Math.min(islandHalf * ZOOM_OUT_LIMIT, Math.max(MIN_HALF, goal.half))
    goal.x = Math.min(bounds.right, Math.max(bounds.left, goal.x))
    goal.z = Math.min(bounds.front, Math.max(bounds.back, goal.z))
  }

  const targetFor = ({ sx, sy }, half) => {
    const { height, margins } = viewport
    const perUnit = height / (2 * half)
    const offsetX = (margins.left - margins.right) / 2
    const offsetY = (margins.top - margins.bottom) / 2
    return groundAt(sx - offsetX / perUnit, sy + offsetY / perUnit)
  }

  const islandFrame = () => {
    const points = extremes.map(screenOf)
    const xs = points.map(point => point.sx)
    const ys = points.map(point => point.sy)
    const { width, height, margins } = viewport
    const usableWidth = Math.max(1, width - margins.left - margins.right)
    const usableHeight = Math.max(1, height - margins.top - margins.bottom)
    const spanX = Math.max(...xs) - Math.min(...xs)
    const spanY = Math.max(...ys) - Math.min(...ys)
    const half = FIT_PADDING * Math.max((spanY * height) / (2 * usableHeight), (spanX * height) / (2 * usableWidth))
    const center = { sx: (Math.max(...xs) + Math.min(...xs)) / 2, sy: (Math.max(...ys) + Math.min(...ys)) / 2 }
    return { half, ...targetFor(center, half) }
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
      islandHalf = islandFrame().half
    },
    frameIsland() {
      Object.assign(goal, islandFrame())
      clamp()
    },
    frameSlot(slot) {
      Object.assign(goal, { half: TICKET_HALF, ...targetFor(screenOf(slot), TICKET_HALF) })
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
