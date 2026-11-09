export function attentionMarkup(runtime, ticket) {
    return `<button class="attention-item" data-ticket="${runtime.escapeText(ticket.key ?? ticket.id)}" aria-label="查看 ${runtime.escapeText(ticket.id)} ${runtime.escapeText(ticket.title)}"><span class="attention-item-head"><span class="mini-flag ${ticket.tone==='failed'?'red':''}">!</span><strong>${runtime.escapeText(ticket.id)}</strong><span class="time">${runtime.escapeText(ticket.duration ?? `${ticket.minutes} 分钟`)}</span></span><p>${runtime.escapeText(ticket.note || runtime.stageOf(ticket).phase)}</p><small>${runtime.escapeText(ticket.title)}</small></button>`;
  }

export function overviewMarkup(runtime) {
    if(!runtime.projects.length)return '<div class="panel-top"><h2>小镇手记</h2></div><div class="panel-content"><div class="empty-state"><strong>暂无项目</strong>用“演示项目数量”添加院落。<br>当前没有委托或待处理事项。</div></div>';
    const focused=runtime.state.view==='project', list=runtime.activeTickets(focused?runtime.state.project:null), alerts=list.filter(runtime.needsAttention);
    return `<div class="panel-top"><h2>${focused?'院落手记':'小镇手记'}</h2><span class="eyebrow">01 / 春</span></div><div class="panel-content"><div class="chapter-label">把进度，写进今天</div><div class="journal-rule"></div><h3 class="greeting">${runtime.state.view==='attention'?'先照顾这些小插曲。':!list.length?'今天的院落，很安静。':'又是认真开工的一天。'}</h3><p class="intro-copy">${!list.length?'这里暂时没有委托。新的任务到来时，工坊会重新热闹起来。':'每个包裹都是一份委托。<br>它们正在各自的工位上，一点点向前。'}</p><div class="summary-grid"><div class="summary-cell"><strong>${list.length.toString().padStart(2,'0')}</strong><span>当前委托</span></div><div class="summary-cell attention"><strong>${alerts.length.toString().padStart(2,'0')}</strong><span>需要关注</span></div></div><div class="section-title"><h3>告示栏</h3><span>${alerts.length?'点击查看详情':'一切安好'}</span></div>${alerts.length?alerts.map(runtime.attentionMarkup).join(''):'<div class="empty-state">暂时没有需要处理的事项。</div>'}${focused&&list.length?`<div class="section-title"><h3>院落中的委托</h3><span>${list.length} 张</span></div>${list.filter(ticket=>!runtime.needsAttention(ticket)).map(ticket=>`<button class="attention-item" data-ticket="${runtime.escapeText(ticket.key ?? ticket.id)}"><span class="attention-item-head"><strong>${runtime.escapeText(ticket.id)}</strong><span class="time">${runtime.escapeText(runtime.stageOf(ticket).phase)}</span></span><p style="margin-left:0">${runtime.escapeText(ticket.title)}</p></button>`).join('')}`:''}<div class="journal-bottom">不用催促每一株嫩芽，<br>但要记得照顾需要帮助的那一株。<small>— 工坊小镇的日常</small></div></div>`;
  }

export function detailMarkup(runtime, ticket) {
    const project=runtime.projectOf(ticket.project),stage=runtime.stageOf(ticket);
    return `<div class="panel-top"><h2>委托详情</h2><button class="close-detail" data-close aria-label="关闭委托详情">×</button></div><div class="panel-content"><div class="ticket-heading"><strong class="ticket-id">${runtime.escapeText(ticket.id)}</strong><span class="state-pill ${runtime.escapeText(ticket.tone)}">${ticket.tone==='failed'?'部署暂停':runtime.escapeText(stage.phase)}</span></div><h3 class="ticket-title">${runtime.escapeText(ticket.title)}<small class="ticket-project">${runtime.escapeText(project.name)}</small></h3>${ticket.reason?`<div class="reason-box ${runtime.escapeText(ticket.tone)}"><strong><span>!</span> 需要你查看</strong><p>${runtime.escapeText(ticket.reason)}</p></div>`:''}<dl class="field-list"><div class="field-row"><dt>当前工位</dt><dd>${stage.name}</dd></div><div class="field-row"><dt>归属</dt><dd>${runtime.escapeText(ticket.owner)}</dd></div><div class="field-row"><dt>本阶段已观察</dt><dd>${runtime.escapeText(ticket.duration ?? `${ticket.minutes} 分钟`)}</dd></div>${ticket.queue?`<div class="field-row"><dt>合并位次</dt><dd>第 ${ticket.queue} 位</dd></div>`:''}<div class="field-row"><dt>数据来源</dt><dd>演示数据</dd></div></dl><div class="section-title"><h3>本次观察记录</h3><span>${ticket.history.length} 条</span></div><ol class="timeline">${ticket.history.map(([time,message])=>`<li>${runtime.escapeText(message)}<time>${runtime.escapeText(time)}</time></li>`).join('')}</ol><p class="timeline-note">这里是设计稿中的示例记录，不代表真实执行历史。</p><div class="panel-actions"><button class="paper-button" data-copy="id">复制票号</button><button class="paper-button" data-copy="detail">复制详情</button></div></div>`;
  }

export function stationMarkup(runtime) {
    const stage=runtime.stages[runtime.state.station],list=runtime.activeTickets(runtime.state.project).filter(ticket=>ticket.stage===runtime.state.station);
    return `<div class="panel-top"><h2>${stage.name}</h2><button class="close-detail" data-close aria-label="关闭工位详情">×</button></div><div class="panel-content"><div class="chapter-label">${runtime.escapeText(stage.phase)} · ${list.length} 张委托</div><div class="journal-rule"></div>${list.length?list.map(ticket=>`<button class="attention-item" data-ticket="${runtime.escapeText(ticket.key ?? ticket.id)}"><span class="attention-item-head"><strong>${runtime.escapeText(ticket.id)}</strong><span class="time">${runtime.escapeText(ticket.duration ?? `${ticket.minutes} 分钟`)}</span></span><p style="margin-left:0">${runtime.escapeText(ticket.title)}</p></button>`).join(''):'<div class="empty-state"><strong>这一站暂时空闲</strong>有委托抵达时，会在这里出现。</div>'}</div>`;
  }

export function updateDetail(runtime) {
    document.querySelector('#detail').innerHTML=runtime.state.selected?runtime.detailMarkup(runtime.ticketOf(runtime.state.selected)):runtime.state.station?runtime.stationMarkup():runtime.overviewMarkup();
    if(runtime.state.selected){
      const ticket=runtime.ticketOf(runtime.state.selected);
      if(ticket.completed)document.querySelector('.state-pill').textContent='已完成';
      runtime.actions.detail?.(runtime, ticket);
    }else if(runtime.state.station){
      document.querySelector('#detail .journal-rule').insertAdjacentHTML('afterend',`<p class="station-description">${runtime.stages[runtime.state.station].description}</p>`);
    }else if(runtime.state.view==='project'){
      document.querySelector('#detail h2').textContent='工坊手记';
      document.querySelector('#detail .intro-copy').insertAdjacentHTML('afterend',`<div class="flow-guide"><strong>主线 · 顺着箭头走</strong><p>领取 → 施工 → 验收 → 审查<br>开 PR → 合并排队 → 部署 → 出厂检查</p><strong>内侧支线 · 处理异常</strong><p>验收异常进入修理台，审查退回进入返工台；无法自动处理时交给人工。</p><p>${runtime.escapeText(runtime.actions.flowNote ?? "包裹位置来自实时快照，动画不代表真实执行进度。")}</p></div>`);
    }
    document.querySelector('#detail-panel').classList.toggle('is-drawer',Boolean(runtime.state.selected||runtime.state.station));
    document.querySelector('.map-column').classList.toggle('drawer-open',Boolean(runtime.state.selected||runtime.state.station));
    document.querySelector('.app').classList.toggle('has-selection',Boolean(runtime.state.selected||runtime.state.station));
  }
