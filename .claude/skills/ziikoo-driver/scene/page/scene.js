const token = new URL(import.meta.url).searchParams.get('t')
const resource = pathname => `${pathname}?t=${encodeURIComponent(token)}`
const [{createPage}, model] = await Promise.all([import(resource('./components/index.mjs')),import(resource('./town.mjs'))])
const element = selector => document.querySelector(selector)
const reduced = matchMedia('(prefers-reduced-motion: reduce)')
const failures = new Map()
let state = model.initialState()
let layout = model.townLayout([])
let connected = false
let fresh = true
let snapshotRequest
let restored = false
let workshops = new Map()

function synchronize(runtime) {
  state.project = runtime.state.project
  state.view = runtime.state.view === 'project' ? 'workshop' : 'town'
  state.selection = runtime.state.selected ? {kind:'parcel',key:decodeURIComponent(runtime.state.selected)} : runtime.state.station ? {kind:'station',station:runtime.state.station} : null
  state.camera = {x:runtime.camera.centerX,y:runtime.camera.centerY,zoom:runtime.camera.scale}
  layout = state.view === 'town' ? model.townLayout(state.scene?.rows ?? []) : model.workshopLayout(state.scene.rows.find(row=>row.path===state.project))
  const url = new URL(location.href)
  for(const name of ['ticket','count','parcel'])url.searchParams.delete(name)
  if(state.selection?.kind==='parcel')url.searchParams.set('parcel',JSON.parse(state.selection.key)[1])
  if(state.project)url.searchParams.set('project',state.project);else url.searchParams.delete('project')
  url.searchParams.set('view',state.view)
  history.replaceState(null,'',url)
}

function decorateDetail(runtime, ticket) {
  const panel = element('#detail')
  if (ticket) {
    const parcel = ticket.parcel
    const row = state.scene.rows.find(row=>row.path===ticket.project)
    const values = {'当前工位':model.stations[model.stationOf(parcel.station)][0],'归属':'未提供','本阶段已观察':parcel.duration,'数据来源':'driver Scene 实时快照'}
    for(const field of panel.querySelectorAll('.field-row'))field.querySelector('dd').textContent=values[field.querySelector('dt').textContent] ?? field.querySelector('dd').textContent
    panel.querySelector('.state-pill').textContent=parcel.label
    panel.querySelector('.ticket-project').textContent=`${row.name} · ${row.path}`
    panel.querySelector('.timeline-note').textContent='当前快照不含执行历史；人物表示工位占用，不代表 worker 数量。'
    const facts = document.createElement('p')
    facts.className='station-description'
    facts.textContent=`真实阶段：${parcel.phase} / ${parcel.label}\n原因：${parcel.reason || parcel.blocker || '未提供'}\n部署：${!row.deploy?'不适用 · 未配置部署':parcel.deployment || (parcel.unknownDeploy?'未知':'未提供')}${parcel.unknownDeploy?'\n部署字段：未知 · driver 未提供 deployment':''}${parcel.deployment?`\n部署原始状态：${parcel.deployment}`:''}`
    panel.querySelector('.field-list').after(facts)
  } else if (runtime.state.station) {
    panel.querySelector('.station-description').textContent=model.stations[runtime.state.station][1]
  }
  const guide=panel.querySelector('.flow-guide p:last-child')
  if(guide)guide.textContent='包裹位置来自实时快照；动画不代表真实执行进度，人物表示工位占用。'
  const empty=panel.querySelector('.empty-state')
  if(!runtime.projects.length&&empty)empty.textContent='暂无项目。等待 driver 的真实项目快照。'
}

const initialURL = new URL(location.href)
const runtime = createPage({projects:[],tickets:[],actions:{
  detailText:({id,title,project,parcel})=>{const row=state.scene.rows.find(row=>row.path===project);const reason=parcel.reason||parcel.blocker;return `${id} · ${title}\n项目：${row.name} · ${row.path}\n工位：${model.stations[model.stationOf(parcel.station)][0]}\n真实阶段：${parcel.phase} / ${parcel.label}\n已观察：${parcel.duration}${reason?`\n原因：${reason}`:''}`},
  projectBounds:page=>workshops.get(page.state.project)?.bounds,
  needsAttention:({parcel})=>['attention','failed'].includes(parcel.tone)||parcel.unknownDeploy||model.stationOf(parcel.station)==='unknown'||['paused','failed'].includes(parcel.deployment),
  changed:synchronize,
  detail:decorateDetail,
  arrived(page,ticket) {state.motions.delete(model.parcelKey(ticket.project,ticket.id))},
  ready(page) {
    element('.project-count').hidden=true
    element('.demo-badge').textContent='实时流水线'
    element('.nav-end').id='status'
    element('.footer-left').textContent='工坊小镇 · driver Scene · 只读'
    element('.footer-right a').remove()
    const original=page.updateDetail
    page.updateDetail=()=>{original();if(!page.state.selected)decorateDetail(page)}
  },
}})
runtime.motionPoint=(motion,amount)=>model.pointAlong(motion.points,amount,motion.dwell ?? 0,motion.dwellAt ?? 0)
runtime.positionOf=ticket=>workshops.get(ticket.project).parcels.get(model.parcelKey(ticket.project,ticket.id))
runtime.updateURL=()=>synchronize(runtime)
const originalSize=runtime.sizeCanvas
runtime.sizeCanvas=(...args)=>{originalSize(...args);state.camera={x:runtime.camera.centerX,y:runtime.camera.centerY,zoom:runtime.camera.scale}}
let pointer
runtime.canvas.addEventListener('pointerdown',event=>{pointer={x:event.clientX,y:event.clientY}})
runtime.canvas.addEventListener('pointerup',event=>{
  if(!pointer||Math.hypot(event.clientX-pointer.x,event.clientY-pointer.y)>4)return
  pointer=null
  if(runtime.state.view==='project')return
  const bounds=runtime.canvas.getBoundingClientRect()
  const point={x:(event.clientX-bounds.left-runtime.camera.width/2)/runtime.camera.scale+runtime.camera.centerX,y:(event.clientY-bounds.top-runtime.camera.height/2)/runtime.camera.scale+runtime.camera.centerY}
  const anchor=runtime.townLayout.visible.find(anchor=>Math.abs(anchor.x-point.x)<145&&Math.abs(anchor.y-point.y)<100)
  if(anchor)runtime.setView('project',anchor.project.id)
})

function renderStatus() {
  const scene=state.scene
  const age=scene?Math.max(0,Math.floor((Date.now()-scene.generatedAt)/1000)):null
  const stale=scene?.state==='stale'||age>40
  element('#status').textContent=[connected?'已连接':'连接断开',scene?`${stale?'快照过期':'实时快照'} · ${age} 秒前 · ${scene.maintenance}`:'等待快照',...[...failures.values()].map(failure=>`${failure.command} ${failure.exitCode ?? ''} ${failure.reason}`)].join(' · ')
  element('#status').title=element('#status').textContent
}

function receive(scene) {
  if(!scene||!Array.isArray(scene.rows)){failures.set('snapshot',{command:'snapshot',reason:'快照缺少 rows'});renderStatus();return}
  const previous=state
  state=model.reconcile(state,scene,{now:runtime.clock.now(),fresh,reduced:runtime.state.reduced})
  fresh=false
  if(state===previous){renderStatus();return}
  workshops=new Map(scene.rows.map(row=>[row.path,model.workshopLayout(row)]))
  runtime.projects=scene.rows.map((row,index)=>({id:row.path,name:row.name,short:row.name.slice(0,1),description:row.path,color:['#b87951','#8d9370','#ae7758','#77918b'][index%4],idle:!row.parcels.length}))
  runtime.tickets=scene.rows.flatMap(row=>row.parcels.map(parcel=>({id:parcel.identifier,key:encodeURIComponent(model.parcelKey(row.path,parcel.identifier)),project:row.path,title:parcel.title,stage:model.stationOf(parcel.station),phase:parcel.phase,tone:parcel.tone,duration:parcel.duration,owner:'未提供',reason:parcel.reason||parcel.blocker,history:[],parcel})))
  runtime.occupancy=new Map(scene.rows.map(row=>[row.path,new Set(row.parcels.map(parcel=>model.stationOf(parcel.station)))]))
  runtime.townLayout=runtime.deriveTownLayout(runtime.projects)
  runtime.state.project=state.project
  runtime.state.view=state.view==='workshop'?'project':runtime.state.view==='attention'?'attention':'town'
  runtime.state.selected=state.selection?.kind==='parcel'?encodeURIComponent(state.selection.key):null
  runtime.state.station=state.selection?.kind==='station'?state.selection.station:null
  runtime.moves=new Map([...state.motions].map(([key,motion])=>[encodeURIComponent(key),motion]))
  if(!restored&&scene.rows.length){
    restored=true
    const project=initialURL.searchParams.get('project')
    const parcel=initialURL.searchParams.get('parcel')
    if(scene.rows.some(row=>row.path===project))runtime.state.project=project
    runtime.setView(['project','workshop'].includes(initialURL.searchParams.get('view'))?'project':'town',runtime.state.project)
    if(runtime.state.view!=='project'&&initialURL.searchParams.has('project'))runtime.focusTownProject()
    if(parcel)runtime.selectTicket(encodeURIComponent(model.parcelKey(runtime.state.project,parcel)))
  }else{
    runtime.updateSidebar();runtime.updateHeader();runtime.updateDetail();runtime.rebuildGround();runtime.sizeCanvas();runtime.createLabels()
    if(state.project!==previous.project||state.view==='town'&&(state.camera.x!==previous.camera.x||state.camera.y!==previous.camera.y))runtime.focusTownProject()
    if(runtime.state.selected&&runtime.camera.width<500)runtime.selectTicket(runtime.state.selected)
    synchronize(runtime)
  }
  renderStatus()
}

async function fetchSnapshot() {
  const controller=new AbortController()
  snapshotRequest?.abort()
  snapshotRequest=controller
  try {
    const response=await fetch(resource('/snapshot'),{signal:controller.signal})
    if(!response.ok)throw new Error(`快照 HTTP ${response.status}`)
    const scene=await response.json()
    if(controller===snapshotRequest)receive(scene)
  }catch(error){if(error.name!=='AbortError'){failures.set('snapshot',{command:'snapshot',reason:error.message});renderStatus()}}
}

let events
function connectEvents() {
  if(events)return
  fresh=true
  events=new EventSource(resource('/events'))
  events.onopen=()=>{connected=true;fresh=true;failures.clear();renderStatus()}
  events.addEventListener('scene',event=>{snapshotRequest?.abort();failures.delete('snapshot');receive(JSON.parse(event.data))})
  events.addEventListener('error',event=>{
    if(event.data){const failure=JSON.parse(event.data);if(failure.recovered)failures.delete(failure.command);else failures.set(failure.command,failure)}
    else{connected=false;fresh=true}
    renderStatus()
  })
}
reduced.addEventListener('change',()=>{runtime.state.reduced=reduced.matches;if(reduced.matches){runtime.moves.clear();state.motions.clear()}runtime.updateHeader();runtime.needsDraw=true})
window.addEventListener('pagehide',()=>{events?.close();events=undefined;connected=false;fresh=true;snapshotRequest?.abort();snapshotRequest=undefined;renderStatus()})
window.addEventListener('pageshow',event=>{if(event.persisted){connectEvents();void fetchSnapshot()}})
setInterval(renderStatus,5000)
connectEvents()
void fetchSnapshot()
window.pixelTown=Object.freeze({get state(){return state},get layout(){return layout},metrics:runtime.metrics,selectedScreenPoint(){const ticket=runtime.ticketOf(runtime.state.selected);const point=ticket?runtime.ticketPosition(ticket):runtime.townLayout.anchors.find(anchor=>anchor.project.id===runtime.state.project);if(!point)return null;const screen=runtime.toScreen(point.x,point.y);const bounds=runtime.canvas.getBoundingClientRect();return {x:bounds.left+screen.x,y:bounds.top+screen.y}}})
