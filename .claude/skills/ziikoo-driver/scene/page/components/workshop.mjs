export function drawProject(runtime) {
    runtime.rect(0,0,1040,760,'#d4c5a3');runtime.rect(37,111,975,595,'#786449');runtime.rect(29,101,975,595,'#68523b');
    runtime.rect(39,111,955,573,'#cfb783');
    const random=runtime.seeded(18);
    for(let row=0;row<20;row++){
      const top=143+row*27;
      runtime.rect(44,top,946,25,row%3===0?'#d8bf8a':row%3===1?'#d4b982':'#ceb17b');
      for(let left=45+(row%2)*68;left<986;left+=137){
        runtime.rect(left,top,2,25,'#bda071');runtime.rect(left+11,top+18,33+random()*38,1,'#b3986838');runtime.rect(left+13,top+5,3,2,'#a88c61');
      }
    }
    runtime.rect(39,107,955,43,'#aa8b5f');
    for(let left=45;left<990;left+=61){runtime.rect(left,109,56,18,'#c7af7f');runtime.rect(left+12,130,44,14,'#bca475');}
    for(const left of [48,306,724,980]){runtime.rect(left,103,10,54,'#775a3d');runtime.rect(left+2,104,3,48,'#b49361');}
    for(const left of [180,820]){
      runtime.rect(left-37,107,74,38,'#6e573e');runtime.rect(left-31,110,62,28,'#a3c4b3');
      runtime.rect(left-27,111,23,23,'#c2d9bb');runtime.rect(left+4,111,23,23,'#b9d3b4');runtime.rect(left-2,109,4,29,'#95764c');runtime.rect(left-39,140,78,5,'#dfc48b');
    }
    runtime.rect(426,110,188,33,'#745a3d');runtime.rect(430,113,180,26,'#a78e5c');
    runtime.rect(39,150,955,6,'#897149');runtime.rect(39,676,955,8,'#8b6a43');
    for(const left of [53,979]){runtime.rect(left,160,8,505,'#8c714d');runtime.rect(left+2,160,3,505,'#b49462');}
    for(const left of [86,969]){
      runtime.rect(left-12,177,24,34,'#b79964');runtime.rect(left-9,181,18,27,'#d8be87');runtime.bush(left,175,false);
      runtime.crate(left-12,650);runtime.crate(left-4,637);
    }
    runtime.rect(102,340,645,119,'#987a4e20');
    runtime.branchBelts.forEach(points=>runtime.belt(points,18,true));
    runtime.belt(runtime.mainBelt,38);
    for(const left of [160,400,640,880]){
      runtime.rect(left-44,660,88,3,'#b19666');runtime.rect(left-34,665,68,2,'#b1966666');
    }
    for(const left of [280,520,760]){runtime.arrow(left,285,0);runtime.arrow(left,610,Math.PI);}
    runtime.arrow(948,391,Math.PI/2);runtime.arrow(948,506,Math.PI/2);
    runtime.arrow(640,341,Math.PI/2,'#f4d79b');runtime.arrow(880,374,Math.PI/2,'#f4d79b');runtime.arrow(400,344,-Math.PI/2,'#f4d79b');
    runtime.arrow(282,442,Math.PI,'#f4d79b');runtime.arrow(524,442,Math.PI,'#f4d79b');runtime.arrow(763,442,Math.PI,'#f4d79b');
  }

export function rebuildGround(runtime) {
    if(runtime.state.view==='project'){runtime.drawContext=runtime.ground;runtime.ground.clearRect(0,0,1040,760);runtime.drawProject();}
    runtime.townDirty=true;runtime.needsDraw=true;
  }

export function ticketPosition(runtime, ticket) {
    if(runtime.positionOf)return runtime.positionOf(ticket);
    const stage=runtime.stageOf(ticket),siblings=runtime.activeTickets(ticket.project).filter(other=>other.stage===ticket.stage).sort((first,second)=>(first.queue||0)-(second.queue||0)),index=siblings.indexOf(ticket);
    return {x:ticket.completed?92:stage.x+(index-(siblings.length-1)/2)*62*(stage.y===285?-1:1),y:stage.y};
  }

export function drawDynamic(runtime, time) {
    runtime.drawContext=runtime.context;
    if(runtime.state.view!=='project'){
      for(const {project,x,y} of runtime.townLayout.visible){
        const list=runtime.activeTickets(project.id);
        if(list.length){
          const walk=runtime.state.reduced?0:Math.sin(time*.33+x)*38;
          runtime.person(x+walk,y+45,time,'#77959b',!runtime.state.reduced,x);
          runtime.person(x-79,y+37,time,'#a69569',false);
          runtime.crate(x+walk+11,y+47,true);
          const puff=runtime.state.reduced?0:(time*8)%31;
          runtime.rect(x+40,y-112-puff,8,6,'#e3e6cd99');runtime.rect(x+44,y-120-puff,12,8,'#e8e9d388');
        }
        if(list.some(ticket=>ticket.tone!=='current')){
          const left=x+104,top=y-59;
          runtime.rect(left,top,3,38,'#8d7450');runtime.rect(left+3,top,22,13,'#c69b4f');runtime.rect(left+11,top+3,2,5,'#f8edc5');runtime.rect(left+11,top+10,2,2,'#f8edc5');
        }
      }
      const random=runtime.seeded(30);
      for(let butterfly=0;butterfly<5;butterfly++){const left=100+random()*850,top=180+random()*480,flutter=runtime.state.reduced?0:Math.sin(time*2+butterfly)*8;runtime.rect(left+flutter,top,3,3,'#edda9b');runtime.rect(left+5+flutter,top+1,3,2,'#f0e7bb');}
    }else{
      runtime.beltRollers(runtime.mainBelt,38,runtime.state.reduced?0:(time*17)%17);
      for(const left of [280,520,760]){runtime.arrow(left,285,0);runtime.arrow(left,610,Math.PI);}
      runtime.arrow(948,391,Math.PI/2);runtime.arrow(948,506,Math.PI/2);
      for(const [id,station] of Object.entries(runtime.stages))runtime.workbench(id,station,time);
      for(const ticket of runtime.activeTickets(runtime.state.project)){
        let position=runtime.ticketPosition(ticket),movement=runtime.moves.get(ticket.key ?? ticket.id);
        if(movement){
          const amount=runtime.state.reduced?1:Math.min(1,Math.max(0,(runtime.clock.now()-movement.start)/(movement.duration ?? 1200)));
          position=runtime.motionPoint ? runtime.motionPoint(movement, amount) : runtime.pointAlong(movement.points,amount);
          if(amount===1){
            runtime.moves.delete(ticket.key ?? ticket.id);
            if(runtime.state.selected===(ticket.key ?? ticket.id))runtime.updateDetail();
            runtime.actions.arrived?.(runtime,ticket);
            runtime.needsDraw=true;
          }
        }
        const label=runtime.labels.querySelector(`[data-ticket="${runtime.escapeText(ticket.key ?? ticket.id)}"]`);
        if(label)runtime.placeTicketLabel(label,ticket,position);
        if(runtime.state.selected===(ticket.key ?? ticket.id)){
          runtime.drawContext.strokeStyle=ticket.tone==='current'?'#507d8e':'#b98940';runtime.drawContext.lineWidth=2;runtime.drawContext.setLineDash([4,4]);runtime.drawContext.strokeRect(Math.round(position.x-19),Math.round(position.y-13),39,29);runtime.drawContext.setLineDash([]);
        }
        runtime.crate(position.x-9,position.y-9);
        runtime.rect(position.x-4,position.y-6,8,6,'#f0ddb1');runtime.rect(position.x-2,position.y-5,4,1,ticket.tone==='current'?'#6e8d94':'#b08746');
        if(ticket.tone!=='current'){runtime.rect(position.x+17,position.y-27,2,23,'#8b754a');runtime.rect(position.x+19,position.y-27,13,10,ticket.tone==='failed'?'#b46e54':'#c69b4f');runtime.rect(position.x+23,position.y-25,2,4,'#f7e7bb');}
        if(ticket.completed){runtime.rect(position.x+17,position.y-27,2,23,'#8b754a');runtime.rect(position.x+19,position.y-27,13,10,'#6f9260');}
        if(ticket.queue){runtime.drawContext.fillStyle='#66593d';runtime.drawContext.font='9px monospace';runtime.drawContext.textAlign='center';runtime.drawContext.fillText(String(ticket.queue),position.x,position.y-14);}
      }
    }
  }
