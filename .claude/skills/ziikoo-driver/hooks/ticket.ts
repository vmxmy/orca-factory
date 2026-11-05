import type { RuntimeTicket } from '../types'

export const TRACK = ['领', '施工', '门禁', '审查', '合并', '部署']

export type Tone = 'current' | 'attention' | 'failed'
export type Position = { at: number | null; tone: Tone; label: string }

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
  if (t.owner === '处置Agent') return { at: null, tone: 'attention', label: '二线处理中' }
  const at = PHASE_AT[t.phase]
  if (at === undefined) return { at: null, tone: 'attention', label: t.phase || '未知阶段' }
  return { at, tone: t.phase === '写 correction' ? 'attention' : 'current', label: t.phase }
}

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
