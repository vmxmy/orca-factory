export function createLabels(runtime) {
    if(runtime.state.view!=='project'){
      runtime.labels.innerHTML=runtime.townLayout.visible.map(({project,x,y})=>{
        const count=runtime.projectCounts(project);
        return `<button class="world-label village-label ${count.attention?'needs-attention':''}" data-project="${runtime.escapeText(project.id)}" data-x="${x}" data-y="${y+100}"><strong>${runtime.escapeText(project.name)}</strong><small><span class="tiny-dot"></span>${count.total?`${count.total} 张委托${count.attention?` · ${count.attention} 项需关注`:' · 正在工作'}`:'当前空闲'}</small></button>`;
      }).join('');
    }else{
      runtime.labels.innerHTML=Object.entries(runtime.stages).map(([id,stage])=>`<button class="world-label station-label ${stage.branch?'branch':''}" data-station="${id}" data-x="${stage.x}" data-y="${stage.y-(stage.branch?94:115)}" aria-pressed="${runtime.state.station===id}" aria-label="${stage.number} ${stage.name}，${stage.phase}"><b>${stage.number}</b>${stage.name}<span>${runtime.activeTickets(runtime.state.project).filter(ticket=>ticket.stage===id).length} 件</span></button>`).join('')+runtime.activeTickets(runtime.state.project).map(ticket=>{
        const position=runtime.ticketPosition(ticket);
        return `<button class="ticket-chip ${ticket.tone} ${runtime.state.selected===(ticket.key ?? ticket.id)?'selected':''}" data-ticket="${runtime.escapeText(ticket.key ?? ticket.id)}" data-x="${position.x}" data-y="${position.y+31}" aria-label="查看 ${runtime.escapeText(ticket.id)} ${runtime.escapeText(ticket.title)}">${runtime.escapeText(ticket.id)}${ticket.completed?' ✓':''}</button>`;
      }).join('');
      runtime.labels.insertAdjacentHTML('beforeend',[
        [520,126,'工坊流水线','room-sign'],
        [500,329,'↑ 修正后回施工',''],
        [714,329,'验收异常 ↓',''],
        [880,329,'审查退回 ↓',''],
        [274,468,'← 人工接手',''],
        [520,665,'← 合并、部署、出厂检查','']
      ].map(([left,top,text,kind])=>`<span class="world-label route-note ${kind}" data-x="${left}" data-y="${top}">${text}</span>`).join(''));
    }
    runtime.positionLabels();
  }

export function toScreen(runtime, left,top) {return {x:(left-runtime.camera.centerX)*runtime.camera.scale+runtime.camera.width/2,y:(top-runtime.camera.centerY)*runtime.camera.scale+runtime.camera.height/2};}

export function placeTicketLabel(runtime, label,ticket,position) {
    label.hidden=false;
    const point=runtime.toScreen(position.x,position.y+31);
    if(runtime.camera.width<500){
      if(runtime.stageOf(ticket).branch){point.x+=35;point.y-=29*runtime.camera.scale;}
      else{
        const siblings=runtime.activeTickets(ticket.project).filter(other=>other.stage===ticket.stage);
        if(siblings.length>1){
          point.x+=(siblings.indexOf(ticket)-(siblings.length-1)/2)*Math.max(0,46-62*runtime.camera.scale)*(runtime.stageOf(ticket).y===285?-1:1);
        }
      }
      const half=label.offsetWidth/2+3;
      if(point.x>-half&&point.x<runtime.camera.width+half)point.x=Math.max(half,Math.min(runtime.camera.width-half,point.x));
    }
    label.style.left=`${point.x}px`;label.style.top=`${point.y}px`;
    label.hidden=point.x+label.offsetWidth/2<0||point.x-label.offsetWidth/2>runtime.camera.width||point.y+label.offsetHeight/2<0||point.y-label.offsetHeight/2>runtime.camera.height;
  }

export function positionLabels(runtime) {
    runtime.labels.querySelectorAll('[data-x]').forEach(label=>{
      if(label.dataset.ticket){const ticket=runtime.ticketOf(label.dataset.ticket);if(!ticket){label.remove();return;}runtime.placeTicketLabel(label,ticket,runtime.ticketPosition(ticket));}
      else{
        label.hidden=false;
        const point=runtime.toScreen(Number(label.dataset.x),Number(label.dataset.y));
        label.style.left=`${point.x}px`;label.style.top=`${point.y}px`;
        const halfWidth=label.offsetWidth/2,halfHeight=label.offsetHeight/2;
        label.hidden=runtime.state.view==='project'
          ?point.x+halfWidth<0||point.x-halfWidth>runtime.camera.width||point.y+halfHeight<0||point.y-halfHeight>runtime.camera.height
          :point.x-halfWidth<4||point.x+halfWidth>runtime.camera.width-4||point.y-halfHeight<130||point.y+halfHeight>runtime.camera.height-85;
      }
    });
  }

export function constrainCamera(runtime) {
    const bounds=runtime.state.view==='project'?{width:1040,height:760}:runtime.townLayout.bounds;
    const halfWidth=runtime.camera.width/(2*runtime.camera.scale),halfHeight=runtime.camera.height/(2*runtime.camera.scale);
    const following=runtime.state.view==='project'&&runtime.state.selected&&matchMedia('(max-width:760px)').matches;
    if(following){
      runtime.camera.centerX=Math.max(0,Math.min(bounds.width,runtime.camera.centerX));
      runtime.camera.centerY=Math.max(0,Math.min(bounds.height+Math.max(0,(runtime.camera.height/2-175)/runtime.camera.scale),runtime.camera.centerY));
    }else{
      runtime.camera.centerX=halfWidth*2>=bounds.width?bounds.width/2:Math.max(halfWidth,Math.min(bounds.width-halfWidth,runtime.camera.centerX));
      runtime.camera.centerY=halfHeight*2>=bounds.height?bounds.height/2:Math.max(halfHeight,Math.min(bounds.height-halfHeight,runtime.camera.centerY));
    }
    runtime.state.panX=(520-runtime.camera.centerX)*runtime.camera.scale;runtime.state.panY=(400-runtime.camera.centerY)*runtime.camera.scale;
  }

export function sizeCanvas(runtime, reset=false) {
    const bounds=runtime.viewport.getBoundingClientRect();runtime.camera.width=bounds.width;runtime.camera.height=bounds.height;
    const bitmapWidth=Math.ceil(bounds.width/2),bitmapHeight=Math.ceil(bounds.height/2);
    if(runtime.canvas.width!==bitmapWidth||runtime.canvas.height!==bitmapHeight)runtime.metrics.resizes++;
    if(runtime.canvas.width!==bitmapWidth)runtime.canvas.width=bitmapWidth;
    if(runtime.canvas.height!==bitmapHeight)runtime.canvas.height=bitmapHeight;
    runtime.context.imageSmoothingEnabled=false;
    const mobile=bounds.width<500;
    const fitted=runtime.state.view==='project'?Math.min(bounds.width/(mobile?930:1040),bounds.height/820):Math.max(.65,Math.min(bounds.width/1040,bounds.height/820));
    const centerX=runtime.camera.centerX,centerY=runtime.camera.centerY,previousScale=runtime.camera.scale;
    if(runtime.state.view!=='project')runtime.state.zoom=Math.max(runtime.state.zoom,.65/fitted);
    runtime.camera.scale=fitted*runtime.state.zoom;
    if(reset){runtime.state.panX=0;runtime.state.panY=0;}
    else if(runtime.state.view!=='project'&&previousScale!==runtime.camera.scale){runtime.state.panX=(520-centerX)*runtime.camera.scale;runtime.state.panY=(400-centerY)*runtime.camera.scale;}
    runtime.camera.centerX=520-runtime.state.panX/runtime.camera.scale;runtime.camera.centerY=400-runtime.state.panY/runtime.camera.scale;
    runtime.constrainCamera();
    document.querySelector('#zoom-text').textContent=`${Math.round(runtime.state.zoom*100)}%`;
    if(runtime.state.view!=='project'){
      const halfWidth=runtime.camera.width/(2*runtime.camera.scale),halfHeight=runtime.camera.height/(2*runtime.camera.scale);
      runtime.townLayout.visible=runtime.townLayout.anchors.filter(anchor=>Math.abs(anchor.x-runtime.camera.centerX)<halfWidth+190&&anchor.y+160>runtime.camera.centerY-halfHeight&&anchor.y-150<runtime.camera.centerY+halfHeight);
      if(runtime.townBase.width!==runtime.canvas.width)runtime.townBase.width=runtime.canvas.width;
      if(runtime.townBase.height!==runtime.canvas.height)runtime.townBase.height=runtime.canvas.height;
      runtime.townDirty=true;runtime.createLabels();
    }else runtime.positionLabels();
    runtime.needsDraw=true;
  }

export function focusTownProject(runtime) {
    const anchor=runtime.townLayout.anchors.find(item=>item.project.id===runtime.state.project);
    if(!anchor)return;
    const left=runtime.camera.width<700?anchor.x:520;
    runtime.state.panX=(520-left)*runtime.camera.scale;runtime.state.panY=(400-anchor.y-40)*runtime.camera.scale;
    runtime.sizeCanvas();
  }

export function zoom(runtime, amount){runtime.state.zoom=Math.max(.7,Math.min(2.4,runtime.state.zoom+amount));runtime.sizeCanvas();}

export function frame(runtime, timestamp) {
    requestAnimationFrame(runtime.frame);
    if(document.hidden||timestamp-runtime.lastFrame<40||runtime.state.reduced&&!runtime.needsDraw&&!runtime.moves.size)return;
    runtime.lastFrame=timestamp;runtime.needsDraw=false;
    runtime.metrics.draws++;
    runtime.metrics.visibleProjects=runtime.state.view==='project'?0:runtime.townLayout.visible.length;
    const selectedMove=runtime.moves.get(runtime.state.selected);
    if(selectedMove&&runtime.state.view==='project'&&matchMedia('(max-width:760px)').matches){
      const amount=runtime.state.reduced?1:Math.max(0,Math.min(1,(runtime.clock.now()-selectedMove.start)/(selectedMove.duration ?? 1200)));
      const position=runtime.motionPoint?runtime.motionPoint(selectedMove,amount):runtime.pointAlong(selectedMove.points,amount);
      runtime.state.panX=(520-position.x)*runtime.camera.scale;
      runtime.state.panY=(400-position.y)*runtime.camera.scale+175-runtime.camera.height/2;
      runtime.camera.centerX=520-runtime.state.panX/runtime.camera.scale;runtime.camera.centerY=400-runtime.state.panY/runtime.camera.scale;
      runtime.positionLabels();
    }
    runtime.context.setTransform(1,0,0,1,0,0);runtime.context.clearRect(0,0,runtime.canvas.width,runtime.canvas.height);runtime.context.fillStyle=runtime.state.view==='project'?'#d4c5a3':runtime.palette.grass;runtime.context.fillRect(0,0,runtime.canvas.width,runtime.canvas.height);
    if(runtime.state.view!=='project'){
      if(runtime.townDirty){
        runtime.townGround.setTransform(1,0,0,1,0,0);runtime.townGround.fillStyle=runtime.palette.grass;runtime.townGround.fillRect(0,0,runtime.townBase.width,runtime.townBase.height);
        runtime.townGround.setTransform(runtime.camera.scale/2,0,0,runtime.camera.scale/2,(runtime.camera.width/2-runtime.camera.centerX*runtime.camera.scale)/2,(runtime.camera.height/2-runtime.camera.centerY*runtime.camera.scale)/2);
        runtime.drawContext=runtime.townGround;runtime.drawTown();runtime.townDirty=false;runtime.metrics.baseDraws++;
      }
      runtime.context.drawImage(runtime.townBase,0,0);
    }
    runtime.context.save();runtime.context.scale(.5,.5);runtime.context.translate(runtime.camera.width/2,runtime.camera.height/2);runtime.context.scale(runtime.camera.scale,runtime.camera.scale);runtime.context.translate(-runtime.camera.centerX,-runtime.camera.centerY);
    if(runtime.state.view==='project')runtime.context.drawImage(runtime.base,0,0);
    runtime.drawDynamic(runtime.clock.now()/1000);runtime.context.restore();
  }
