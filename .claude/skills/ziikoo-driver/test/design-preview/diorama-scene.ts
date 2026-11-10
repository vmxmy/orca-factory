import { readFileSync, writeFileSync } from 'node:fs'
import { parseConfig } from '../../hooks/config'
import { parseRuntime } from '../../hooks/runtime'
import { sceneOf } from '../../hooks/scene'

const read = (url: URL) => readFileSync(url, 'utf8')
const recorded = (name: string) => read(new URL(`../recorded/${name}`, import.meta.url))
const config = parseConfig(recorded('live-config.json'))
const scene = sceneOf(parseRuntime(recorded('live-status.json')), config)
writeFileSync(new URL('./diorama-scene.json', import.meta.url), JSON.stringify(scene))
const nextScan = read(new URL('../../../../../driver/testdata/recorded/driver-status-json-next-scan.json', import.meta.url))
writeFileSync(new URL('./diorama-scene-next-scan.json', import.meta.url), JSON.stringify(sceneOf(parseRuntime(nextScan), config)))
