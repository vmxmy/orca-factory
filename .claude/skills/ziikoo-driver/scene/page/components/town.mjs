export function deriveTownLayout(runtime, list) {
    const anchors=list.map((project,index)=>({project,x:list.length===1?520:index%2?760:290,y:list.length===1?330:245+Math.floor(index/2)*320}));
    return {anchors,riverX:list.length===1?270:514,bounds:{width:1040,height:Math.max(760,(anchors.at(-1)?.y||245)+280)},visible:[]};
  }

export function drawTown(runtime) {
    const leftEdge=runtime.camera.centerX-runtime.camera.width/(2*runtime.camera.scale),rightEdge=runtime.camera.centerX+runtime.camera.width/(2*runtime.camera.scale);
    const topEdge=Math.max(0,runtime.camera.centerY-runtime.camera.height/(2*runtime.camera.scale)-60),bottomEdge=Math.min(runtime.townLayout.bounds.height,runtime.camera.centerY+runtime.camera.height/(2*runtime.camera.scale)+60);
    runtime.rect(leftEdge,topEdge,rightEdge-leftEdge,bottomEdge-topEdge,runtime.palette.grass);
    for(let tileY=Math.floor(topEdge/128);tileY<=Math.floor(bottomEdge/128);tileY++){
      for(let tileX=Math.floor(leftEdge/128);tileX<=Math.floor(rightEdge/128);tileX++){
        const random=runtime.seeded(tileY*104729+tileX*290+414);
        for(let item=0;item<52;item++){
          const left=tileX*128+Math.floor(random()*42)*3,top=tileY*128+Math.floor(random()*42)*3;
          runtime.rect(left,top,random()>.6?4:2,2,random()>.5?'#c4d28e':'#a6bd78');
          if(item%7===0){runtime.rect(left,top-3,1,3,'#9db36d');runtime.rect(left+3,top-2,1,2,'#9db36d');}
        }
      }
    }
    for(let row=Math.floor(topEdge/6)*6;row<bottomEdge;row+=6){
      const left=runtime.townLayout.riverX+Math.round(Math.sin(row/100)*17);
      runtime.rect(left-30,row,64,6,'#96b180');runtime.rect(left-25,row,54,6,runtime.palette.water);runtime.rect(left-24,row,4,6,'#adc8a3');runtime.rect(left+23,row,3,6,'#6f998b');
      if(row%30===0){runtime.rect(left-12,row,18,2,runtime.palette.waterLight);runtime.rect(left+8,row+9,11,2,'#a6c6b7');}
    }
    if(runtime.projects.length){
      const roadStart=Math.max(topEdge,runtime.projects.length===1?390:310),roadEnd=Math.min(bottomEdge,runtime.townLayout.bounds.height-55);
      if(roadStart<roadEnd)for(const left of runtime.projects.length===1?[520]:[290,760])runtime.path([[left,roadStart],[left,roadEnd]],27);
      const rows=Math.ceil(runtime.projects.length/2);
      for(let row=Math.max(0,Math.floor((topEdge-435)/320));row<Math.min(rows,Math.ceil((bottomEdge-375)/320)+1);row++){
        const top=runtime.projects.length===1?492:407+row*320;
        runtime.path([[0,top],[1040,top]],30);
        runtime.drawContext.save();runtime.drawContext.translate(runtime.townLayout.riverX-514,0);
        runtime.rect(469,top-26,92,54,'#7b6746');
        for(let board=0;board<10;board++){runtime.rect(472+board*9,top-23,7,47,'#c7aa73');runtime.rect(472+board*9,top-20,7,2,'#e1c58c');}
        runtime.rect(466,top-27,98,4,'#8c724a');runtime.rect(466,top+25,98,4,'#8c724a');
        for(const left of [468,560]){runtime.rect(left,top-32,4,65,'#a58a59');runtime.rect(left-2,top-32,8,5,'#dcc28d');}
        runtime.drawContext.restore();
        runtime.rect(88,top-24,22,8,'#9c8559');runtime.rect(90,top-17,4,15,'#7e6946');runtime.rect(105,top-17,4,15,'#7e6946');
      }
    }
    for(const anchor of runtime.townLayout.visible){
      const {project,x:left,y:top}=anchor;
      runtime.rect(left-147,top-83,294,178,'#acbd7a');
      runtime.path([[left-97,top+59],[left+103,top+59]],25);runtime.path([[left,top+60],[left,top+152]],22);
      runtime.fence(left-148,top-80,120);runtime.fence(left+23,top-80,120);
      runtime.fence(left-148,top+105,114);runtime.fence(left+38,top+105,104);
      runtime.building(left,top,'workshop',project.color,1.05,project.idle);
      runtime.building(left-107,top+11,'shed',project.color,.70,project.idle);
      runtime.building(left+108,top+10,'post',project.color,.67,project.idle);
      for(let flowerIndex=0;flowerIndex<12;flowerIndex++){runtime.flower(left-130+flowerIndex*11,top+92+(flowerIndex%2)*5,flowerIndex);}
      runtime.tree(left-163,top-30,.72,1);runtime.tree(left+158,top-32,.75);runtime.bush(left-119,top+80,true);runtime.bush(left+119,top+78,true);
      if(project.idle){runtime.rect(left-19,top+69,39,5,'#a58a55');runtime.rect(left-16,top+74,4,7,'#7b6544');runtime.rect(left+14,top+74,4,7,'#7b6544');}
    }
    for(let row=Math.max(0,Math.floor((topEdge-27)/58));row*58+27<bottomEdge;row++){
      const random=runtime.seeded(row+414),top=row*58+27;
      runtime.tree(35+random()*20,top,.88);runtime.tree(995+random()*16,top,1.05,1);
    }
    const random=runtime.seeded(414);
    for(let left=90;left<970;left+=61){
      if(Math.abs(left-runtime.townLayout.riverX)<50)continue;
      runtime.tree(left,39+random()*9,.72,left%2);runtime.tree(left,runtime.townLayout.bounds.height-17+random()*10,.87);
      runtime.flower(left+15,79,left);runtime.flower(left-12,runtime.townLayout.bounds.height-40,left);
    }
  }
