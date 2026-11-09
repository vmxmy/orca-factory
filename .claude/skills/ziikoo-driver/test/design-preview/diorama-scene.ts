import { readFileSync, writeFileSync } from 'node:fs'
import { parseConfig } from '../../hooks/config'
import { parseRuntime } from '../../hooks/runtime'
import { sceneOf } from '../../hooks/scene'

const recorded = (name: string) => readFileSync(new URL(`../recorded/${name}`, import.meta.url), 'utf8')
const scene = sceneOf(parseRuntime(recorded('live-status.json')), parseConfig(recorded('live-config.json')))
writeFileSync(new URL('./diorama-scene.json', import.meta.url), JSON.stringify(scene))
