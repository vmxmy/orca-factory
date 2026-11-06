export function updateURL(runtime) {
    const current=new URLSearchParams(location.search);
    if(runtime.applyProjectCount)current.set('count',runtime.projects.length);
    current.set('view',runtime.state.view);
    if(runtime.state.project)current.set('project',runtime.state.project);else current.delete('project');
    if(runtime.state.selected)current.set('ticket',runtime.state.selected);else current.delete('ticket');
    try{history.replaceState(null,'',`${location.pathname}?${current}`);}catch{}
    runtime.actions.changed?.(runtime);
  }

export function setView(runtime, view,project) {
    const returning=runtime.state.view==='project'&&view!=='project';
    runtime.state.project=runtime.projectOf(project)?.id||runtime.projectOf(runtime.state.project)?.id||runtime.projects[0]?.id||null;
    runtime.state.view=view==='project'&&!runtime.state.project?'town':view;
    runtime.state.selected=null;runtime.state.station=null;runtime.state.zoom=1;runtime.state.panX=0;runtime.state.panY=0;
    runtime.updateSidebar();runtime.updateHeader();runtime.updateDetail();runtime.rebuildGround();runtime.sizeCanvas(true);runtime.createLabels();
    if(runtime.state.view!=='project'&&(returning||runtime.camera.width<700))runtime.focusTownProject();
    runtime.updateURL();
  }

export function selectTicket(runtime, id) {
    const ticket=runtime.ticketOf(id);if(!ticket)return;
    if(runtime.state.view!=='project'||runtime.state.project!==ticket.project)runtime.setView('project',ticket.project);
    runtime.state.selected=id;runtime.state.station=null;runtime.updateDetail();runtime.createLabels();runtime.needsDraw=true;
    document.querySelector('#detail-panel').scrollTop=0;
    if(matchMedia('(max-width:760px)').matches){
      runtime.state.zoom=1.65;runtime.sizeCanvas();
      const position=runtime.ticketPosition(ticket);
      runtime.state.panX=(520-position.x)*runtime.camera.scale;
      runtime.state.panY=(400-position.y)*runtime.camera.scale+175-runtime.camera.height/2;
      runtime.sizeCanvas();
    }
    runtime.updateURL();
  }

export function showToast(runtime, message) {
    const toast=document.querySelector('#toast');toast.textContent=message;toast.classList.add('visible');clearTimeout(runtime.toastTimer);runtime.toastTimer=setTimeout(()=>toast.classList.remove('visible'),2300);
  }

export async function copyText(runtime, text) {
    try {
      if(navigator.clipboard&&window.isSecureContext)await navigator.clipboard.writeText(text);
      else {const textarea=document.createElement('textarea');textarea.value=text;textarea.style.cssText='position:fixed;left:-9999px';document.body.append(textarea);textarea.select();const copied=document.execCommand('copy');textarea.remove();if(!copied)throw new Error('clipboard');}
      runtime.showToast('已复制到剪贴板');
    }catch{runtime.showToast('浏览器限制了复制，请手动选择详情中的文字。');}
  }

export function movementPath(runtime, fromStage,next,from,to) {
    if(fromStage==='review'&&next==='pack')return [from,{x:948,y:285},{x:948,y:610},to];
    if(fromStage==='review'&&next==='return')return [from,{x:880,y:442},to];
    if(fromStage==='repair'&&next==='work')return [from,{x:400,y:442},to];
    return [from,to];
  }

export function pointAlong(runtime, points,amount) {
    const lengths=points.slice(1).map((point,index)=>Math.hypot(point.x-points[index].x,point.y-points[index].y));
    let remaining=lengths.reduce((sum,length)=>sum+length,0)*amount;
    for(let index=0;index<lengths.length;index++){
      if(remaining<=lengths[index]){
        const fraction=lengths[index]?remaining/lengths[index]:1;
        return {x:points[index].x+(points[index+1].x-points[index].x)*fraction,y:points[index].y+(points[index+1].y-points[index].y)*fraction};
      }
      remaining-=lengths[index];
    }
    return points[points.length-1];
  }
