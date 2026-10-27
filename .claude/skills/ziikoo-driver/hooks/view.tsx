import type { ElementTable, RenderChildren } from 'claude-code'

import type { TrendSample } from '../types'

export type Kit = Pick<ElementTable, 'Box' | 'Text' | 'Button'>

export type Alert = 'warning' | 'error'

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
