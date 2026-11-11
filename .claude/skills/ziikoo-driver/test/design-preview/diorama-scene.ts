import { readFileSync, writeFileSync } from 'node:fs'
import { parseConfig } from '../../hooks/config'
import { parseRuntime } from '../../hooks/runtime'
import { sceneOf } from '../../hooks/scene'

const read = (url: URL) => readFileSync(url, 'utf8')
const recorded = (name: string) => read(new URL(`../recorded/${name}`, import.meta.url))
const driverRecorded = (name: string) => read(new URL(`../../../../../driver/testdata/recorded/${name}`, import.meta.url))
const config = parseConfig(recorded('live-config.json'))
const scene = sceneOf(parseRuntime(recorded('live-status.json')), config)
writeFileSync(new URL('./diorama-scene.json', import.meta.url), JSON.stringify(scene))
const nextScan = driverRecorded('driver-status-json-next-scan.json')
writeFileSync(new URL('./diorama-scene-next-scan.json', import.meta.url), JSON.stringify(sceneOf(parseRuntime(nextScan), config)))
const stale = driverRecorded('driver-status-json-stale.json')
writeFileSync(new URL('./diorama-scene-human.json', import.meta.url), JSON.stringify(sceneOf(parseRuntime(stale), config)))
const staleStatus = JSON.parse(stale)
const waiting = staleStatus.tickets.find((ticket: { identifier: string }) => ticket.identifier === 'ZY-428')
const older = staleStatus.tickets.find((ticket: { identifier: string }) => ticket.identifier === 'OF-425')
const { since: _since, ...untimed } = waiting
const queue = { ...staleStatus, tickets: [...staleStatus.tickets, { ...waiting, identifier: 'ZY-429', since: older.since }, { ...untimed, identifier: 'ZY-430' }] }
writeFileSync(new URL('./diorama-scene-human-queue.json', import.meta.url), JSON.stringify(sceneOf(parseRuntime(JSON.stringify(queue)), config)))
