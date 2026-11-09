const resource = name => { const url = new URL(name, import.meta.url); const token = new URL(import.meta.url).searchParams.get('t'); if (token) url.searchParams.set('t', token); return url.href; };
const modules = await Promise.all(["./primitives.mjs","./town.mjs","./equipment.mjs","./workshop.mjs","./navigation.mjs","./details.mjs","./camera.mjs","./controls.mjs"].map(name=>import(resource(name))));
const { mountShell } = await import(resource('./shell.mjs'));
const geometry = await import(resource('./geometry.mjs'));
export function createPage({projects, tickets, occupancy = null, clock = {now:()=>performance.now()}, actions = {}}) {
  mountShell();
  const runtime = {clock, actions, occupancy, metrics:{resizes:0,draws:0,baseDraws:0,visibleProjects:0}};
  for(const module of modules)for(const [name, method] of Object.entries(module))runtime[name]=method.bind(null,runtime);
  Object.assign(runtime, actions.bind?.(runtime));
runtime.parameters = new URLSearchParams(location.search);
runtime.projects = projects;
runtime.townLayout = runtime.deriveTownLayout(runtime.projects);
Object.assign(runtime, geometry);
runtime.stageOf = ticket => runtime.stages[ticket.stage] ?? {name:'未知阶段',phase:ticket.phase ?? ticket.stage,x:520,y:720};
runtime.tickets = tickets;
runtime.state = {view:'town',project:runtime.projects[0]?.id||null,selected:null,station:null,query:'',reduced:matchMedia('(prefers-reduced-motion: reduce)').matches,zoom:1,panX:0,panY:0};
runtime.palette = {grass:'#b4c681',grassLight:'#c0ce8d',grassDark:'#a5bc75',tree:'#667c46',treeDark:'#465f36',treeLight:'#879b56',trunk:'#826445',path:'#d8c395',pathLight:'#e6d3a5',pathDark:'#b5a478',wood:'#9c7750',darkWood:'#6e5239',wall:'#e9d3a0',wallDark:'#c7ac7b',water:'#82aaa3',waterLight:'#afd0bd'};
runtime.canvas = document.querySelector('#world');
runtime.context = runtime.canvas.getContext('2d');
runtime.viewport = document.querySelector('#viewport');
runtime.labels = document.querySelector('#map-labels');
runtime.base = document.createElement('canvas');
runtime.base.width=1040;
runtime.base.height=760;
runtime.ground = runtime.base.getContext('2d');
runtime.townBase = document.createElement('canvas');
runtime.townGround = runtime.townBase.getContext('2d');
runtime.camera = {scale:1,width:0,height:0,centerX:520,centerY:400};
runtime.drawContext = runtime.ground;
runtime.lastFrame = 0;
runtime.needsDraw = true;
runtime.toastTimer = undefined;
runtime.drag = null;
runtime.townDirty = true;
runtime.moves = new Map();
runtime.escapeText = text => String(text).replace(/[&<>"']/g, character => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[character]));
runtime.activeTickets = project => runtime.tickets.filter(ticket => !project || ticket.project === project);
runtime.needsAttention = actions.needsAttention ?? (ticket => ticket.tone !== 'current');
runtime.detailText = actions.detailText ?? (ticket => `${ticket.id} · ${ticket.title}\n项目：${runtime.projectOf(ticket.project).name}\n阶段：${runtime.stageOf(ticket).phase}\n已观察：${ticket.duration ?? `${ticket.minutes} 分钟`}${ticket.reason?`\n原因：${ticket.reason}`:''}\n（设计原型演示数据）`);
runtime.projectBounds = () => actions.projectBounds?.(runtime) ?? {width:1040,height:760};
runtime.attentionTickets = () => runtime.tickets.filter(runtime.needsAttention);
runtime.projectOf = id => runtime.projects.find(project => project.id === id);
runtime.ticketOf = id => runtime.tickets.find(ticket => (ticket.key ?? ticket.id) === id);
runtime.rect = (left,top,width,height,color) => {runtime.drawContext.fillStyle=color;runtime.drawContext.fillRect(Math.round(left),Math.round(top),Math.round(width),Math.round(height));};
document.addEventListener('click',event=>{
    const target=event.target.closest('button');if(!target)return;
    if(target.dataset.project){runtime.setView('project',target.dataset.project);runtime.clearSearch();}
    else if(target.dataset.ticket){runtime.selectTicket(target.dataset.ticket);runtime.clearSearch();}
    else if(target.dataset.view)runtime.setView(target.dataset.view);
    else if(target.dataset.station){runtime.state.station=target.dataset.station;runtime.state.selected=null;runtime.updateDetail();runtime.createLabels();runtime.updateURL();runtime.needsDraw=true;}
    else if(target.hasAttribute('data-close')){runtime.state.selected=null;runtime.state.station=null;runtime.updateDetail();runtime.createLabels();runtime.updateURL();runtime.needsDraw=true;}
    else if(target.dataset.copy){
      const ticket=runtime.ticketOf(runtime.state.selected);if(!ticket)return;
      runtime.copyText(target.dataset.copy==='id'?ticket.id:runtime.detailText(ticket));
    }else if(target.id==='advance')runtime.advance?.();
    else if(target.id==='demo-exception')runtime.advance?.('exception');
    else if(target.id==='demo-human')runtime.advance?.('human');
  });
document.querySelector('#brand').addEventListener('click',()=>runtime.setView('town'));
if(runtime.applyProjectCount){
document.querySelector('#project-count').addEventListener('change',runtime.applyProjectCount);
document.querySelector('#project-count').addEventListener('keydown',event=>{if(event.key==='Enter'){event.preventDefault();runtime.applyProjectCount();}});
}
document.querySelector('#search').addEventListener('input',runtime.runSearch);
document.querySelector('#search').addEventListener('keydown',event=>{if(event.key==='Enter'){const first=document.querySelector('#search-results button');if(first){first.click();event.preventDefault();}}if(event.key==='Escape')runtime.clearSearch();});
document.addEventListener('pointerdown',event=>{if(!event.target.closest('.search-wrap'))document.querySelector('#search-results').hidden=true;});
document.querySelector('#search').addEventListener('focus',()=>{if(runtime.state.query)runtime.runSearch();});
document.querySelector('#motion-toggle').addEventListener('click',()=>{runtime.state.reduced=!runtime.state.reduced;runtime.updateHeader();runtime.needsDraw=true;});
document.querySelector('#zoom-in').addEventListener('click',()=>runtime.zoom(.2));
document.querySelector('#zoom-out').addEventListener('click',()=>runtime.zoom(-.2));
document.querySelector('#fit-map').addEventListener('click',()=>{runtime.state.zoom=1;runtime.sizeCanvas(true);if(runtime.state.view!=='project')runtime.focusTownProject();});
runtime.canvas.tabIndex=0;
runtime.canvas.setAttribute('aria-label','地图：方向键平移，加减键缩放，Home 复位');
runtime.canvas.addEventListener('keydown',event=>{
    const step={ArrowLeft:[40,0],ArrowRight:[-40,0],ArrowUp:[0,40],ArrowDown:[0,-40]}[event.key];
    if(step){runtime.state.panX+=step[0];runtime.state.panY+=step[1];runtime.sizeCanvas();}
    else if(event.key==='+'||event.key==='=')runtime.zoom(.2);
    else if(event.key==='-')runtime.zoom(-.2);
    else if(event.key==='Home')document.querySelector('#fit-map').click();
    else return;
    event.preventDefault();
  });
runtime.canvas.addEventListener('wheel',event=>{event.preventDefault();runtime.zoom(event.deltaY>0?-.08:.08);},{passive:false});
runtime.canvas.addEventListener('pointerdown',event=>{runtime.drag={x:event.clientX,y:event.clientY,panX:runtime.state.panX,panY:runtime.state.panY};runtime.canvas.setPointerCapture(event.pointerId);});
runtime.canvas.addEventListener('pointermove',event=>{if(!runtime.drag)return;runtime.state.panX=runtime.drag.panX+event.clientX-runtime.drag.x;runtime.state.panY=runtime.drag.panY+event.clientY-runtime.drag.y;runtime.sizeCanvas();});
runtime.canvas.addEventListener('pointerup',()=>{runtime.drag=null;});
runtime.canvas.addEventListener('pointercancel',()=>{runtime.drag=null;});
document.addEventListener('keydown',event=>{
    if(event.target.matches('input,textarea,[contenteditable]'))return;
    if(event.key==='/'){event.preventDefault();document.querySelector('#search').focus();}
    if(event.key==='Escape'){if(runtime.state.selected||runtime.state.station){runtime.state.selected=null;runtime.state.station=null;runtime.updateDetail();runtime.createLabels();runtime.updateURL();runtime.needsDraw=true;}else if(runtime.state.view!=='town')runtime.setView('town');}
  });
new ResizeObserver(()=>runtime.sizeCanvas()).observe(runtime.viewport);
runtime.initialView = runtime.parameters.get('view');
runtime.initialProject = runtime.parameters.get('project');
document.querySelector('#project-count').value=runtime.projects.length;
if(runtime.projectOf(runtime.initialProject))runtime.state.project=runtime.initialProject;
runtime.setView(['town','project','attention'].includes(runtime.initialView)?runtime.initialView:'town',runtime.state.project);
if(runtime.state.view!=='project'&&runtime.projectOf(runtime.initialProject))runtime.focusTownProject();
if(runtime.ticketOf(runtime.parameters.get('ticket')))runtime.selectTicket(runtime.parameters.get('ticket'));
requestAnimationFrame(runtime.frame);
  runtime.actions.ready?.(runtime);
  return runtime;
}
