type Segment = { text: string; color?: string }
type Model = {
  inFlight?: number; max?: number; failed?: number; queue?: number; prs?: number; doneToday?: number; costUsd?: number
  quota?: { remaining?: number; limit?: number; resetAt?: string | number }
  driver?: 'ok' | 'unknown' | 'stale'; at?: string; refreshSec?: number; snapshotMin?: number; version?: string; view?: string
  globalPause?: { active?: boolean; durationSeconds?: number }; detail?: boolean; repo?: string; complexity?: number
  linearUsage?: { known?: boolean; total?: number; bySource?: Record<string, number>; board?: number }
  lastRun?: string; stale?: boolean; needsRestart?: boolean; installedVersion?: string
  automation?: boolean; automationKnown?: boolean; refreshing?: boolean
}

// Host-independent terminal widths: keep grapheme clusters intact, including emoji sequences.
const graphemes = new Intl.Segmenter(undefined, { granularity: 'grapheme' })
const width = (text: string): number => {
  let result = 0
  for (const { segment } of graphemes.segment(text)) {
    if (/^[\p{Mark}\p{Control}\p{Format}]+$/u.test(segment)) continue
    const cp = segment.codePointAt(0) ?? 0
    result += /\p{Emoji_Presentation}/u.test(segment) || segment.includes('\uFE0F')
      || cp >= 0x1100 && (cp <= 0x115f || cp === 0x2329 || cp === 0x232a
        || cp >= 0x2e80 && cp <= 0xa4cf && cp !== 0x303f
        || cp >= 0xac00 && cp <= 0xd7a3 || cp >= 0xf900 && cp <= 0xfaff
        || cp >= 0xfe10 && cp <= 0xfe19 || cp >= 0xfe30 && cp <= 0xfe6f
        || cp >= 0xff01 && cp <= 0xff60 || cp >= 0xffe0 && cp <= 0xffe6
        || cp >= 0x20000 && cp <= 0x3fffd) ? 2 : 1
  }
  return result
}
const clean = (text: string) => text.replace(/[\p{Control}\p{Line_Separator}\p{Paragraph_Separator}]/gu, ' ')
const rowWidth = (row: Segment[]) => width(row.map(s => s.text).join(' '))
const truncate = (row: Segment[], cols: number): Segment[] => {
  if (rowWidth(row) <= cols) return row
  const result: Segment[] = []
  let used = 0
  for (const part of row) {
    const room = cols - used - (result.length ? 1 : 0)
    if (room <= 0) break
    if (width(part.text) <= room) {
      result.push(part)
      used += width(part.text) + (result.length > 1 ? 1 : 0)
      continue
    }
    let text = ''
    for (const { segment } of graphemes.segment(part.text)) {
      if (width(text + segment) > room - 1) break
      text += segment
    }
    result.push({ ...part, text: text + '…' })
    break
  }
  return result
}
const align = (left: Segment[], right: Segment[], cols: number): Segment[] => {
  const r = truncate(right, Math.floor(cols / 2))
  const l = truncate(left, cols - rowWidth(r) - 2)
  return [...l, { text: ' '.repeat(Math.max(0, cols - rowWidth(l) - rowWidth(r) - 2)) }, ...r]
}

export function statusBarRows(model: Model = {}, cols = 100): Segment[][] {
  cols = Number.isFinite(cols) ? Math.max(0, Math.floor(cols)) : 100
  const number = (value?: number) => Number.isFinite(value) ? Math.max(0, value!) : 0
  const part = (text: string, color?: string): Segment => ({ text: clean(text), ...(color ? { color } : {}) })
  const pipeline = [part(`在做 ${number(model.inFlight)}/${number(model.max)}`),
    model.globalPause?.active ? part(`⏸ 全局暂停 ${number(model.globalPause.durationSeconds)} 秒`, 'yellow')
      : part(`需要人 ${number(model.failed)}`, number(model.failed) > 0 ? 'yellow' : undefined),
    part(`待领 ${number(model.queue)}`), part(`PR ${number(model.prs)}`), part(`今日完成 ${number(model.doneToday)}`)]
  if (cols >= 140 || model.detail) pipeline.push(part(`今日按 API 价折算 $${number(model.costUsd).toFixed(2)}`))
  const remaining = model.quota?.remaining, limit = model.quota?.limit
  const ratio = limit && remaining !== undefined ? remaining / limit : 1
  const reset = model.quota?.resetAt === undefined ? undefined : new Date(model.quota.resetAt)
  const resources = [part(`Linear 剩余 ${remaining ?? '?'}/${limit ?? '?'}`, ratio < 0.1 ? 'red' : ratio < 0.2 ? 'yellow' : undefined),
    part(`重置 ${reset && Number.isFinite(reset.getTime()) ? reset.toLocaleTimeString() : '未知'}`),
    part(`驱动器 ${model.driver === 'ok' ? '在线' : model.driver === 'stale' ? '状态过期' : '未知'}`, model.driver === 'stale' ? 'yellow' : undefined)]
  if (model.detail) {
    if (model.repo) resources.push(part(`仓库 ${model.repo}`))
    if (model.complexity !== undefined) resources.push(part(`复杂度 ${number(model.complexity)}`))
    if (model.linearUsage) {
      resources.push(part(model.linearUsage.known ? `本小时 ${number(model.linearUsage.total)}` : 'Linear 用量未知'))
      for (const [source, count] of Object.entries(model.linearUsage.bySource ?? {})) resources.push(part(`${source} ${number(count)}`))
      resources.push(part(`看板 ${number(model.linearUsage.board)}`))
    }
  }
  const views: Record<string, string> = { swarm: '总览', task: '泳道', graph: '依赖', throughput: '吞吐', failures: '失败' }
  const controls = [part(`视图 [${views[model.view ?? 'task'] ?? model.view}]`), part(`明细 ${model.detail ? '开' : '关'}`),
    part(`定时 ${model.automation === undefined ? model.automationKnown ? '未安装' : '?' : model.automation ? '开' : '关'}`),
    part(model.refreshing ? '刷新中…' : '刷新 [r]')]
  const footer = [part(model.at ? `更新于 ${model.at}` : '加载中…'),
    part(`下次刷新 ${number(model.refreshSec)}s`), part(`快照 ${model.snapshotMin === undefined ? '未知' : number(model.snapshotMin) + ' 分钟前'}`),
    part(`v${model.version ?? '?'}`)]
  if (model.lastRun) footer.push(part(model.lastRun))
  if (model.stale) footer.push(part('数据已过期'))
  if (model.needsRestart) footer.push(part(`已安装 v${model.installedVersion ?? '?'}，请重启`))
  if (model.stale || model.needsRestart) for (const segment of footer) segment.color = 'yellow'
  return cols < 140 ? [pipeline, resources, controls, footer].map(row => truncate(row, cols))
    : [align(pipeline, resources, cols), align(controls, footer, cols)]
}
