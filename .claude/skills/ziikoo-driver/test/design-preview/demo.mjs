export function parseProjectCount(runtime, value) {
    if(value===null||value.trim()==='')return null;
    const count=Number(value);
    return Number.isInteger(count)&&count>=0&&count<=1000?count:null;
  }

export function demoProjects(runtime, count) {
    return Array.from({length:count},(_,index)=>runtime.projectSeeds[index]||{
      id:`project-${index+1}`,name:`小镇工坊 ${String(index+1).padStart(4,'0')}`,short:'坊',
      description:'等待新的委托',color:runtime.projectSeeds[index%runtime.projectSeeds.length].color
    });
  }

export function applyProjectCount(runtime) {
    const input=document.querySelector('#project-count'),error=document.querySelector('#project-count-error'),count=runtime.parseProjectCount(input.value);
    input.setAttribute('aria-invalid',String(count===null));error.hidden=count!==null;
    if(count===null){error.textContent='请输入 0 到 1000 之间的整数，当前项目保持不变。';return;}
    input.value=count;
    if(count===runtime.projects.length)return;
    const previousAnchor=runtime.townLayout.anchors.find(anchor=>anchor.project.id===runtime.state.project);
    const previousIds=new Set(runtime.projects.map(project=>project.id));
    runtime.projects=runtime.demoProjects(count);
    const ids=new Set(runtime.projects.map(project=>project.id));
    runtime.tickets=runtime.tickets.filter(ticket=>ids.has(ticket.project));
    runtime.tickets.push(...runtime.ticketSeeds.filter(ticket=>ids.has(ticket.project)&&!previousIds.has(ticket.project)).map(ticket=>({...ticket,history:ticket.history.map(entry=>[...entry])})));
    for(const id of runtime.moves.keys())if(!runtime.ticketOf(id))runtime.moves.delete(id);
    const removedProject=!ids.has(runtime.state.project);
    if(removedProject){runtime.state.project=runtime.projects[0]?.id||null;runtime.state.station=null;runtime.state.zoom=1;}
    if(!runtime.ticketOf(runtime.state.selected))runtime.state.selected=null;
    if(!runtime.projects.length){runtime.state.view='town';runtime.state.station=null;}
    runtime.townLayout=runtime.deriveTownLayout(runtime.projects);
    const nextAnchor=runtime.townLayout.anchors.find(anchor=>anchor.project.id===runtime.state.project);
    const anchorMoved=previousAnchor&&nextAnchor&&(previousAnchor.x!==nextAnchor.x||previousAnchor.y!==nextAnchor.y);
    runtime.clearSearch();runtime.updateSidebar();runtime.updateHeader();runtime.updateDetail();runtime.rebuildGround();runtime.createLabels();runtime.sizeCanvas(removedProject);
    if(runtime.state.view!=='project'&&(removedProject||anchorMoved))runtime.focusTownProject();
    runtime.updateURL();
  }

export function advance(runtime, branch) {
    const ticket=runtime.ticketOf('OF-127');
    if(runtime.moves.has(ticket.id)||ticket.completed)return;
    const previous=ticket.stage,from=runtime.ticketPosition(ticket);
    let next=runtime.mainRoute[runtime.mainRoute.indexOf(previous)+1];
    if(branch==='exception'&&previous==='gate')next='repair';
    else if(branch==='exception'&&previous==='review')next='return';
    else if(branch==='human'&&previous==='repair')next='human';
    else if(previous==='human')next='return';
    else if(previous==='repair'||previous==='return')next='work';
    else if(previous==='smoke'){next='smoke';ticket.completed=true;}
    ticket.stage=next;ticket.minutes=0;ticket.owner=next==='merge'?'合并队列':'执行器';ticket.queue=next==='merge'?1:undefined;
    ticket.tone=runtime.stages[next].branch?'attention':'current';
    ticket.reason=next==='repair'?'演示：验收未通过，包裹已转入修理支线。':next==='return'?'演示：任务需要修改，确认后回到制作台。':next==='human'?'演示：自动处置未能解决问题，等待人工确认。':undefined;
    if(next==='human')ticket.owner='人工';
    runtime.moves.set(ticket.id,{points:runtime.movementPath(previous,next,from,runtime.ticketPosition(ticket)),start:performance.now()});
    const [lastHour,lastMinute]=ticket.history[0][0].split(':').map(Number),nextMinute=(lastHour*60+lastMinute+1)%(24*60);
    const demoTime=`${String(Math.floor(nextMinute/60)).padStart(2,'0')}:${String(nextMinute%60).padStart(2,'0')}`;
    ticket.history.unshift([demoTime,ticket.completed?'演示：出厂检查通过，任务完成':`演示：进入${runtime.stages[next].name}`]);
    if(ticket.history.length>5)ticket.history.pop();
    runtime.updateDetail();runtime.updateSidebar();runtime.updateHeader();runtime.createLabels();runtime.rebuildGround();
  }

export function demoActions(runtime, ticket) {
    const busy=runtime.moves.has(ticket.id),disabled=busy||ticket.completed?'disabled':'';
    const label=ticket.completed?'演示已完成 · 刷新可重置':busy?'包裹正在沿传送带移动…':ticket.stage==='human'?'演示流转 · 人工确认后返工':ticket.stage==='return'||ticket.stage==='repair'?'演示流转 · 修正后回施工':ticket.stage==='smoke'?'演示流转 · 检查通过并完成':'演示流转 · 推进一个工位';
    return `<button class="paper-button primary advance-button" id="advance" ${disabled}>${label}</button>${['gate','review'].includes(ticket.stage)?`<button class="paper-button branch-action" id="demo-exception" ${disabled}>${ticket.stage==='gate'?'演示支线 · 验收未通过':'演示支线 · 审查退回'}</button>`:''}${ticket.stage==='repair'?`<button class="paper-button branch-action" id="demo-human" ${disabled}>演示支线 · 转交人工</button>`:''}`;
  }
