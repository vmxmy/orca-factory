import { expect, test } from 'claude-code/testing'

import { parseConfig } from '../hooks/config'
import { parseRuntime } from '../hooks/runtime'
import { sceneOf } from '../hooks/scene'
import { SAMPLE_CONFIG } from './sample-config'
import { RECORDED_CONFIG, RECORDED_DEPLOY_STATUS, RECORDED_STATUS } from './sample-recorded'
import { SAMPLE_STATUS } from './sample-runtime'

const config = () => parseConfig(JSON.stringify(SAMPLE_CONFIG))
const runtime = () => parseRuntime(JSON.stringify(SAMPLE_STATUS))

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

test('sceneOf 消费真实录制的 status/config 快照', () => {
  const recordedRuntime = parseRuntime(RECORDED_STATUS)
  const recordedConfig = parseConfig(RECORDED_CONFIG)
  const scene = sceneOf(recordedRuntime, recordedConfig)
  expect(scene.state).toBe('stale')
  expect(scene.maintenance).toBe('draining')
  const rowsByName = new Map(scene.rows.map(row => [row.name, row]))
  expect(rowsByName.size).toBe(recordedConfig.repos.length)
  const factory = rowsByName.get('orca-factory')
  expect(factory?.parcels.map(parcel => parcel.identifier)).toEqual(['OF-414', 'OF-415'])
  const queued = factory?.parcels.find(parcel => parcel.identifier === 'OF-414')
  expect(queued?.station).toBe('merge')
  expect(queued?.unknownDeploy).toBe(false)
  const resolved = factory?.parcels.find(parcel => parcel.identifier === 'OF-415')
  expect(resolved?.station).toBe('repair')
  expect(resolved?.tone).toBe('attention')
  expect(factory?.deploy).toBe(true)
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
