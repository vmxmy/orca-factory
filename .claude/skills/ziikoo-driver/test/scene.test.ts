import { expect, test } from 'claude-code/testing'

import { parseConfig } from '../hooks/config'
import { parseRuntime } from '../hooks/runtime'
import { sceneOf } from '../hooks/scene'
import { SAMPLE_CONFIG } from './sample-config'
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

test('parseRuntime 保留数字 queuePosition，缺失保持 undefined，非数字拒绝', () => {
  const raw = structuredClone(SAMPLE_STATUS)
  const status = { ...raw, tickets: raw.tickets.map(one => ({ ...one, queuePosition: 3 })) }
  expect(parseRuntime(JSON.stringify(status)).tickets[0]?.queuePosition).toBe(3)
  expect(runtime().tickets[0]?.queuePosition).toBe(undefined)
  expect(() => parseRuntime(JSON.stringify({ ...raw, tickets: [{ ...raw.tickets[0], queuePosition: '3' }] }))).toThrow()
})
