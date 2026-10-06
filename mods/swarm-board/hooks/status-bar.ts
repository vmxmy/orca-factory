export type Segment = { text: string; color?: string }
export type StatusBarModel = {
  inFlight: number; max: number; failed: number; queue: number; prs: number; doneToday: number; costUsd?: number
  quota?: { remaining: number; limit: number; resetAt?: string }
  driver?: string; globalPauseSec?: number
  view?: string; detail?: boolean; automation?: boolean; automationKnown?: boolean; refreshing?: boolean
  at?: string; refreshSec?: number; snapshotMin?: number; version?: string; stale?: boolean
  restart?: boolean; installedVersion?: string; lastRun?: string; repo?: string; complexity?: number
  linearUsage?: { known: boolean; total: number; bySource: Record<string, number>; board: number }
}

// The plugin host has no Bun global. Count terminal columns without a runtime dependency.
const graphemes = new Intl.Segmenter(undefined, { granularity: 'grapheme' })
const width = (text: string): number => {
  let columns = 0
  for (const { segment } of graphemes.segment(text)) {
    if (/^[\p{Mark}\p{Control}\p{Format}]+$/u.test(segment)) continue
    const cp = segment.codePointAt(0) ?? 0
    columns += /\p{Emoji_Presentation}/u.test(segment) || segment.includes('\uFE0F')
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

export function statusBarRows(model: StatusBarModel, cols: number): Segment[][] {
  cols = Number.isFinite(cols) ? Math.max(0, Math.floor(cols)) : 100
  const n = (value = 0) => Number.isFinite(value) ? Math.max(0, value) : 0
  const part = (text: string, color?: string): Segment => ({ text: text.replace(/[\p{Control}\p{Line_Separator}\p{Paragraph_Separator}]/gu, ' '), ...(color ? { color } : {}) })
  const pipeline = [part(`● 在做 ${n(model.inFlight)}/${n(model.max)}`)]
  if (model.globalPauseSec !== undefined) pipeline.push(part(`⏸ 全局暂停 ${n(model.globalPauseSec)}s`, 'yellow'))
  else if (model.failed > 0) pipeline.push(part(`▲ 需要人 ${n(model.failed)}`, 'yellow'))
  pipeline.push(part(`· 待领 ${n(model.queue)}`), part(`◐ PR ${n(model.prs)}`), part(`✓ 今日完成 ${n(model.doneToday)}`))
  if ((cols >= 140 || model.detail) && model.costUsd !== undefined) pipeline.push(part(`est. $${n(model.costUsd).toFixed(2)}`))
  const { remaining, limit, resetAt } = model.quota ?? {}
  const ratio = remaining !== undefined && limit && limit > 0 ? remaining / limit : 1
  const reset = resetAt === undefined ? undefined : new Date(resetAt)
  const resources: Segment[] = []
  if (model.quota) {
    resources.push(part(`剩余 ${remaining}/${limit}`, ratio < 0.1 ? 'red' : ratio < 0.2 ? 'yellow' : undefined))
    if (reset) resources.push(part(`重置 ${Number.isFinite(reset.getTime()) ? reset.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit', hour12: false }) : '未知'}`))
  }
  resources.push(part(`驱动器 ${model.driver === 'ok' ? '在线' : model.driver === 'stale' ? '状态过期' : '未知'}`, model.driver === 'stale' ? 'yellow' : undefined))
  if (model.detail) {
    if (model.repo) resources.push(part(`仓库 ${model.repo}`))
    if (model.complexity !== undefined) resources.push(part(`复杂度 ${n(model.complexity)}`))
    if (model.linearUsage) {
      resources.push(part(model.linearUsage.known ? `本小时 ${n(model.linearUsage.total)}` : 'Linear 用量未知'))
      for (const [source, count] of Object.entries(model.linearUsage.bySource)) resources.push(part(`${source} ${n(count)}`))
      resources.push(part(`看板 ${n(model.linearUsage.board)}`))
    }
  }
  const views = Object.entries({ swarm: '总览', task: '泳道', graph: '依赖', throughput: '吞吐', failures: '失败' })
    .map(([key, label]) => part((model.view ?? 'task') === key ? `[${label}]` : label))
  const actions = [part(model.detail ? '[明细]' : '明细'),
    part(`定时 ${model.automation === undefined ? model.automationKnown ? '未安装' : '?' : model.automation ? '开' : '关'}`),
    part(model.refreshing ? '刷新中…' : '刷新')]
  const footer = [part([model.at ? `更新于 ${model.at}` : '加载中…', `下次刷新 ${n(model.refreshSec)}s`,
    `快照 ${model.snapshotMin === undefined ? '未知' : n(model.snapshotMin) + ' 分钟前'}`, `v${model.version ?? '?'}`,
    model.stale ? '数据已过期' : '', model.restart ? `已安装 v${model.installedVersion ?? '?'}，请重启` : '', model.lastRun ?? '']
    .filter(Boolean).join(' · '), model.stale || model.restart ? 'yellow' : undefined)]
  if (cols < 140) return [truncate(pipeline, cols), truncate(resources, cols), align(views, actions, cols), truncate(footer, cols)]
  const footerWidth = rowWidth(truncate(footer, Math.floor(cols / 2)))
  return [align(pipeline, resources, cols), align(align(views, actions, cols - footerWidth - 2), footer, cols)]
}
