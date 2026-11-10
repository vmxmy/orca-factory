import { expect, test } from 'claude-code/testing'

import type { Runtime } from '../types'
import { parseConfig } from '../hooks/config'
import { parseRuntime } from '../hooks/runtime'
import { sceneOf } from '../hooks/scene'
import { SAMPLE_CONFIG } from './sample-config'
import { RECORDED_CONFIG, RECORDED_DEPLOY_STATUS, RECORDED_LEDGER_DEPLOYMENT, RECORDED_STATUS } from './sample-recorded'
import { SAMPLE_STATUS } from './sample-runtime'

const config = () => parseConfig(JSON.stringify(SAMPLE_CONFIG))
const runtime = () => parseRuntime(JSON.stringify(SAMPLE_STATUS))

test('sceneOf 透传可选记录 url，缺字段时省略', () => {
  const status = structuredClone(SAMPLE_STATUS)
  const url = 'https://example.feishu.cn/base/records?record=rec123&view=grid'
  Object.assign(status.tickets[0]!, { url })
  const parsed = parseRuntime(JSON.stringify(status))
  expect(parsed.tickets[0]).toHaveProperty('url', url)
  const parcels = sceneOf(parsed, config()).rows[0]!.parcels
  expect(parcels[0]).toHaveProperty('url', url)
  expect(JSON.parse(JSON.stringify(parsed.tickets[1]))).not.toHaveProperty('url')
  expect(JSON.parse(JSON.stringify(parcels[1]))).not.toHaveProperty('url')
})

test('sceneOf 将 candidate 放到领取台，保留锁链原因', () => {
  const scene = sceneOf(runtime(), config())
  const parcel = scene.rows.flatMap(row => row.parcels).find(one => one.identifier === 'ZW-8')
  expect(parcel?.station).toBe('pickup')
  expect(parcel?.blocker).toBe('blocked by ZW-7')
  expect(scene.rows).toHaveLength(SAMPLE_CONFIG.repos.length)
})

test('sceneOf 将处置Agent放到维修间，human 优先于处置归属', () => {
  const status = runtime()
  status.tickets[0]!.owner = '处置Agent'
  expect(sceneOf(status, config()).rows[0]?.parcels[0]?.station).toBe('repair')
  status.tickets[0]!.stage = 'human'
  expect(sceneOf(status, config()).rows[0]?.parcels[0]?.station).toBe('human')
})

test('sceneOf 对没有 deployment 的已合并票标记 unknownDeploy 且不画卡车', () => {
  const status = runtime()
  status.tickets[2]!.deployment = undefined
  const parcel = sceneOf(status, config()).rows[0]?.parcels[2]
  expect(parcel?.unknownDeploy).toBe(true)
  expect(parcel?.station).toBe('merge')
})

test('sceneOf 沿用部署、候选、人工、处置、阶段与回流的优先级', () => {
  const status = runtime()
  const ticket = status.tickets[2]!
  ticket.stage = 'human'
  ticket.owner = '处置Agent'
  expect(sceneOf(status, config()).rows[0]?.parcels[2]?.station).toBe('smoke')
  ticket.deployment!.state = 'paused'
  expect(sceneOf(status, config()).rows[0]?.parcels[2]?.tone).toBe('failed')
  const scene = sceneOf(status, config())
  expect(scene.rows[0]?.parcels[1]?.station).toBe('return')
  expect(scene.rows[0]?.deploy).toBe(true)
  expect(scene.rows[1]?.deploy).toBe(false)
  expect(scene.rows[0]?.workers).toBe(Number(SAMPLE_CONFIG.scheduler.find(one => one.key === 'max_in_flight')?.value))
})

test('sceneOf 透传 lane 与打回次数，缺字段时省略，形状不对时报错', () => {
  const status = structuredClone(SAMPLE_STATUS)
  Object.assign(status.tickets[2]!, { lane: 2, corrections: { count: 0, limit: 3 } })
  const parcels = sceneOf(parseRuntime(JSON.stringify(status)), config()).rows[0]!.parcels
  expect([parcels[2]?.lane, parcels[2]?.corrections]).toEqual([2, { count: 0, limit: 3 }])
  expect(Object.keys(JSON.parse(JSON.stringify(parcels[0])))).not.toContain('corrections')
  expect(Object.keys(JSON.parse(JSON.stringify(parcels[0])))).not.toContain('lane')
  Object.assign(status.tickets[2]!, { lane: 4 })
  expect(() => parseRuntime(JSON.stringify(status))).toThrow('tickets[2].lane 不是 1–3 的整数')
  Object.assign(status.tickets[2]!, { lane: 2, corrections: { count: 0, limit: 0 } })
  expect(() => parseRuntime(JSON.stringify(status))).toThrow('tickets[2].corrections.limit 不是 1–20 的整数')
})

test('sceneOf 消费真实录制的 status/config 快照', () => {
  const recordedRuntime = parseRuntime(RECORDED_STATUS)
  const recordedConfig = parseConfig(RECORDED_CONFIG)
  const scene = sceneOf(recordedRuntime, recordedConfig)
  expect(scene.state).toBe('stale')
  expect(scene.maintenance).toBe('draining')
  expect(scene.nextScanAt).toBeUndefined()
  const rowsByName = new Map(scene.rows.map(row => [row.name, row]))
  expect(rowsByName.size).toBe(recordedConfig.repos.length)
  const factory = rowsByName.get('orca-factory')
  expect(factory?.parcels.map(parcel => parcel.identifier)).toEqual(['OF-414', 'OF-415'])
  const queued = factory?.parcels.find(parcel => parcel.identifier === 'OF-414')
  expect(queued?.station).toBe('merge')
  expect(queued?.unknownDeploy).toBe(false)
  expect(queued?.owner).toBe('合并通道')
  expect(queued?.queuePosition).toBe(1)
  const resolved = factory?.parcels.find(parcel => parcel.identifier === 'OF-415')
  expect(resolved?.station).toBe('repair')
  expect(resolved?.tone).toBe('attention')
  expect(resolved?.owner).toBe('处置Agent')
  expect(JSON.parse(JSON.stringify(resolved))).not.toHaveProperty('queuePosition')
  expect(factory?.deploy).toBe(true)
})

test('sceneOf 透传录制里的下次巡检时间', () => {
  const scene = sceneOf(parseRuntime(RECORDED_DEPLOY_STATUS), parseConfig(RECORDED_CONFIG))
  expect(scene.nextScanAt).toBe(Date.parse('2026-11-04T16:06:00+08:00'))
})

test('sceneOf 消费还原的部署屏障与未知阶段录制快照', () => {
  const runtime = parseRuntime(RECORDED_DEPLOY_STATUS)
  const scene = sceneOf(runtime, parseConfig(RECORDED_CONFIG))
  const row = scene.rows.find(one => one.name === 'zhiyun-ai-dashboard')
  const barrier = row?.parcels.find(one => one.identifier === 'ZY-365')
  expect(barrier?.station).toBe('dispatch')
  expect(barrier?.tone).toBe('current')
  expect(barrier?.label).toBe('等部署')
  expect(barrier?.deployment).toBe('pending')
  expect(barrier?.blocker).toBe('deployment: pending')
  const unmapped = row?.parcels.find(one => one.identifier === 'ZY-366')
  expect(unmapped?.station).toBe('unknown')
  expect(unmapped?.tone).toBe('attention')
  expect(unmapped?.label).toBe('准备镜像')
  expect(unmapped?.unknownDeploy).toBe(false)
})

test('部署屏障快照的字段来自真实账本录制', () => {
  const ledger = JSON.parse(RECORDED_LEDGER_DEPLOYMENT)
  const handoff = ledger.comments.flatMap((comment: { body: string }) => comment.body.split('\n'))
    .find((line: string) => line.startsWith('merge-handoff-v1: '))
  const progresses = ledger.comments.flatMap((comment: { body: string }) => comment.body.split('\n'))
    .filter((line: string) => line.startsWith('merge-progress-v1: '))
    .map((line: string) => JSON.parse(line.slice('merge-progress-v1: '.length)))
  const pending = progresses.find(progress => progress.phase === 'merge-intent' && progress.deployment?.state === 'pending')
  expect(handoff).toBeDefined()
  expect(pending).toBeDefined()
  const frozen = JSON.parse(handoff!.slice('merge-handoff-v1: '.length))
  const status = JSON.parse(RECORDED_DEPLOY_STATUS)
  const ticket = status.tickets.find((one: { identifier: string }) => one.identifier === 'ZY-365')
  expect(ticket.deployment.queueId).toBe(frozen.queue_id)
  expect(ticket.deployment.sha).toBe(frozen.head)
  expect(ticket.deployment.workflow).toBe(pending.deployment.workflow)
  expect(ticket.deployment.state).toBe(pending.deployment.state)
  expect(ticket.blockedReason).toBe(`deployment: ${pending.deployment.state}`)
})

test('录制的部署暂停记录驱动 sceneOf 的部署异常分支', () => {
  const ledger = JSON.parse(RECORDED_LEDGER_DEPLOYMENT)
  const lines = ledger.comments.flatMap((comment: { body: string }) => comment.body.split('\n'))
  const progressLines = lines
    .filter((line: string) => line.startsWith('merge-progress-v1: '))
    .map((line: string) => JSON.parse(line.slice('merge-progress-v1: '.length)))
  const paused = progressLines.find(progress => progress.deployment?.state === 'paused')
  expect(paused).toBeDefined()
  expect(paused.deployment.reason).toBe('deployment workflow failed')
  expect(paused.phase).toBe('merged')
  const frozen = JSON.parse(lines.find((line: string) => line.startsWith('merge-handoff-v1: '))!.slice('merge-handoff-v1: '.length))
  const recorded = JSON.parse(RECORDED_DEPLOY_STATUS)
  const barrier = recorded.tickets.find((ticket: { deployment?: { queueId: string } }) => ticket.deployment?.queueId === frozen.queue_id)
  expect(barrier).toBeDefined()
  const runtime: Runtime = {
    state: 'ok',
    generatedAt: 1,
    tickets: [{
      repo: barrier.repo,
      identifier: barrier.identifier,
      owner: '处置Agent',
      phase: '已合并',
      since: Date.parse(barrier.since),
      blockedReason: `deployment: ${paused.deployment.state}`,
      deployment: {
        queueId: paused.queue_id,
        sha: paused.head,
        workflow: paused.deployment.workflow,
        state: paused.deployment.state,
        reason: paused.deployment.reason,
      },
    }],
  }
  const scene = sceneOf(runtime, parseConfig(RECORDED_CONFIG))
  const row = scene.rows.find(one => one.name === 'zhiyun-ai-dashboard')
  const parcel = row?.parcels.find(one => one.identifier === barrier.identifier)
  expect(parcel?.station).toBe('dispatch')
  expect(parcel?.tone).toBe('failed')
  expect(parcel?.label).toBe('部署暂停')
  expect(parcel?.deployment).toBe('paused')
  expect(parcel?.blocker).toBe('deployment: paused')
  expect(parcel?.reason).toBe('deployment: paused')
})
