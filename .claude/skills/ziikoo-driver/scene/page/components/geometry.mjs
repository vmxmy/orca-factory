export const stages = {
    pickup:{name:'领料台',phase:'等待领取',x:160,y:285,number:'01',description:'票据先成为待领包裹。依赖未满足时留在本站，显示等待原因。'},
    work:{name:'制作台',phase:'施工中',x:400,y:285,number:'02',description:'执行器在此处理任务。工位旁的工匠对应当前执行者。'},
    gate:{name:'验收台',phase:'验收中',x:640,y:285,number:'03',description:'执行检查。通过后进入审查，未通过则转入内侧修理支线。'},
    review:{name:'审查台',phase:'审查中',x:880,y:285,number:'04',description:'审查变更。通过后沿右侧转角进入包装，退回时转入返工台。'},
    pack:{name:'包装台',phase:'开 PR',x:880,y:610,number:'05',description:'整理变更并准备 PR。主线在这一排从右向左流转。'},
    merge:{name:'合并排队台',phase:'排队合并',x:640,y:610,number:'06',description:'包裹按队列顺序等待合并，数字标记实际演示位次。'},
    dispatch:{name:'部署台',phase:'部署中',x:400,y:610,number:'07',description:'合并后执行部署。部署异常时包裹停留，显示暂停原因。'},
    smoke:{name:'出厂检查台',phase:'冒烟检查',x:160,y:610,number:'08',description:'部署后进行 smoke 检查。检查通过才标为已完成，完成包裹留在出口。'},
    human:{name:'人工工作台',phase:'等待人工',x:160,y:442,number:'C',branch:true,description:'自动处置无法继续时交给人工。确认处理方向后，可经返工台回到制作。'},
    return:{name:'返工台',phase:'打回修改',x:400,y:442,number:'B',branch:true,description:'接收审查退回或人工确认后的任务。沿向上的回流带重新进入制作。'},
    repair:{name:'修理台',phase:'处置中',x:640,y:442,number:'A',branch:true,description:'接收验收异常。修正后经返工通道回到制作，无法解决时转交人工。'}
  };
export const mainRoute = ['pickup','work','gate','review','pack','merge','dispatch','smoke'];
export const mainBelt = [{x:92,y:285},{x:948,y:285},{x:948,y:610},{x:92,y:610}];
export const branchBelts = [
    [{x:640,y:285},{x:640,y:442},{x:160,y:442}],
    [{x:880,y:285},{x:880,y:442},{x:400,y:442}],
    [{x:400,y:442},{x:400,y:285}]
  ];
