export const projectSeeds = [
    {id:'orca',name:'orca-factory',short:'工',description:'让协作有序发生',color:'#b87951'},
    {id:'zhiyun',name:'zhiyun-ai-dashboard',short:'云',description:'让每次调用更清晰',color:'#8d9370'},
    {id:'control',name:'ziikoo-control-plane',short:'枢',description:'小镇的调度中枢',color:'#ae7758'},
    {id:'app',name:'gpt-ziikoo-app',short:'知',description:'今天，休息一下也很好',color:'#77918b'}
  ];
export const ticketSeeds = [
    {id:'OF-126',project:'orca',title:'让排队状态更清晰',stage:'pickup',tone:'current',minutes:4,owner:'待领取',history:[['14:17','进入待领列表']]},
    {id:'OF-127',project:'orca',title:'重构工厂页面',stage:'work',tone:'current',minutes:12,owner:'执行器',history:[['14:20','进入施工'],['14:18','已领取'],['14:17','等待领取']]},
    {id:'OF-128',project:'orca',title:'修复部署检查',stage:'human',tone:'attention',minutes:18,owner:'人工',reason:'连续修正后，验收仍未通过。自动处置已结束，等待人工确认下一步。',history:[['14:32','转交人工'],['14:30','自动处置结束'],['14:18','验收未通过']]},
    {id:'OF-129',project:'orca',title:'补全审查记录',stage:'review',tone:'current',minutes:6,owner:'审查员',history:[['14:26','开始审查'],['14:24','验收通过']]},
    {id:'OF-130',project:'orca',title:'整理项目说明',stage:'pack',tone:'current',minutes:2,owner:'执行器',history:[['14:30','准备 PR'],['14:27','审查完成']]},
    {id:'OF-131',project:'orca',title:'支持新的任务入口',stage:'pickup',tone:'attention',minutes:24,owner:'待领取',reason:'依赖票 OF-127 尚未完成，暂不进入施工。',history:[['14:08','等待依赖完成']]},
    {id:'ZY-205',project:'zhiyun',title:'优化调用统计',stage:'work',tone:'current',minutes:8,owner:'执行器',history:[['14:24','进入施工']]},
    {id:'ZY-206',project:'zhiyun',title:'更新用量面板',stage:'dispatch',tone:'failed',minutes:15,owner:'处置 Agent',reason:'部署检查未通过，后续发货暂时暂停。当前为演示异常状态。',history:[['14:17','部署暂停'],['14:14','开始部署']]},
    {id:'ZY-207',project:'zhiyun',title:'优化表格筛选',stage:'gate',tone:'current',minutes:3,owner:'执行器',history:[['14:29','进入验收']]},
    {id:'ZCP-86',project:'control',title:'完善工作区索引',stage:'merge',tone:'current',minutes:9,owner:'合并队列',queue:1,history:[['14:23','进入合并队列']]},
    {id:'ZCP-87',project:'control',title:'补充运行摘要',stage:'merge',tone:'current',minutes:5,owner:'合并队列',queue:2,history:[['14:27','进入合并队列']]},
    {id:'ZCP-88',project:'control',title:'调整通知文案',stage:'work',tone:'current',minutes:7,owner:'执行器',history:[['14:25','进入施工']]}
  ];
