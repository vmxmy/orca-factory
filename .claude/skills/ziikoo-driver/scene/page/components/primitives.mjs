export function seeded(runtime, seed) {let value=seed;return () => {value=(value*1664525+1013904223)>>>0;return value/4294967296;};}

export function ellipse(runtime, left,top,width,height,color) {runtime.drawContext.fillStyle=color;runtime.drawContext.beginPath();runtime.drawContext.ellipse(left,top,width,height,0,0,Math.PI*2);runtime.drawContext.fill();}

export function tree(runtime, left,top,scale=1,variant=0) {
    runtime.drawContext.save();runtime.drawContext.translate(Math.round(left),Math.round(top));runtime.drawContext.scale(scale,scale);
    runtime.rect(-21,28,48,8,'#728b4c44');runtime.rect(-5,8,10,28,runtime.palette.trunk);runtime.rect(0,13,3,20,'#614b35');
    const dark=variant?'#617b45':runtime.palette.treeDark,mid=variant?'#7f9651':runtime.palette.tree,light=variant?'#98ac62':runtime.palette.treeLight;
    runtime.rect(-18,-25,37,58,dark);runtime.rect(-27,-14,54,35,dark);runtime.rect(-11,-34,24,10,dark);
    runtime.rect(-22,-13,42,32,mid);runtime.rect(-16,-24,32,44,mid);runtime.rect(-8,-31,17,12,mid);
    runtime.rect(-17,-16,16,13,light);runtime.rect(-9,-26,14,14,light);runtime.rect(-23,-5,9,12,light);
    runtime.rect(9,5,12,9,dark);runtime.rect(-5,20,17,7,dark);runtime.rect(-14,4,7,5,'#879c56');
    runtime.drawContext.restore();
  }

export function flower(runtime, left,top,kind=0) {
    runtime.rect(left,top+3,2,5,'#7f9756');runtime.rect(left-2,top+5,3,2,'#8aa15f');
    const colors=['#f5e8c3','#d8a28d','#d5b65b','#ede8c9'];
    runtime.rect(left-2,top,6,3,colors[kind%4]);runtime.rect(left,top-2,2,7,colors[kind%4]);runtime.rect(left,top,2,2,'#b79a51');
  }

export function bush(runtime, left,top,flowers=false) {
    runtime.rect(left-14,top-5,29,15,'#70894b');runtime.rect(left-9,top-10,20,7,'#7c9650');runtime.rect(left-11,top-3,10,5,'#93a75d');
    if(flowers){runtime.flower(left-7,top-4,1);runtime.flower(left+5,top,0);}
  }

export function fence(runtime, left,top,width) {
    runtime.rect(left,top+4,width,5,'#8b7551');runtime.rect(left,top+15,width,4,'#9e885e');
    for(let offset=0;offset<=width;offset+=24){runtime.rect(left+offset,top,5,26,'#806545');runtime.rect(left+offset,top,3,20,'#c0a877');runtime.rect(left+offset-1,top+2,7,3,'#d3be8e');}
  }

export function path(runtime, points,width=26) {
    runtime.drawContext.lineJoin='round';runtime.drawContext.lineCap='square';runtime.drawContext.strokeStyle=runtime.palette.pathDark;runtime.drawContext.lineWidth=width+6;
    runtime.drawContext.beginPath();points.forEach(([left,top],index)=>index?runtime.drawContext.lineTo(left,top):runtime.drawContext.moveTo(left,top));runtime.drawContext.stroke();
    runtime.drawContext.strokeStyle=runtime.palette.path;runtime.drawContext.lineWidth=width;runtime.drawContext.stroke();
    runtime.drawContext.strokeStyle=runtime.palette.pathLight;runtime.drawContext.lineWidth=width-8;runtime.drawContext.stroke();
    const random=runtime.seeded(Math.round(points[0][0]*7+points[0][1]));
    for(let segment=1;segment<points.length;segment++){
      const from=points[segment-1],to=points[segment],length=Math.hypot(to[0]-from[0],to[1]-from[1]);
      for(let distance=0;distance<length;distance+=16){const part=distance/length;runtime.rect(from[0]+(to[0]-from[0])*part+(random()-.5)*width-2,from[1]+(to[1]-from[1])*part+(random()-.5)*width-2,4,2,'#c4b183');}
    }
  }

export function crate(runtime, left,top,small=false) {
    const width=small?13:19,height=small?12:17;
    runtime.rect(left+2,top+height,width,3,'#726a3b3b');runtime.rect(left,top,width,height,'#8d6c44');runtime.rect(left+2,top+2,width-4,height-4,'#c5a16a');runtime.rect(left+3,top+3,width-6,2,'#dfbf85');runtime.rect(left+width/2-1,top,2,height,'#9d7a4b');runtime.rect(left,top+height-4,width,2,'#a07c49');
  }

export function windowAt(runtime, left,top,lit=true) {
    runtime.rect(left,top,17,20,'#8b6743');runtime.rect(left+2,top+2,13,15,lit?'#eac774':'#a6bab0');runtime.rect(left+3,top+3,5,6,lit?'#f8dfa2':'#c9d8c4');runtime.rect(left+8,top,2,18,'#8c6b48');runtime.rect(left,top+9,17,2,'#8c6b48');runtime.rect(left-2,top+18,21,4,'#bb9763');
  }

export function roof(runtime, left,top,width,height,color) {
    runtime.rect(left-5,top+height,width+10,7,'#6f5138');
    for(let row=0;row<height;row+=5){
      const inset=Math.round((height-row)*.73);
      runtime.rect(left+inset-4,top+row,width-inset*2+8,5,color);
      runtime.rect(left+inset-4,top+row,width-inset*2+8,1,'#d7a07988');
      for(let column=left+inset+((row/5)%2)*8;column<left+width-inset;column+=17)runtime.rect(column,top+row,1,5,'#74503966');
    }
    runtime.rect(left+height*.73-4,top-2,width-height*1.46+8,3,'#dbb18a');
  }

export function building(runtime, left,top,kind='workshop',color='#b47d59',scale=1,idle=false) {
    runtime.drawContext.save();runtime.drawContext.translate(Math.round(left),Math.round(top));runtime.drawContext.scale(scale,scale);
    if(kind==='board'){
      runtime.rect(-31,16,65,8,'#61744232');runtime.rect(-26,-35,5,62,'#8a6945');runtime.rect(22,-35,5,62,'#8a6945');runtime.rect(-34,-42,68,47,'#826344');runtime.rect(-30,-38,60,39,'#b6945f');runtime.rect(-27,-35,54,32,'#ccae77');runtime.roof(-38,-63,76,18,color);
      runtime.rect(-21,-30,18,25,'#f1e3b8');runtime.rect(3,-28,19,21,'#e9d7a5');runtime.rect(-17,-25,10,2,'#a99b75');runtime.rect(-17,-20,10,2,'#a99b75');runtime.rect(7,-23,11,2,'#a99b75');runtime.rect(9,-14,5,5,'#ba8662');
      runtime.drawContext.restore();return;
    }
    const width=kind==='shed'?84:kind==='hall'?130:kind==='gate'?92:116;
    const height=kind==='shed'?45:65;
    runtime.rect(-width/2-5,27,width+19,13,'#60743f35');
    runtime.rect(-width/2,-height+27,width,height,runtime.palette.darkWood);runtime.rect(-width/2+4,-height+31,width-8,height-7,runtime.palette.wall);
    runtime.rect(-width/2+5,20,width-10,7,runtime.palette.wallDark);
    for(let row=-height+38;row<20;row+=10)runtime.rect(-width/2+5,row,width-10,1,'#c6ab7f66');
    runtime.rect(-width/2+2,-height+28,5,height,'#ad8452');runtime.rect(width/2-8,-height+28,5,height,'#ad8452');
    runtime.rect(-13,-7,27,36,'#795b3e');runtime.rect(-10,-4,21,31,'#9c754b');runtime.rect(-8,-2,7,26,'#ac8455');runtime.rect(6,11,2,2,'#eddba1');runtime.rect(-19,28,39,5,'#c5b188');runtime.rect(-22,33,45,5,'#e1cda2');
    if(kind!=='shed'){runtime.windowAt(-width/2+13,-21,!idle);runtime.windowAt(width/2-30,-21,!idle);}
    if(kind==='workshop'||kind==='repair'){runtime.rect(width/2-24,-97,14,49,'#8f8973');runtime.rect(width/2-27,-99,20,5,'#b0a88e');runtime.rect(width/2-21,-91,5,8,'#bcb49c');}
    runtime.roof(-width/2-6,-height-18,width+12,45,color);
    runtime.rect(-15,-height+8,30,19,'#71563e');runtime.rect(-13,-height+10,26,15,'#f2deb0');
    runtime.drawContext.strokeStyle='#8a704c';runtime.drawContext.lineWidth=2;runtime.drawContext.beginPath();
    if(kind==='library'){runtime.drawContext.rect(-8,-height+14,7,8);runtime.drawContext.rect(1,-height+14,7,8);}
    else if(kind==='post'){runtime.drawContext.rect(-8,-height+13,17,10);runtime.drawContext.moveTo(-8,-height+13);runtime.drawContext.lineTo(0,-height+19);runtime.drawContext.lineTo(9,-height+13);}
    else if(kind==='gate'){runtime.drawContext.moveTo(-7,-height+18);runtime.drawContext.lineTo(-2,-height+22);runtime.drawContext.lineTo(7,-height+13);}
    else if(kind==='hall'){runtime.drawContext.rect(-8,-height+17,6,6);runtime.drawContext.rect(2,-height+17,6,6);runtime.drawContext.rect(-6,-height+13,2,2);runtime.drawContext.rect(4,-height+13,2,2);}
    else {runtime.drawContext.moveTo(-7,-height+13);runtime.drawContext.lineTo(7,-height+24);runtime.drawContext.moveTo(5,-height+13);runtime.drawContext.lineTo(-6,-height+24);}
    runtime.drawContext.stroke();
    if(kind==='workshop'){runtime.rect(22,43,39,5,'#977044');runtime.rect(25,48,4,13,'#795d3e');runtime.rect(54,48,4,13,'#795d3e');runtime.rect(30,39,12,4,'#c5b18b');runtime.crate(-width/2-23,18);}
    if(kind==='post'){runtime.crate(width/2+4,9);runtime.crate(width/2+24,19,true);runtime.rect(-width/2-19,-4,9,29,'#8f7351');runtime.rect(-width/2-23,-9,17,13,'#9b7750');}
    if(kind==='shop'){runtime.rect(-width/2+7,-8,width-14,6,'#9e8b5a');for(let stripe=0;stripe<width-14;stripe+=14)runtime.rect(-width/2+7+stripe,-8,7,8,'#dcd0a4');runtime.crate(width/2+4,21,true);}
    runtime.bush(-width/2-7,28,true);runtime.bush(width/2+5,31,false);
    runtime.drawContext.restore();
  }

export function person(runtime, left,top,time=0,color='#6d8b98',walking=false,seed=0) {
    const stride=walking?Math.floor(time*5+seed)%2:0;
    runtime.rect(left-5,top+18,13,3,'#51633b33');runtime.rect(left-3,top+10,4,8+stride*2,'#665d48');runtime.rect(left+3,top+10,4,10-stride*2,'#665d48');
    runtime.rect(left-4,top-1,12,13,color);runtime.rect(left-7,top+1,3,9,'#d5b58b');runtime.rect(left+8,top+1,3,8,'#d5b58b');runtime.rect(left-2,top-11,9,10,'#e5c497');runtime.rect(left+5,top-8,2,2,'#5a4c37');runtime.rect(left-3,top-13,10,4,'#7b6242');runtime.rect(left-6,top-12,16,3,'#c2a56a');runtime.rect(left-2,top-17,9,5,'#d8bc7c');
  }
