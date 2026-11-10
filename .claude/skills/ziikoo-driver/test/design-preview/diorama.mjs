import { mountDiorama } from '../../scene/page/diorama/index.mjs'

const stage = document.querySelector('.diorama')

try {
  const response = await fetch('./diorama-scene.json', { cache: 'no-store' })
  if (!response.ok) throw new Error(`读不到 diorama-scene.json（HTTP ${response.status}）。先运行 bun test/design-preview/diorama-scene.ts。`)
  const scene = await response.json()
  const query = new URLSearchParams(location.search)
  const opened = performance.now()
  window.diorama = mountDiorama(stage, {
    scene,
    path: query.get('project'),
    ticket: query.get('ticket'),
    now: () => scene.generatedAt + performance.now() - opened,
  })
} catch (error) {
  const failure = document.createElement('p')
  failure.className = 'notice failure'
  failure.textContent = error.message
  stage.append(failure)
  throw error
}
