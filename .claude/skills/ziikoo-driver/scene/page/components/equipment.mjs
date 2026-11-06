export function belt(runtime, points,width,branch=false) {
    for(const [thickness,color] of [[width+10,'#655a43'],[width+6,branch?'#b28b52':'#b6a477'],[width,branch?'#9a8760':'#758179'],[width-6,branch?'#b6a071':'#929d8e']]){
      runtime.drawContext.strokeStyle=color;runtime.drawContext.lineWidth=thickness;runtime.drawContext.lineJoin='round';runtime.drawContext.beginPath();
      points.forEach((point,index)=>index?runtime.drawContext.lineTo(point.x,point.y):runtime.drawContext.moveTo(point.x,point.y));
      runtime.drawContext.stroke();
    }
    if(branch)runtime.beltRollers(points,width,0,branch);
  }

export function beltRollers(runtime, points,width,offset,branch=false) {
    for(let index=1;index<points.length;index++){
      const from=points[index-1],to=points[index],length=Math.hypot(to.x-from.x,to.y-from.y),horizontal=from.y===to.y;
      for(let distance=offset;distance<length;distance+=17){
        const left=from.x+(to.x-from.x)*distance/length,top=from.y+(to.y-from.y)*distance/length;
        runtime.rect(left-(horizontal?1:width/2-3),top-(horizontal?width/2-3:1),horizontal?2:width-6,horizontal?width-6:2,branch?'#89734f':'#62766d');
        runtime.rect(left+(horizontal?2:0),top+(horizontal?0:2),horizontal?1:width-7,horizontal?width-7:1,branch?'#d3b984':'#bac4aa');
      }
    }
  }

export function arrow(runtime, left,top,direction,color='#eee4bd') {
    runtime.drawContext.save();runtime.drawContext.translate(left,top);runtime.drawContext.rotate(direction);
    runtime.rect(-7,-2,11,4,color);runtime.rect(1,-6,3,12,color);runtime.rect(4,-4,3,8,color);runtime.rect(7,-2,3,4,color);
    runtime.drawContext.restore();
  }

export function stationPolygon(runtime, points,color) {
    runtime.drawContext.fillStyle=color;
    runtime.drawContext.beginPath();
    points.forEach(([left,top],index)=>index?runtime.drawContext.lineTo(left,top):runtime.drawContext.moveTo(left,top));
    runtime.drawContext.closePath();
    runtime.drawContext.fill();
  }

export function stationGauge(runtime, left,top,needle=0,color='#657f65') {
    const rim='#465856';
    runtime.rect(left-7,top-10,14,20,rim);
    runtime.rect(left-10,top-7,20,14,rim);
    runtime.rect(left-6,top-8,12,16,'#d2c6a0');
    runtime.rect(left-8,top-6,16,12,'#ede3bc');
    runtime.rect(left-5,top-5,2,3,'#ac7055');
    runtime.rect(left+4,top-5,2,3,color);
    runtime.rect(left-1,top-7,2,2,'#66736a');
    runtime.rect(left-6,top,2,2,'#66736a');
    runtime.rect(left+5,top,2,2,'#66736a');
    runtime.stationPolygon([[left-1,top+2],[left+needle-1,top-5],[left+needle+2,top-5],[left+2,top+2]],'#4b5b53');
    runtime.rect(left-2,top-1,4,4,'#a4734c');
  }

export function workbench(runtime, id,station,time=0) {
    const occupied=runtime.occupancy ? runtime.occupancy.get(runtime.state.project)?.has(id) : runtime.activeTickets(runtime.state.project).some(ticket=>ticket.stage===id);
    const motion=runtime.state.reduced?0:time;
    const beat=occupied?Math.floor(motion*3)%2:0;
    const light=occupied?'#9bc481':'#7b8473';
    let workerLeft=-64;
    runtime.drawContext.save();
    runtime.drawContext.translate(Math.round(station.x),Math.round(station.y));
    switch(id) {
      case 'pickup': {
        runtime.rect(-60,-29,114,5,'#6e58312e');
        runtime.rect(-52,-79,6,51,'#705037');
        runtime.rect(-3,-79,6,51,'#705037');
        runtime.rect(-59,-95,68,43,'#654a35');
        runtime.rect(-55,-91,60,35,'#b18b54');
        runtime.rect(-55,-91,60,3,'#d2af75');
        runtime.rect(-59,-98,68,5,'#d1af75');
        runtime.rect(-48,-83,19,23,'#f5e8bd');
        runtime.rect(-24,-86,21,16,'#e6d49a');
        runtime.rect(-20,-65,19,8,'#c5d0a1');
        for(let row=0;row<3;row++)runtime.rect(-44,-76+row*4,11,1,'#ae9365');
        runtime.rect(-18,-80,12,2,'#ad976c');
        runtime.rect(-42,-85,4,4,'#ad6650');
        runtime.rect(-17,-88,4,4,'#6e8f86');
        runtime.rect(-11,-67,3,3,'#bd8b4c');
        runtime.rect(16,-66,5,37,'#6c4e36');
        runtime.rect(61,-66,5,37,'#6c4e36');
        runtime.rect(11,-47,60,5,'#9c7549');
        runtime.rect(11,-29,60,5,'#9c7549');
        runtime.rect(14,-47,54,2,'#ddbd7e');
        runtime.rect(14,-29,54,2,'#ddbd7e');
        runtime.crate(20,-65);
        runtime.crate(43,-60,true);
        runtime.crate(23,-43,true);
        runtime.crate(45,-43,true);
        runtime.rect(58,-69,9,5,'#d9be7b');
        runtime.rect(60,-68,5,3,light);
        break;
      }
      case 'work': {
        runtime.rect(-57,-28,116,6,'#6a4c382e');
        runtime.rect(-50,-94,22,42,'#645d54');
        runtime.rect(-54,-99,30,7,'#a19780');
        runtime.rect(-47,-91,16,4,'#b1a48b');
        runtime.rect(-47,-82,16,3,'#8d826f');
        runtime.rect(-39,-89,2,8,'#484e49');
        runtime.rect(-48,-70,58,39,'#64645a');
        runtime.rect(-54,-58,70,29,'#68665b');
        runtime.rect(-46,-73,54,9,'#9f9d87');
        runtime.rect(-51,-61,14,11,'#aaa48b');
        runtime.rect(-34,-62,13,9,'#8a8c79');
        runtime.rect(-18,-62,13,10,'#b4aa8d');
        runtime.rect(-2,-60,14,10,'#92917a');
        runtime.rect(-51,-47,12,13,'#8f927f');
        runtime.rect(0,-46,13,12,'#a9a28a');
        runtime.rect(-35,-52,32,24,'#46463d');
        runtime.rect(-31,-56,24,30,'#3b3b35');
        runtime.rect(-31,-43,24,14,occupied?'#b45736':'#6d4937');
        runtime.rect(-27,-47-beat*3,7,17+beat*3,occupied?'#ea9c49':'#9c6841');
        runtime.rect(-17,-49+beat*2,6,19-beat*2,occupied?'#f5cb6a':'#b3834f');
        runtime.rect(-23,-38,7,8,occupied?'#f8e3a0':'#c19758');
        runtime.rect(-39,-29,42,5,'#aca18a');
        runtime.rect(-30,-28,26,3,'#786853');
        runtime.stationPolygon([[17,-42],[40,-45],[44,-31],[17,-28]],'#76513a');
        runtime.stationPolygon([[18,-44],[39,-49],[42,-44],[18,-38]],'#bf8b57');
        for(let fold=0;fold<3;fold++)runtime.rect(22+fold*6,-40,2,9,'#503f32');
        runtime.rect(9,-40,9,4,'#8d8871');
        runtime.rect(42,-55,12,30,'#796247');
        runtime.rect(35,-30,27,6,'#ad8755');
        runtime.stationPolygon([[24,-65],[61,-65],[70,-61],[60,-56],[53,-56],[52,-46],[61,-42],[61,-38],[32,-38],[32,-42],[40,-47],[40,-55],[27,-57]],'#455e60');
        runtime.rect(28,-66,34,4,'#a6b7ab');
        runtime.rect(41,-55,11,4,'#6e8b88');
        runtime.rect(53,-78,4,13,'#b68956');
        runtime.rect(47,-82,16,7,'#687b78');
        runtime.rect(48,-82,14,2,'#adb8a6');
        break;
      }
      case 'gate': {
        runtime.rect(-61,7,22,6,'#6a655033');
        runtime.rect(40,7,22,6,'#6a655033');
        runtime.rect(-56,-84,13,93,'#455b5c');
        runtime.rect(43,-84,13,93,'#455b5c');
        runtime.rect(-53,-80,5,83,'#91aaa2');
        runtime.rect(46,-80,5,83,'#7b958e');
        runtime.rect(-59,-87,118,17,'#405654');
        runtime.rect(-55,-84,110,10,'#829c90');
        runtime.rect(-55,-86,110,3,'#c2cbb0');
        runtime.rect(-38,-70,5,9,'#4b6461');
        runtime.rect(33,-70,5,9,'#4b6461');
        runtime.rect(-58,-1,18,9,'#566a65');
        runtime.rect(40,-1,18,9,'#566a65');
        for(const left of [-55,46]){
          runtime.rect(left,-23,7,4,'#e1bf69');
          runtime.rect(left,-15,7,4,'#e1bf69');
          runtime.rect(left,-7,7,4,'#e1bf69');
        }
        runtime.rect(-13,-97,26,25,'#60756b');
        runtime.stationGauge(0,-85,occupied?3:-3);
        runtime.rect(18,-82,9,6,light);
        runtime.rect(31,-82,9,6,occupied?'#e3c36e':'#8b8060');
        runtime.rect(-40,-82,7,6,'#bc7758');
        runtime.rect(-30,-82,7,6,'#d5bf75');
        runtime.rect(-58,-54,18,23,'#3e5250');
        runtime.rect(-55,-51,12,10,'#b6cda8');
        runtime.rect(-53,-49,3,6,'#618461');
        runtime.rect(-49,-47,4,2,'#618461');
        runtime.rect(-54,-37,4,3,light);
        runtime.rect(-47,-37,4,3,'#c19454');
        runtime.rect(37,-49,5,14,'#aac6b5');
        runtime.rect(-42,-49,5,14,'#aac6b5');
        workerLeft=-68;
        break;
      }
      case 'review': {
        runtime.rect(-59,-28,119,5,'#634c362b');
        runtime.rect(30,-98,36,68,'#674c3b');
        runtime.rect(34,-94,28,60,'#99724c');
        runtime.rect(27,-99,42,5,'#ba9160');
        for(let shelf=0;shelf<3;shelf++){
          const top=-90+shelf*18;
          runtime.rect(32,top+13,32,4,'#d0a16a');
          for(let book=0;book<4;book++){
            const colors=['#7c968b','#b36650','#d1b36c','#8393a2'];
            runtime.rect(35+book*6,top+(book%2)*3,5,13-(book%2)*3,colors[(book+shelf)%4]);
            runtime.rect(36+book*6,top+8,3,1,'#e5cc96');
          }
        }
        runtime.rect(-48,-50,7,25,'#6d4935');
        runtime.rect(9,-50,7,25,'#6d4935');
        runtime.stationPolygon([[-59,-52],[-41,-73],[22,-63],[7,-40]],'#674a38');
        runtime.stationPolygon([[-59,-57],[-41,-78],[22,-68],[7,-45]],'#b98c59');
        runtime.stationPolygon([[-55,-57],[-40,-74],[16,-66],[5,-49]],'#dbb87b');
        runtime.stationPolygon([[-43,-61],[-33,-74],[-12,-67],[-19,-53]],'#f8eccc');
        runtime.stationPolygon([[-19,-53],[-12,-67],[9,-68],[2,-53]],'#e7d6a6');
        runtime.stationPolygon([[-21,-55],[-14,-68],[-11,-67],[-18,-53]],'#a38056');
        for(let line=0;line<3;line++){
          runtime.rect(-35+line*2,-67+line*4,12,1,'#b7a17a');
          runtime.rect(-7-line*2,-64+line*4,11,1,'#ae9873');
        }
        runtime.rect(-61,-86,4,32,'#666c51');
        runtime.rect(-68,-55,18,4,'#6c6d50');
        runtime.stationPolygon([[-70,-87],[-66,-96],[-53,-96],[-47,-87]],'#648778');
        runtime.rect(-70,-87,23,3,'#405e55');
        runtime.rect(-65,-84,13,3,occupied?'#f3d890':'#c1af79');
        runtime.rect(14,-43,12,5,'#8b6350');
        runtime.rect(17,-51,6,8,'#9b6d50');
        runtime.rect(18,-51,4,2,'#e4d4a9');
        workerLeft=-38;
        break;
      }
      case 'pack': {
        runtime.rect(-61,-29,126,5,'#6f52322b');
        runtime.rect(-47,-48,7,23,'#765238');
        runtime.rect(37,-48,7,23,'#765238');
        runtime.rect(-53,-55,104,10,'#765238');
        runtime.rect(-55,-59,109,6,'#ddbd83');
        runtime.rect(-51,-53,100,3,'#b38b55');
        runtime.rect(-39,-79,6,22,'#6f786c');
        runtime.rect(34,-79,6,22,'#6f786c');
        runtime.rect(-42,-86,85,5,'#655c45');
        runtime.rect(-36,-91,68,21,'#b58a52');
        runtime.rect(-36,-89,68,13,'#d9b77a');
        runtime.rect(-36,-88,68,4,'#efcf91');
        runtime.ellipse(-35,-81,8,11,'#8d693d');
        runtime.ellipse(-35,-81,5,8,'#e1bc7d');
        runtime.ellipse(-35,-81,2,3,'#795c3a');
        runtime.ellipse(31,-81,7,11,'#e8c88c');
        runtime.ellipse(31,-81,3,5,'#aa7d48');
        runtime.stationPolygon([[-24,-74],[26,-74],[35,-59],[-20,-54]],'#e6c98d');
        runtime.rect(-19,-69,43,2,'#f2db9c');
        runtime.rect(6,-64,2,9,'#bc965e');
        runtime.rect(-43,-53,23,16,'#bd8a51');
        runtime.rect(-43,-53,23,3,'#e7bc7a');
        runtime.rect(-34,-53,5,16,'#f0d69b');
        runtime.rect(45,-49,22,22,'#af7b47');
        runtime.rect(47,-47,18,17,'#c89d62');
        runtime.rect(54,-49,5,22,'#ebd29a');
        runtime.stationPolygon([[44,-49],[40,-56],[53,-59],[57,-49]],'#e3bb79');
        runtime.stationPolygon([[57,-49],[62,-58],[72,-53],[67,-46]],'#d5a365');
        runtime.rect(13,-51,14,3,'#596f68');
        runtime.rect(14,-54,3,7,'#829487');
        runtime.rect(25,-54,3,7,'#829487');
        break;
      }
      case 'merge': {
        runtime.rect(-56,-29,109,5,'#5f59312b');
        for(let bay=0;bay<3;bay++){
          const left=-54+bay*31;
          runtime.rect(left,-78,26,49,'#484e47');
          runtime.rect(left+3,-76,20,45,'#858f7a');
          runtime.rect(left+2,-74,3,43,'#c1c4a3');
          runtime.rect(left+21,-74,3,43,'#b2b99b');
          for(let roller=0;roller<6;roller++){
            runtime.rect(left+5,-70+roller*6,15,3,'#596c60');
            runtime.rect(left+5,-70+roller*6,15,1,'#b0bea3');
          }
          runtime.rect(left+4,-87,18,8,'#766242');
          for(let mark=0;mark<=bay;mark++)runtime.rect(left+7+mark*4,-85,2,4,'#f0d69c');
          runtime.rect(left,-31,26,4,'#bd995d');
        }
        runtime.rect(53,-95,7,70,'#5c6659');
        runtime.rect(54,-93,2,66,'#bac1a0');
        runtime.rect(48,-29,17,5,'#6e7662');
        runtime.rect(45,-93,23,21,'#40524c');
        runtime.rect(48,-90,7,7,occupied?'#dbac62':'#827557');
        runtime.rect(58,-81,7,7,light);
        runtime.stationPolygon(occupied?[[57,-65],[73,-65],[73,-59],[57,-59]]:[[57,-65],[68,-76],[73,-71],[61,-60]],'#b06b4b');
        runtime.rect(55,-65,5,5,'#e6c98a');
        runtime.rect(-55,-97,88,4,'#c7bb88');
        break;
      }
      case 'dispatch': {
        runtime.rect(-56,-36,119,12,'#725035');
        for(let plank=0;plank<9;plank++){
          runtime.rect(-54+plank*13,-39,11,11,'#b08b58');
          runtime.rect(-53+plank*13,-39,10,2,'#ddbc7d');
          runtime.rect(-49+plank*13,-32,2,2,'#7a6040');
        }
        runtime.rect(-49,-96,12,60,'#4a6666');
        runtime.rect(-46,-94,5,54,'#87a29a');
        runtime.rect(-56,-42,26,6,'#48605a');
        runtime.rect(-53,-100,100,10,'#4c6763');
        runtime.rect(-49,-99,93,3,'#b7c0a1');
        runtime.stationPolygon([[-37,-90],[-14,-90],[-37,-65]],'#5f7c72');
        runtime.stationPolygon([[-35,-87],[-22,-87],[-35,-73]],'#cfb783');
        runtime.rect(32,-94,11,9,'#b18c50');
        runtime.rect(35,-85,3,14+beat,'#525b4d');
        runtime.rect(31,-73+beat,3,7,'#5c6655');
        runtime.rect(32,-68+beat,8,3,'#5c6655');
        runtime.rect(38,-73+beat,3,7,'#5c6655');
        runtime.rect(26,-68+beat,2,8,'#a2824f');
        runtime.rect(45,-68+beat,2,8,'#a2824f');
        runtime.stationPolygon([[27,-69+beat],[36,-75+beat],[46,-69+beat],[44,-67+beat],[36,-72+beat],[29,-67+beat]],'#84714d');
        runtime.crate(27,-61+beat);
        runtime.rect(18,-45,3,6,'#c8b17a');
        runtime.rect(52,-45,3,6,'#c8b17a');
        runtime.rect(-58,-66,25,16,'#6c7c69');
        runtime.rect(-55,-63,19,10,'#aeb99a');
        runtime.rect(-51,-60,10,3,'#697764');
        runtime.rect(-46,-64,3,12,'#697764');
        runtime.rect(-47,-59,5,5,'#d6bd7b');
        runtime.rect(-18,-43,25,4,'#d7bf83');
        runtime.rect(-13,-49,15,7,'#aa8351');
        runtime.rect(-8,-50,4,11,'#e5d19c');
        workerLeft=-67;
        break;
      }
      case 'smoke': {
        runtime.rect(-60,-29,121,5,'#6a51352b');
        runtime.rect(-56,-84,8,49,'#775a40');
        runtime.rect(-53,-82,3,46,'#e2a96c');
        runtime.rect(-56,-87,28,8,'#976946');
        runtime.rect(-54,-85,24,3,'#edba7b');
        runtime.rect(-36,-83,8,17,'#b78350');
        runtime.rect(-42,-70,12,6,'#deb178');
        runtime.rect(-38,-61,45,30,'#8f633f');
        runtime.rect(-43,-56,55,18,'#ad7848');
        runtime.rect(-37,-66,43,9,'#d9a268');
        runtime.rect(-32,-70,33,6,'#e7b97d');
        runtime.rect(-36,-60,5,25,'#edbb7c');
        runtime.rect(0,-59,5,25,'#77583c');
        runtime.rect(-42,-43,53,4,'#d3a772');
        runtime.rect(-40,-33,49,6,'#6b6d58');
        runtime.rect(-30,-53,27,16,'#5b7469');
        runtime.rect(-27,-51,21,11,'#a9c5a3');
        runtime.rect(-24,-48,4,5,light);
        runtime.rect(-17,-48,8,2,'#678e70');
        runtime.rect(-19,-84,7,16,'#b18050');
        runtime.stationGauge(-16,-87,occupied?3:-3);
        runtime.rect(20,-89,28,62,'#58675c');
        runtime.rect(23,-87,22,57,'#8a9580');
        runtime.stationGauge(34,-75,occupied?2:-4);
        runtime.stationGauge(34,-51,occupied?4:-2);
        runtime.rect(12,-44,9,7,'#c28c54');
        runtime.rect(48,-39,18,7,'#688765');
        runtime.rect(60,-50,8,15,'#809c71');
        runtime.rect(57,-52,14,5,'#acc18a');
        runtime.rect(61,-51,6,2,light);
        runtime.rect(63,-63-beat*3,5,5,'#d1d8b2');
        runtime.rect(59,-70-beat*3,7,4,'#e1dfbb');
        runtime.rect(-60,-72,17,4,'#6d7761');
        runtime.rect(-54,-78,4,16,'#6d7761');
        runtime.rect(-55,-73,6,6,'#bc9d62');
        break;
      }
      case 'repair': {
        runtime.rect(-58,-28,120,5,'#5c56352b');
        runtime.rect(-55,-72,7,43,'#596b69');
        runtime.rect(47,-72,7,43,'#596b69');
        runtime.rect(-59,-76,117,8,'#728a80');
        runtime.rect(-56,-75,111,2,'#b6bd9c');
        runtime.rect(-60,-31,18,5,'#6f7b65');
        runtime.rect(42,-31,18,5,'#6f7b65');
        runtime.rect(-31,-68,3,14,'#8d7950');
        runtime.rect(-37,-57,14,4,'#bfc8ad');
        runtime.rect(-36,-62,4,10,'#bfc8ad');
        runtime.rect(-27,-62,4,10,'#bfc8ad');
        runtime.rect(27,-68,3,21,'#c49c62');
        runtime.rect(22,-58,13,5,'#9cae9b');
        runtime.rect(28,-49,4,9,'#5c7370');
        runtime.ellipse(-4,-47,24,23,'#455a59');
        runtime.ellipse(-4,-48,20,19,'#a2b4a0');
        runtime.ellipse(-4,-48,15,14,'#5b7068');
        runtime.ellipse(-4,-48,11,10,'#c3bc90');
        runtime.rect(-6,-65,4,35,'#7b9182');
        runtime.rect(-22,-50,36,4,'#7b9182');
        runtime.stationPolygon([[-19,-59],[-16,-62],[10,-36],[7,-33]],'#8ca18c');
        runtime.stationPolygon([[-19,-36],[-16,-33],[10,-59],[7,-62]],'#8ca18c');
        runtime.ellipse(-4,-48,6,6,'#526864');
        runtime.rect(-6,-50,4,4,'#d9bf80');
        runtime.rect(-19,-27,31,4,'#6d6a50');
        runtime.rect(42,-59,19,12,'#b46e4a');
        runtime.rect(45,-56,13,7,'#d69a62');
        runtime.rect(47,-63,9,4,'#536961');
        runtime.rect(49,-61,5,4,'#cfb783');
        runtime.rect(-60,-48,15,12,'#8d9b80');
        runtime.rect(-58,-46,11,3,'#c2ccab');
        runtime.rect(-50,-45,3,6,light);
        workerLeft=-67;
        break;
      }
      case 'return': {
        runtime.ellipse(-9,-40,47,18,'#6a6750');
        runtime.ellipse(-9,-43,47,18,'#879887');
        runtime.ellipse(-9,-44,41,14,'#b5b998');
        runtime.ellipse(-9,-44,32,10,'#7b8b75');
        runtime.ellipse(-9,-44,28,7,'#a4b396');
        runtime.rect(-51,-46,84,3,'#4d6863');
        runtime.rect(-51,-39,84,3,'#4d6863');
        for(let sleeper=0;sleeper<6;sleeper++)runtime.rect(-44+sleeper*13,-49,4,17,'#d2bd83');
        runtime.rect(-52,-46,86,2,'#566e66');
        runtime.rect(-52,-38,86,2,'#566e66');
        runtime.ellipse(-9,-42,6,5,'#4f6a61');
        runtime.ellipse(-9,-43,3,2,'#d1c08a');
        runtime.rect(46,-62,6,37,'#6c755d');
        runtime.rect(41,-29,16,4,'#80846a');
        runtime.rect(35,-74,29,23,'#5a7469');
        runtime.rect(32,-70,35,15,'#5a7469');
        runtime.rect(36,-71,27,16,'#b5c69c');
        runtime.rect(43,-68,14,3,'#527a67');
        runtime.rect(55,-66,4,8,'#527a67');
        runtime.rect(42,-59,16,3,'#527a67');
        runtime.rect(40,-65,3,8,'#527a67');
        runtime.stationPolygon([[36,-64],[43,-70],[43,-58]],'#527a67');
        runtime.rect(-60,-36,12,4,'#9b7c50');
        runtime.rect(-58,-55,3,19,'#637970');
        runtime.rect(-61,-57,10,5,'#c29859');
        runtime.rect(-59,-56,5,3,light);
        workerLeft=-68;
        break;
      }
      case 'human': {
        runtime.rect(-11,-75,25,15,'#8f6650');
        runtime.rect(-8,-73,19,10,'#cda674');
        runtime.rect(-9,-61,5,13,'#765741');
        runtime.rect(7,-61,5,13,'#765741');
        runtime.rect(-60,-53,17,21,'#90674f');
        runtime.rect(-58,-51,13,14,'#d2ad79');
        runtime.rect(-58,-35,4,10,'#745741');
        runtime.rect(-47,-35,4,10,'#745741');
        runtime.rect(44,-53,17,21,'#90674f');
        runtime.rect(46,-51,13,14,'#d2ad79');
        runtime.rect(46,-35,4,10,'#745741');
        runtime.rect(57,-35,4,10,'#745741');
        runtime.rect(-24,-39,7,15,'#78543a');
        runtime.rect(18,-39,7,15,'#78543a');
        runtime.ellipse(0,-46,43,21,'#77513b');
        runtime.ellipse(0,-50,43,21,'#c59661');
        runtime.ellipse(0,-52,38,17,'#dfb982');
        runtime.rect(-28,-55,57,1,'#c49b69');
        runtime.rect(-31,-46,61,1,'#c49b69');
        runtime.stationPolygon([[-14,-61],[8,-63],[16,-52],[-7,-49]],'#f1e1b8');
        runtime.rect(-6,-59,11,2,'#b49f78');
        runtime.rect(-3,-55,11,1,'#b49f78');
        for(const [left,top] of [[-27,-59],[23,-53],[-12,-39]]){
          runtime.rect(left,top,7,7,'#779991');
          runtime.rect(left+1,top-1,5,2,'#eff0ce');
          runtime.rect(left+2,top,3,2,'#846347');
          runtime.rect(left+7,top+2,3,4,'#527d74');
          runtime.rect(left+8,top+3,1,1,'#dfb982');
        }
        runtime.rect(17,-65,8,4,'#a8784c');
        runtime.rect(20,-69,2,5,'#6f8d61');
        runtime.rect(16,-70,5,3,'#92a86e');
        runtime.rect(21,-73,5,4,'#738f5f');
        workerLeft=-66;
        break;
      }
    }
    if(occupied)runtime.person(workerLeft,-17,motion,station.branch?'#b28a5f':'#77978d',false);
    runtime.drawContext.restore();
  }
