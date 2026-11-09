import { createPage } from '../../scene/page/components/index.mjs'
import * as demo from './demo.mjs'
import { projectSeeds, ticketSeeds } from './seeds.mjs'

const parameters = new URLSearchParams(location.search)
const idleProjects = new Set(parameters.has('idle') ? parameters.get('idle').split(',').filter(Boolean) : ['app'])
const withIdle = list => list.map(project=>({...project,idle:idleProjects.has(project.id)}))
const projects = withIdle(demo.demoProjects({projectSeeds}, demo.parseProjectCount({}, parameters.get('count')) ?? 8))
const tickets = ticketSeeds.filter(ticket => projects.some(project => project.id === ticket.project)).map(ticket => structuredClone(ticket))
for(const ticket of tickets)if(ticket.id==='OF-131')ticket.note='等待依赖完成'
const time = parameters.get('time')
const clock = {now: () => time === null ? performance.now() : Number(time)}
window.componentTown = createPage({projects, tickets, clock, actions: {
  flowNote:'选择 OF-127 可演示主线与返工。动画不代表真实执行进度。',
  bind(runtime) {
    return {projectSeeds, ticketSeeds, ...Object.fromEntries(Object.entries(demo).map(([name, method]) => [name, method.bind(null, runtime)])), demoProjects: count => withIdle(demo.demoProjects(runtime, count))}
  },
  detail(runtime, ticket) {
    if (ticket?.id === 'OF-127') document.querySelector('.panel-content').insertAdjacentHTML('beforeend', runtime.demoActions(ticket))
  },
  arrived(runtime,ticket) {runtime.showToast(ticket.completed?'演示：出厂检查通过，任务已完成':`演示：${ticket.id} 已到${runtime.stages[ticket.stage].name}`)},
}})
