export type Segment = { text: string; color?: string }
export type StatusBarModel = {
  inFlight: number; max: number; failed: number; queue: number; prs: number; doneToday: number; costUsd?: number
  driver?: 'ok' | 'stale' | 'unknown' | 'none'; driverRequests?: number; complexity?: number
  // Per-repo limit phrases (请求剩余 … · 重置 …) rendered verbatim when the driver is live.
  driverBadge?: string
  globalPauseSec?: number
  quota?: { remaining: number; limit: number; resetAt?: string }
  linearUsage?: { known: boolean; total: number; bySource: Record<string, number>; board: number }
  linearLimit?: number
  view?: string; detail?: boolean; automation?: boolean; automationKnown?: boolean; refreshing?: boolean
  at?: string; refreshSec?: number; snapshotMin?: number; version?: string; stale?: boolean
  restart?: boolean; installedVersion?: string; lastRun?: string
}

// The plugin host has no Bun global. Count terminal columns without a runtime dependency.
const graphemes = new Intl.Segmenter(undefined, { granularity: 'grapheme' })
const width = (text: string): number => {
  let columns = 0
  for (const { segment } of graphemes.segment(text)) {
    if (/^[\p{Mark}\p{Control}\p{Format}]+$/u.test(segment)) continue
    const cp = segment.codePointAt(0) ?? 0
    columns += /\p{Emoji_Presentation}/u.test(segment) || segment.includes('️')
      || cp >= 0x1100 && (cp <= 0x115f || cp === 0x2329 || cp === 0x232a
        || cp >= 0x2e80 && cp <= 0xa4cf && cp !== 0x303f
        || cp >= 0xac00 && cp <= 0xd7a3 || cp >= 0xf900 && cp <= 0xfaff
        || cp >= 0xfe10 && cp <= 0xfe19 || cp >= 0xfe30 && cp <= 0xfe6f
        || cp >= 0xff01 && cp <= 0xff60 || cp >= 0xffe0 && cp <= 0xffe6
        || cp >= 0x20000 && cp <= 0x3fffd) ? 2 : 1
  }
  return columns
}
const rowWidth = (row: Segment[]) => width(row.map(segment => segment.text).join(' '))
const truncate = (row: Segment[], cols: number): Segment[] => {
  const result: Segment[] = []
  let used = 0
  for (const segment of row) {
    const gap = result.length ? 1 : 0
    const room = cols - used - gap
    if (room <= 0) break
    let text = segment.text
    if (width(text) > room) {
      text = ''
      for (const part of graphemes.segment(segment.text)) {
        if (width(text + part.segment) > room - 1) break
        text += part.segment
      }
      result.push({ ...segment, text: text + '…' })
      break
    }
    result.push(segment)
    used += width(text) + gap
  }
  return result
}
const align = (left: Segment[], right: Segment[], cols: number): Segment[] => {
  const r = truncate(right, Math.floor(cols / 2))
  const l = truncate(left, Math.max(0, cols - rowWidth(r) - 2))
  return truncate([...l, { text: ' '.repeat(Math.max(0, cols - rowWidth(l) - rowWidth(r) - 2)) }, ...r], cols)
}

// Each segment carries one complete phrase so a single Text node still matches board assertions.
export function statusBarRows(model: StatusBarModel, cols: number): Segment[][] {
  cols = Number.isFinite(cols) ? Math.max(0, Math.floor(cols)) : 100
  const n = (value = 0) => Number.isFinite(value) ? Math.max(0, value) : 0
  const part = (text: string, color?: string): Segment => ({ text: text.replace(/[\p{Control}\p{Line_Separator}\p{Paragraph_Separator}]/gu, ' '), ...(color ? { color } : {}) })

  // Pipeline group: liveness first, then volume counters.
  const health = model.globalPauseSec !== undefined ? part(`⏸ 全局暂停 ${n(model.globalPauseSec)} 秒`, 'yellow')
    : model.failed === 0 ? part('🟢 需要人 0')
      : model.failed < 3 ? part(`🟡 需要人 ${n(model.failed)}`) : part(`🔴 需要人 ${n(model.failed)}`, 'yellow')
  const pipeline = [part(`● 在做 ${n(model.inFlight)}/${n(model.max)}`), health,
    part(`· 待领 ${n(model.queue)}`), part(`◐ PR ${n(model.prs)}`), part(`✓ 今日完成 ${n(model.doneToday)}`)]
  if (model.costUsd !== undefined && model.costUsd > 0) pipeline.push(part(`今日按 API 价折算 $${n(model.costUsd).toFixed(2)}`))

  // Resources group: driver quota when live, the counter-file estimate otherwise.
  const resources: Segment[] = []
  const limit = model.linearLimit ?? 2500
  const badge = (color?: string) => model.linearUsage
    ? part(model.linearUsage.known ? `Linear 本小时 ${n(model.linearUsage.total)}/${limit}` : 'Linear 用量未知', color)
    : undefined
  if (model.driver === 'ok') {
    resources.push(part(`驱动器 Linear 本小时 请求 ${n(model.driverRequests)} · 复杂度 ${n(model.complexity)}`))
    if (model.driverBadge) resources.push(part(model.driverBadge))
    if (model.quota) {
      const ratio = model.quota.limit > 0 ? model.quota.remaining / model.quota.limit : 1
      resources.push(part(`剩余 ${n(model.quota.remaining)}/${n(model.quota.limit)}`, ratio < 0.1 ? 'red' : ratio < 0.2 ? 'yellow' : undefined))
    }
  } else {
    if (model.driver === 'stale') resources.push(part('驱动器 Linear 状态过期', 'yellow'))
    if (model.driver === 'unknown') resources.push(part('驱动器 Linear 未知'))
    const color = model.linearUsage?.known && model.linearUsage.total >= limit * 0.9 ? 'red' : model.linearUsage?.known && model.linearUsage.total >= limit * 0.7 ? 'yellow' : undefined
    const linear = badge(color)
    if (linear) resources.push(linear)
  }
  if (model.detail) {
    if (model.quota?.resetAt) {
      const reset = new Date(model.quota.resetAt)
      resources.push(part(`重置 ${Number.isFinite(reset.getTime()) ? reset.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit', hour12: false }) : '未知'}`))
    }
    if (model.complexity !== undefined && model.driver === 'ok') resources.push(part(`复杂度剩余 ${n(model.complexity)}`))
    if (model.linearUsage) {
      resources.push(part(`本小时 ${n(model.linearUsage.total)}`))
      for (const [source, count] of Object.entries(model.linearUsage.bySource)) resources.push(part(`${source} ${n(count)}`))
      resources.push(part(`看板 ${n(model.linearUsage.board)}`))
    }
  }

  // Controls group: view switch, detail toggle, automation, refresh.
  const views = Object.entries({ swarm: '总览', task: '泳道', graph: '依赖', throughput: '吞吐', failures: '失败' })
    .map(([key, label]) => part((model.view ?? 'task') === key ? `[${label}]` : label))
  const automation = model.automation === undefined
    ? part(model.automationKnown ? '定时：未安装' : '定时：?')
    : part(model.automation ? '● 定时 开' : '○ 定时 关')
  const controls = [...views, part(model.detail ? '[明细]' : '明细'), automation, part(model.refreshing ? '刷新中…' : '刷新')]

  // Freshness group: kept as separate segments so wrapping never cuts a phrase.
  const freshness = model.driver === 'ok' ? '数据来自驱动器实时状态'
    : model.snapshotMin !== undefined ? `数据来自快照 ${n(model.snapshotMin)} 分钟前` : '快照缺失，Linear 读取已节流'
  const footer = [
    part(model.at ? `更新于 ${model.at}` : '加载中…'),
    part(`下次刷新 ${n(model.refreshSec)}s`),
    part(freshness),
    part(`v${model.version ?? '?'}`),
    ...(model.restart ? [part(`→ 已安装 v${model.installedVersion ?? '?'}，请重启`, 'yellow')] : []),
    ...(model.stale && !model.restart ? [part('数据已过期', 'yellow')] : []),
    ...(model.lastRun ? [part(model.lastRun)] : []),
  ]
  if (cols < 140) return [truncate(pipeline, cols), resources, align(controls, [], cols), footer]
  return [align(pipeline, resources, cols), align(controls, footer, cols)]
}
