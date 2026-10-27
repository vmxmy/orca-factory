import type { ElementConstructor, ElementTable, InputProps, RenderChildren } from 'claude-code'

import type { Alert, Fetched, Page, Repo, Runtime, RuntimeTicket, Timeline, TrendSample } from '../types'

export type Kit = Pick<ElementTable, 'Box' | 'Text' | 'Button'>

export type InputElement = ElementConstructor<InputProps>

export const cell = (ui: Kit, width: number, value: string, dimColor = false, color?: string) => (
  <ui.Box width={width} flexShrink={0}>
    <ui.Text wrap="truncate" dimColor={dimColor} color={color}>
      {value}
    </ui.Text>
  </ui.Box>
)

// Border plus one column of padding on each side.
export const SECTION_INSET = 4

// East Asian wide characters take two terminal columns.
const WIDE = /[\u1100-\u115f\u2e80-\u303e\u3041-\u33ff\u3400-\u4dbf\u4e00-\u9fff\ua000-\ua4cf\uac00-\ud7a3\uf900-\ufaff\ufe30-\ufe4f\uff00-\uff60\uffe0-\uffe6]/u

export const displayWidth = (text: string) => [...text].reduce((sum, ch) => sum + (WIDE.test(ch) ? 2 : 1), 0)

// Title and corner each sit two columns in from the border's ends; the title is padded by a space on both sides, the corner's chips pad themselves.
export const SECTION_EDGE_FRAME = 6

// A title inside a bordered Box is clipped, so it is laid over the border from the unbordered parent.
export const section = (
  ui: Kit,
  title: string,
  body: RenderChildren,
  note?: string,
  corner?: { key: string; node: RenderChildren },
) => (
  <ui.Box flexDirection="column">
    <ui.Box borderStyle="round" borderDimColor flexDirection="column" paddingX={1}>
      {body}
    </ui.Box>
    <ui.Box position="absolute" top={0} left={2} flexDirection="row">
      <ui.Text>{' '}</ui.Text>
      <ui.Text bold>{title}</ui.Text>
      {note !== undefined && <ui.Text dimColor>{` ${note}`}</ui.Text>}
      <ui.Text>{' '}</ui.Text>
    </ui.Box>
    {corner && (
      <ui.Box key={corner.key} position="absolute" top={0} right={2} flexDirection="row">
        {corner.node}
      </ui.Box>
    )}
  </ui.Box>
)

// Plain labels get the same padding as filled ones so a column of them lines up.
export const chipText = (text: string) => ` ${text} `

export const chip = (ui: Kit, text: string, alert?: Alert) =>
  alert === undefined ? (
    <ui.Text dimColor>{chipText(text)}</ui.Text>
  ) : (
    <ui.Text backgroundColor={alert} color="inverseText">{chipText(text)}</ui.Text>
  )

export const TRACK = ['领', '施工', '门禁', '审查', '合并', '部署']
const SHORT_TRACK = ['领', '施', '门', '审', '合', '部']
const SEGMENT_MIN = 2
const SEGMENT_MAX = 6
// At 4 columns the two-character labels fill each cell and run together.
const FULL_LABEL_SEGMENT = 5

export const ROW = { id: 9, label: 14, since: 12 }

export const segmentWidth = (cols: number): number | null => {
  const room = cols - SECTION_INSET - ROW.id - ROW.label - ROW.since
  const segment = Math.floor(room / TRACK.length)
  return segment < SEGMENT_MIN ? null : Math.min(SEGMENT_MAX, segment)
}

export type Tone = 'current' | 'attention' | 'failed'
export type Position = { at: number | null; tone: Tone; label: string }

export const TONE_COLOR: Record<Tone, string> = { current: 'suggestion', attention: 'warning', failed: 'error' }

export const field = (ui: Kit, label: string, value: string, color?: string) => (
  <ui.Box flexDirection="row">
    {cell(ui, 12, label, true)}
    <ui.Text wrap="truncate" color={color}>
      {value}
    </ui.Text>
  </ui.Box>
)

export const clockTime = (ms: number) => {
  const at = new Date(ms)
  return [at.getHours(), at.getMinutes(), at.getSeconds()].map(n => String(n).padStart(2, '0')).join(':')
}

const PHASE_AT: Record<string, number> = {
  待领: 0,
  'worker 运行中': 1,
  '写 correction': 1,
  分支检查: 2,
  gate: 2,
  审查: 3,
  '开 PR': 4,
  排队合并: 4,
  已合并: 4,
}

const DEPLOY_LABELS: Record<string, string> = { pending: '等部署', 'deploy-intent': '部署中', 'smoke-intent': 'smoke', paused: '部署暂停' }

export const position = (t: RuntimeTicket): Position => {
  const d = t.deployment
  if (d && d.state !== 'passed') return { at: TRACK.length - 1, tone: d.state === 'paused' ? 'failed' : 'current', label: DEPLOY_LABELS[d.state] ?? d.state }
  if (t.stage === 'candidate') return { at: 0, tone: 'current', label: '待领' }
  if (t.stage === 'human') return { at: null, tone: 'attention', label: '转人' }
  if (t.owner === '处置Agent') return { at: null, tone: 'attention', label: '处置 Agent' }
  const at = PHASE_AT[t.phase]
  if (at === undefined) return { at: null, tone: 'attention', label: t.phase || '未知阶段' }
  return { at, tone: t.phase === '写 correction' ? 'attention' : 'current', label: t.phase }
}

// A second unit follows only a one-digit first, so the longest texts, 「9 小时 59 分」 and 「9 天 23 小时」, fit ROW.since.
export const duration = (ms: number) => {
  const seconds = Math.max(0, Math.floor(ms / 1000))
  if (seconds < 60) return `${seconds} 秒`
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `${minutes} 分钟`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return hours >= 10 || minutes % 60 === 0 ? `${hours} 小时` : `${hours} 小时 ${minutes % 60} 分`
  const days = Math.floor(hours / 24)
  return days >= 10 || hours % 24 === 0 ? `${days} 天` : `${days} 天 ${hours % 24} 小时`
}

const reasonOf = (t: RuntimeTicket): { text: string; color?: string } | undefined => {
  if (t.blockedReason) return { text: t.blockedReason, color: 'warning' }
  const d = t.deployment
  if (!d || d.state === 'passed') return undefined
  const text = [d.workflow || '部署', d.state, d.sha.slice(0, 7), d.reason].filter(Boolean).join(' · ')
  return d.state === 'paused' ? { text, color: 'error' } : { text }
}

export const reasonLine = (ui: Kit, t: RuntimeTicket) => {
  const reason = reasonOf(t)
  return (
    reason && (
      <ui.Text color={reason.color} dimColor={reason.color === undefined} wrap="truncate">
        {`↳ ${reason.text}`}
      </ui.Text>
    )
  )
}

export const trackView = (ui: Kit, pos: Position, nodes: number, segment: number) => (
  <ui.Box flexDirection="row" width={segment * TRACK.length} flexShrink={0}>
    {Array.from({ length: nodes }, (_, i) => {
      const done = pos.at !== null && i < pos.at
      const here = pos.at === i
      return (
        <ui.Box flexDirection="row">
          <ui.Text color={done ? 'success' : here ? TONE_COLOR[pos.tone] : undefined} dimColor={!done && !here}>
            {done ? '●' : here ? '◉' : '○'}
          </ui.Text>
          {i < nodes - 1 && (
            <ui.Text color={done ? 'success' : undefined} dimColor={!done}>
              {(done ? '━' : '─').repeat(segment - 1)}
            </ui.Text>
          )}
        </ui.Box>
      )
    })}
  </ui.Box>
)

export const trackLabels = (ui: Kit, segment: number) => (
  <ui.Box flexDirection="row" marginLeft={SECTION_INSET / 2 + ROW.id}>
    {(segment >= FULL_LABEL_SEGMENT ? TRACK : SHORT_TRACK).map(label => cell(ui, segment, label, true))}
  </ui.Box>
)

export const TREND_MINUTES = 30
const MINUTE_MS = 60_000
const BARS = '▁▂▃▄▅▆▇█'
const TREND_TEXT = { label: '趋势', note: `近 ${TREND_MINUTES} 分钟` }
// Plus the two one-column gaps between label, sparkline and note.
const TREND_FRAME = displayWidth(TREND_TEXT.label) + displayWidth(TREND_TEXT.note) + 2

const NO_SAMPLE = '·'

type Run = { empty: boolean; text: string }

const sparkline = (samples: TrendSample[], now: number, buckets: number): Run[] => {
  const minutesAgo = (one: TrendSample) => Math.floor((now - one.at) / MINUTE_MS)
  const peaks = Array.from({ length: buckets }, (_, i) => {
    const values = samples.filter(one => minutesAgo(one) === buckets - 1 - i).map(one => one.inFlight)
    return values.length === 0 ? null : Math.max(...values)
  })
  const top = Math.max(1, ...peaks.filter(peak => peak !== null))
  return peaks.reduce<Run[]>((runs, peak) => {
    const empty = peak === null
    const glyph = empty ? NO_SAMPLE : BARS.charAt(Math.round((peak / top) * (BARS.length - 1)))
    const last = runs.at(-1)
    return last?.empty === empty
      ? [...runs.slice(0, -1), { empty, text: last.text + glyph }]
      : [...runs, { empty, text: glyph }]
  }, [])
}

export const trendView = (ui: Kit, samples: TrendSample[], now: number, cols: number) => (
  <ui.Box key="trend" flexDirection="row" gap={1}>
    <ui.Text dimColor>{TREND_TEXT.label}</ui.Text>
    <ui.Box flexDirection="row">
      {sparkline(samples, now, Math.max(0, Math.min(TREND_MINUTES, cols - TREND_FRAME))).map(run =>
        run.empty ? <ui.Text dimColor>{run.text}</ui.Text> : <ui.Text color="suggestion">{run.text}</ui.Text>,
      )}
    </ui.Box>
    <ui.Text dimColor>{TREND_TEXT.note}</ui.Text>
  </ui.Box>
)

const BASE64 = 'ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/'
const TERMINAL_DEFAULT = 0x01000000
const SPINNER = [...'⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏']

// Hooks have no Buffer, and Raster cells are base64 of little-endian u32 words.
const base64 = (bytes: number[]) => {
  const at = (i: number) => bytes[i] ?? 0
  return Array.from({ length: Math.ceil(bytes.length / 3) }, (_, k) => {
    const i = k * 3
    const n = (at(i) << 16) | (at(i + 1) << 8) | at(i + 2)
    const sextet = (shift: number) => BASE64.charAt((n >> shift) & 63)
    return sextet(18) + sextet(12) + (i + 1 < bytes.length ? sextet(6) : '=') + (i + 2 < bytes.length ? sextet(0) : '=')
  }).join('')
}

const littleEndian = (word: number) => [word & 255, (word >>> 8) & 255, (word >>> 16) & 255, (word >>> 24) & 255]

export const spinnerCells = (frame: number) => {
  const glyph = SPINNER[frame % SPINNER.length] ?? '⠋'
  return base64([glyph.codePointAt(0) ?? 32, TERMINAL_DEFAULT, TERMINAL_DEFAULT].flatMap(littleEndian))
}

const STDERR_LINES = 6

const failureTitle = (exitCode: number | null) => {
  if (exitCode === null) return '无法运行 orca-factory-driver'
  return exitCode === 0 ? '输出无法解析' : `取数失败（退出码 ${exitCode}）`
}

export const failedView = (ui: Kit, state: { exitCode: number | null; stderr: string }) => (
  <ui.Box flexDirection="column" borderStyle="round" borderColor="error" paddingX={1}>
    <ui.Text color="error">{failureTitle(state.exitCode)}</ui.Text>
    {state.stderr
      .split('\n')
      .filter(line => line.trim() !== '')
      .slice(0, STDERR_LINES)
      .map(line => (
        <ui.Text dimColor wrap="truncate">
          {line}
        </ui.Text>
      ))}
    <ui.Text dimColor>r 重试</ui.Text>
  </ui.Box>
)

export const matches = (filter: string, ...texts: (string | undefined)[]) => {
  const wanted = filter.toLowerCase()
  return wanted === '' || texts.some(one => one?.toLowerCase().includes(wanted))
}

// The field stays drawn: a render that removes a focused element clears the ring, which would undo a move made on submit.
// The host empties the field after submit, so the button carries the text that still filters.
export const filterBar = (
  ui: Kit,
  Input: InputElement | undefined,
  filter: string,
  placeholder: string,
  edit: () => void,
  set: (text: string) => void,
  submit: (text: string) => void,
) => (
  <ui.Box key="filter-bar" flexDirection="row" gap={1}>
    <ui.Button key="filter" label={filter === '' ? '筛选' : `筛选：${filter}`} hotkey="f" plain onPress={edit} />
    {Input && (
      <Input
        key="filter-input"
        value={filter}
        placeholder={filter === '' ? placeholder : '空回车清除筛选'}
        submitLabel="确定"
        onInput={set}
        onSubmit={submit}
      />
    )}
  </ui.Box>
)

export const NO_MATCH = '无匹配'

const LIST_HELP = 'r 刷新 · 1–5 切页 · Tab/↑↓ 移动 · Esc 回到输入框'

const PAGE_HELP: Record<Page, string> = {
  pipeline: '↑↓ 选择 · ⏎ 详情 · f 筛选 · r 刷新 · 1–5 切页 · Esc 回到输入框',
  health: LIST_HELP,
  models: LIST_HELP,
  scheduler: LIST_HELP,
  environment: 'f 筛选 · r 刷新 · 1–5 切页 · Esc 回到输入框',
}

export const helpText = (current: Page, inDetail: boolean) =>
  inDetail ? 'q 返回 · r 刷新 · 1–5 切页 · Esc 回到输入框' : PAGE_HELP[current]

const TIMELINE_ROWS = 15

const timelineBody = (ui: Kit, id: string, state: Fetched<Timeline>) => {
  if (state.kind === 'failed') return failedView(ui, state)
  if (state.kind === 'loading' || state.value.id !== id) {
    return <ui.Text dimColor>{`正在读取 orca-factory-driver logs ${id} …`}</ui.Text>
  }
  const { entries } = state.value
  if (entries.length === 0) return <ui.Text dimColor>没有执行器日志</ui.Text>
  const hidden = Math.max(0, entries.length - TIMELINE_ROWS)
  return [
    ...(hidden > 0 ? [<ui.Text dimColor>{`更早 ${hidden} 条`}</ui.Text>] : []),
    ...entries.slice(hidden).map((one, i) => (
      <ui.Box key={`timeline-${hidden + i}`} flexDirection="row">
        {cell(ui, 9, clockTime(one.at), true)}
        <ui.Text wrap="truncate" color={one.alert}>
          {one.label}
        </ui.Text>
      </ui.Box>
    )),
  ]
}

const ticketFacts = (ui: Kit, t: RuntimeTicket, repos: Repo[], now: number) => {
  const pos = position(t)
  return [
    <ui.Box flexDirection="row">
      {cell(ui, 12, '阶段', true)}
      <ui.Box flexDirection="row" gap={1}>
        <ui.Text color={TONE_COLOR[pos.tone]}>{pos.label}</ui.Text>
        {t.since !== undefined && <ui.Text dimColor>{duration(now - t.since)}</ui.Text>}
      </ui.Box>
    </ui.Box>,
    field(ui, '归属', t.owner),
    field(ui, '仓库', repos.find(one => one.path === t.repo)?.name ?? t.repo),
    reasonLine(ui, t),
  ]
}

const factsBody = (ui: Kit, id: string, status: Fetched<Runtime>, repos: Repo[]) => {
  if (status.kind === 'loading') return <ui.Text dimColor>正在读取 orca-factory-driver status --json …</ui.Text>
  if (status.kind === 'failed') return failedView(ui, status)
  const ticket = status.value.tickets.find(t => t.identifier === id)
  return ticket ? ticketFacts(ui, ticket, repos, status.fetchedAt) : <ui.Text dimColor>已不在在途列表</ui.Text>
}

export const detailView = (
  ui: Kit,
  id: string,
  status: Fetched<Runtime>,
  repos: Repo[],
  timeline: Fetched<Timeline>,
  back: () => void,
) => {
  const title = status.kind === 'ready' ? status.value.tickets.find(t => t.identifier === id)?.title : undefined
  return (
    <ui.Box flexDirection="column">
      <ui.Button key="detail-back" label="← 返回" hotkey="q" onPress={back} />
      {section(ui, id, factsBody(ui, id, status, repos), title)}
      {section(ui, '时间线', timelineBody(ui, id, timeline))}
    </ui.Box>
  )
}
