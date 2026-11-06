const colors = { ink: '#5c3e30', wood: '#885b3d', light: '#f7e7be', roof: '#b96243', grass: '#a8b77a', current: '#568c9b', attention: '#c29130', failed: '#b44e42', done: '#5e8358' }
const rect = (context, x, y, width, height, color) => { context.fillStyle = color; context.fillRect(Math.round(x), Math.round(y), width, height) }
const line = (context, points, color, width = 3) => {
  context.strokeStyle = color
  context.lineWidth = width
  context.lineJoin = 'miter'
  context.beginPath()
  points.forEach((point, index) => index ? context.lineTo(point.x, point.y) : context.moveTo(point.x, point.y))
  context.stroke()
}
const circle = (context, x, y, radius, color) => { context.fillStyle = color; context.beginPath(); context.arc(x, y, radius, 0, Math.PI * 2); context.fill() }
export function text(context, value, x, y, maxWidth = 220, color = colors.ink, size = 13) {
  context.font = `${size}px ui-monospace, "PingFang SC", monospace`
  context.textAlign = 'center'
  context.fillStyle = color
  let visible = String(value)
  if (context.measureText(visible).width > maxWidth) {
    while (visible.length && context.measureText(`${visible}…`).width > maxWidth) visible = visible.slice(0, -1)
    visible += '…'
  }
  context.fillText(visible, x, y)
}

function tree(context, x, y) {
  rect(context, x - 4, y, 8, 25, colors.wood)
  rect(context, x - 19, y - 20, 38, 30, '#708c55')
  rect(context, x - 12, y - 30, 24, 12, '#8ba464')
  rect(context, x - 23, y - 9, 8, 15, '#708c55')
  rect(context, x + 8, y - 19, 8, 9, '#a8ba76')
}

function house(context, project) {
  const { x, y, row } = project
  rect(context, x - 114, y + 48, 228, 12, '#849765')
  rect(context, x - 90, y - 50, 180, 105, colors.wood)
  rect(context, x - 84, y - 47, 168, 96, colors.light)
  for (const beam of [-80, -12, 78]) rect(context, x + beam, y - 42, 6, 92, colors.wood)
  rect(context, x - 84, y + 6, 168, 5, colors.wood)
  for (let tier = 0; tier < 5; tier++) rect(context, x - 108 + tier * 16, y - 57 - tier * 9, 216 - tier * 32, 11, colors.roof)
  rect(context, x + 47, y - 101, 18, 32, colors.wood)
  rect(context, x + 43, y - 103, 26, 7, '#e0c397')
  for (const windowX of [-58, 37]) {
    rect(context, x + windowX, y - 25, 27, 27, colors.wood)
    rect(context, x + windowX + 4, y - 21, 19, 19, '#b8d6c7')
    rect(context, x + windowX + 13, y - 21, 3, 19, colors.wood)
  }
  rect(context, x - 4, y + 13, 31, 40, '#74503a')
  rect(context, x + 18, y + 34, 4, 4, '#dec27e')
  rect(context, x - 90, y + 65, 180, 24, '#f4e3b9')
  text(context, row.name, x, y + 82, 168, colors.ink, 14)
  text(context, row.path, x, y + 104, 250, '#665944', 10)
  text(context, `${row.parcels.length} 张票 · ${row.deploy ? '含部署' : '部署不适用'}`, x, y + 122, 250, colors.ink, 11)
  tree(context, x - 113, y + 5)
  for (const offset of [-98, 96]) {
    rect(context, x + offset, y + 39, 10, 10, '#c17c47')
    rect(context, x + offset - 2, y + 34, 14, 6, '#cfa76c')
  }
}

function belt(context, points) {
  line(context, points, '#775b43', 23)
  line(context, points, '#d6c095', 17)
  for (let index = 1; index < points.length; index++) {
    const from = points[index - 1]
    const to = points[index]
    const length = Math.hypot(to.x - from.x, to.y - from.y)
    for (let step = 0; step < length; step += 18) {
      const x = from.x + (to.x - from.x) * step / length
      const y = from.y + (to.y - from.y) * step / length
      rect(context, x - 2, y - 5, 4, 10, '#9d8560')
    }
  }
}

function device(context, node) {
  const { station, x, y } = node
  context.save()
  context.translate(x, y)
  if (station === 'pickup') {
    rect(context, -54, -44, 65, 57, colors.wood)
    rect(context, -48, -38, 53, 43, '#d9c297')
    for (const paperX of [-42, -19]) rect(context, paperX, -31, 17, 25, '#fff3ce')
    for (const post of [-50, 3]) rect(context, post, 13, 6, 31, colors.wood)
    for (const shelf of [8, 30]) {
      rect(context, 19, shelf, 40, 5, colors.wood)
      for (const boxX of [23, 43]) rect(context, boxX, shelf - 17, 14, 17, '#c8975a')
    }
  } else if (station === 'work') {
    rect(context, -55, -42, 61, 77, '#7c7a68')
    for (let tier = 0; tier < 5; tier++) for (let brick = 0; brick < 3; brick++) rect(context, -52 + brick * 20 + tier % 2 * 3, -39 + tier * 14, 17, 11, '#a7a08a')
    rect(context, -42, -14, 33, 42, '#593e32')
    rect(context, -37, 9, 23, 15, '#e0a451')
    rect(context, -39, -65, 28, 23, '#8a816c')
    rect(context, 16, 22, 44, 11, '#696e68')
    rect(context, 23, 10, 26, 13, '#747d76')
    rect(context, 32, 33, 11, 14, colors.wood)
    line(context, [{ x: 9, y: 5 }, { x: 36, y: -7 }, { x: 56, y: 4 }, { x: 9, y: 5 }], '#9d754d', 7)
  } else if (station === 'gate') {
    rect(context, -53, -60, 18, 106, '#788a7b')
    rect(context, 35, -60, 18, 106, '#788a7b')
    rect(context, -57, -63, 114, 18, '#5e756d')
    rect(context, -34, -19, 68, 5, '#b6d1b4')
    rect(context, 40, 6, 8, 14, '#d6c38d')
  } else if (station === 'review') {
    line(context, [{ x: -56, y: 3 }, { x: 25, y: -18 }, { x: 43, y: 12 }, { x: -41, y: 31 }, { x: -56, y: 3 }], colors.wood, 6)
    for (const leg of [-42, 27]) rect(context, leg, 20, 7, 28, colors.wood)
    line(context, [{ x: -37, y: 1 }, { x: -7, y: -8 }, { x: 0, y: 0 }, { x: 24, y: -7 }, { x: 31, y: 9 }, { x: 1, y: 15 }, { x: -31, y: 21 }, { x: -37, y: 1 }], '#fff0c9', 7)
    rect(context, 47, -53, 20, 84, colors.wood)
    for (let book = 0; book < 4; book++) rect(context, 50, -47 + book * 18, 13, 12, ['#9c6c51', '#759181', '#bc9d69', '#6e8193'][book])
  } else if (station === 'pack') {
    rect(context, -58, -3, 115, 14, colors.wood)
    for (const leg of [-50, 43]) rect(context, leg, 10, 8, 38, colors.wood)
    rect(context, -18, -31, 38, 28, '#c4975c')
    line(context, [{ x: -18, y: -31 }, { x: -30, y: -44 }, { x: -5, y: -40 }, { x: 0, y: -30 }, { x: 22, y: -43 }, { x: 34, y: -35 }, { x: 20, y: -31 }], '#d9b77b', 6)
    circle(context, -43, -17, 13, '#ead4a6')
    circle(context, -43, -17, 5, colors.wood)
    rect(context, -49, -12, 22, 13, '#ead4a6')
  } else if (station === 'merge') {
    for (const track of [-25, 18]) {
      for (const rail of [-11, 11]) rect(context, -59, track + rail, 108, 4, '#777f73')
      for (let tie = -52; tie < 48; tie += 17) rect(context, tie, track - 16, 6, 36, '#a58b62')
    }
    rect(context, 53, -52, 6, 96, colors.wood)
    rect(context, 44, -58, 25, 40, '#595c50')
    circle(context, 56, -47, 6, '#b85d43')
    circle(context, 56, -30, 6, '#b9ab78')
  } else if (station === 'dispatch') {
    for (const support of [-52, 47]) rect(context, support, -62, 10, 108, colors.wood)
    rect(context, -58, -68, 122, 13, colors.wood)
    line(context, [{ x: -47, y: -54 }, { x: 47, y: -26 }, { x: -47, y: -7 }], '#bca275', 4)
    rect(context, 10, -55, 4, 35, '#61695f')
    rect(context, -14, -20, 50, 37, '#be9058')
    for (const strap of [-8, 23]) rect(context, strap, -20, 5, 37, '#ead8a9')
  } else if (station === 'smoke') {
    circle(context, 0, -3, 40, '#966044')
    rect(context, -35, -29, 70, 60, '#b88352')
    circle(context, 0, -3, 24, '#745c45')
    circle(context, 0, -3, 17, '#b1c4ad')
    line(context, [{ x: -37, y: 5 }, { x: -58, y: 5 }, { x: -58, y: -38 }, { x: -29, y: -38 }], '#b17b4f', 8)
    circle(context, 35, -43, 16, '#6a6956')
    circle(context, 35, -43, 11, '#eee0b9')
    line(context, [{ x: 35, y: -43 }, { x: 40, y: -50 }], colors.ink, 2)
    for (const foot of [-27, 18]) rect(context, foot, 30, 9, 17, '#746449')
  } else if (station === 'repair') {
    for (const post of [-48, 37]) rect(context, post, -26, 9, 71, '#717d70')
    rect(context, -49, -30, 97, 10, '#8b9b83')
    for (const wheelX of [-26, 23]) { circle(context, wheelX, 5, 21, '#626b61'); circle(context, wheelX, 5, 10, '#bcb899') }
    line(context, [{ x: -26, y: 5 }, { x: 23, y: 5 }], '#a49d7f', 6)
    line(context, [{ x: 57, y: -20 }, { x: 45, y: 26 }], '#b59e76', 6)
    line(context, [{ x: 48, y: -28 }, { x: 57, y: -20 }, { x: 66, y: -23 }], '#838b7d', 6)
  } else if (station === 'return') {
    circle(context, 0, 10, 44, '#8f7150')
    circle(context, 0, 10, 34, '#c7af77')
    line(context, [{ x: -24, y: 16 }, { x: 18, y: -4 }, { x: 5, y: -7 }, { x: 18, y: -4 }, { x: 12, y: 9 }], '#8c6645', 5)
    rect(context, -49, -44, 7, 69, colors.wood)
    rect(context, -62, -49, 48, 20, '#d6bd83')
    text(context, '返工', -38, -35, 40, colors.ink, 11)
  } else if (station === 'human') {
    for (const [seatX, seatY] of [[-44, -22], [40, -20], [-35, 34], [32, 35]]) {
      rect(context, seatX - 10, seatY - 8, 21, 19, '#8a6447')
      rect(context, seatX - 11, seatY - 14, 23, 7, '#a9845a')
    }
    circle(context, 0, 1, 36, '#a77b52')
    circle(context, 0, -3, 32, '#d6b581')
    rect(context, -14, -17, 24, 18, '#f6e4bb')
    circle(context, 15, 10, 5, '#eee3be')
  } else {
    rect(context, -41, -36, 82, 77, '#d5b97f')
    text(context, '?', 0, 20, 80, '#a4513d', 52)
  }
  context.restore()
}

export function paintBase(context, state, layout, viewport, model) {
  rect(context, viewport.x, viewport.y, viewport.width, viewport.height, '#b9c78c')
  for (let y = Math.floor(viewport.y / 64) * 64; y < viewport.y + viewport.height; y += 64) for (let x = Math.floor(viewport.x / 64) * 64; x < viewport.x + viewport.width; x += 64) {
    rect(context, x + 9, y + 13, 5, 3, '#9eaf73')
    rect(context, x + 41, y + 38, 3, 6, '#a8b97c')
    rect(context, x + 19, y + 52, 3, 3, '#d8cc8d')
  }
  if (state.view === 'town') {
    rect(context, -39, viewport.y, 78, viewport.height, '#d7ca9b')
    rect(context, -21, viewport.y, 42, viewport.height, '#8fbaba')
    rect(context, -18, viewport.y, 6, viewport.height, '#b1d2c3')
    for (const project of model.visibleProjects(layout, viewport)) {
      rect(context, project.x < 0 ? project.x : 0, project.y + 32, Math.abs(project.x), 29, '#d5be8b')
      rect(context, -39, project.y + 27, 78, 40, '#a67d51')
      for (let plank = -32; plank < 39; plank += 12) rect(context, plank, project.y + 30, 9, 34, '#d8b581')
      house(context, project)
    }
    return
  }
  rect(context, 26, 45, layout.bounds.width - 52, layout.bounds.height - 55, '#e7d4a7')
  for (const beamX of [26, 940]) rect(context, beamX, 38, 13, layout.bounds.height - 40, '#896342')
  for (const beamY of [38, layout.bounds.height - 6]) rect(context, 26, beamY, 927, 11, '#896342')
  const mainline = ['pickup', 'work', 'gate', 'review', 'pack', 'merge', 'dispatch', 'smoke']
  belt(context, mainline.map(station => ({ x: layout.nodes.get(station).x, y: layout.nodes.get(station).y + 85 })))
  for (const branch of ['repair', 'return', 'human', 'unknown']) belt(context, model.travelPath(layout, branch, 'work'))
  for (const node of layout.nodes.values()) {
    if (node.y + 150 < viewport.y || node.y - 100 > viewport.y + viewport.height) continue
    device(context, node)
    const disabled = !layout.row.deploy && (node.station === 'dispatch' || node.station === 'smoke')
    text(context, model.stations[node.station][0], node.x, node.y - 82, 160, colors.ink, 15)
    text(context, disabled ? '不适用 · 未配置部署' : `${node.count} 张票${node.station === 'work' ? ` · ${layout.row.workers} 个执行位` : ''}`, node.x, node.y + 65, 180, '#725c3f', 11)
  }
}

export function paintActors(context, state, layout, viewport, now, model, reduced) {
  const hits = []
  if (state.view === 'town') {
    for (const project of model.visibleProjects(layout, viewport)) {
      const selected = state.project === project.path
      if (selected) {
        context.strokeStyle = '#a9633d'
        context.lineWidth = 3
        context.strokeRect(project.x - 133, project.y - 110, 266, 244)
      }
      const active = project.row.parcels.some(parcel => parcel.tone === 'current')
      const step = reduced || !active ? 0 : Math.floor(now / 420) % 2 * 3
      const personX = project.x + 52 + step
      rect(context, personX, project.y + 32, 10, 11, '#5f8590')
      rect(context, personX + 1, project.y + 26, 8, 7, '#e3bd86')
      rect(context, personX, project.y + 43, 4, 6, colors.wood)
      rect(context, personX + 7, project.y + 43 - step, 4, 6, colors.wood)
      const failed = project.row.parcels.filter(parcel => parcel.tone === 'failed' || parcel.tone === 'attention' || parcel.unknownDeploy || model.stationOf(parcel.station) === 'unknown').length
      if (failed) { circle(context, project.x + 83, project.y - 65, 14, colors.failed); text(context, failed, project.x + 83, project.y - 61, 24, '#fff3d1', 11) }
      hits.push({ kind: 'project', path: project.path, x: project.x - project.width / 2, y: project.y - 110, width: project.width, height: project.height })
    }
    return hits
  }
  for (const node of layout.nodes.values()) hits.push({ kind: 'station', station: node.station, x: node.x - 80, y: node.y - 100, width: 160, height: 166 })
  const work = layout.nodes.get('work')
  if (layout.row.parcels.some(parcel => parcel.station === 'work' && parcel.tone === 'current' && !['paused', 'failed'].includes(parcel.deployment)) && !reduced && state.scene.state === 'ok' && state.scene.maintenance === 'running') {
    rect(context, work.x - 32, work.y - 6, 12, 24, '#e0a451')
    rect(context, work.x - 28, work.y - 14 + Math.floor(now / 180) % 3 * 4, 5, 24, '#f6d17b')
  }
  for (const [key, destination] of layout.parcels) {
    const motion = state.motions.get(key)
    const progress = motion ? (now - motion.start) / motion.duration : 1
    const point = motion && progress < 1 && !reduced ? model.pointAlong(motion.points, progress, motion.dwell ?? 0, motion.dwellAt ?? 0) : destination
    if (progress >= 1) state.motions.delete(key)
    if (point.x + 20 < viewport.x || point.x - 20 > viewport.x + viewport.width || point.y + 25 < viewport.y || point.y - 20 > viewport.y + viewport.height) continue
    const parcel = destination.parcel
    const color = colors[parcel.tone] || colors.attention
    rect(context, point.x - 13, point.y - 13, 26, 26, '#80593e')
    rect(context, point.x - 11, point.y - 11, 22, 22, '#d8ae70')
    rect(context, point.x - 3, point.y - 11, 6, 22, '#f1d6a0')
    rect(context, point.x + 5, point.y - 11, 6, 6, color)
    if (state.selection?.key === key) {
      context.strokeStyle = '#a94d31'
      context.lineWidth = 3
      context.strokeRect(point.x - 17, point.y - 17, 34, 34)
    }
    text(context, parcel.identifier, point.x, point.y + 24, 31, colors.ink, 8)
    hits.push({ kind: 'parcel', key, x: point.x - 17, y: point.y - 17, width: 34, height: 43 })
  }
  return hits
}
