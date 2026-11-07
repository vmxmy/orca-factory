export function projectCounts(runtime, project) {const list=runtime.activeTickets(project.id);return {total:list.length,attention:list.filter(ticket=>ticket.tone!=='current').length};}

export function updateSidebar(runtime) {
    document.querySelector('#project-total').textContent=runtime.projects.length;
    document.querySelector('#repo-list').innerHTML=runtime.projects.map(project=>{
      const count=runtime.projectCounts(project);
      return `<button class="repo-button ${runtime.state.view==='project'&&runtime.state.project===project.id?'active':''}" data-project="${runtime.escapeText(project.id)}" aria-label="查看 ${runtime.escapeText(project.name)} 院落"><span class="repo-symbol">${project.short}</span><span class="repo-info"><strong>${runtime.escapeText(project.name)}</strong><small class="${count.attention?'attention':''}">${count.total?`${count.total} 张委托${count.attention?` · ${count.attention} 项需关注`:' · 正在有序推进'}`:'当前空闲'}</small></span></button>`;
    }).join('')||'<div class="empty-state">没有项目</div>';
  }

export function updateHeader(runtime) {
    document.querySelector('.map-column').classList.toggle('interior',runtime.state.view==='project');
    document.querySelectorAll('[data-view]').forEach(button=>{const active=button.dataset.view===runtime.state.view;button.classList.toggle('active',active);button.setAttribute('aria-current',active?'page':'false');});
    document.querySelector('#attention-count').textContent=runtime.attentionTickets().length;
    document.querySelector('[data-view="project"]').disabled=!runtime.projects.length;
    document.querySelector('#map-title').textContent=runtime.state.view==='project'?runtime.projectOf(runtime.state.project).name:runtime.state.view==='attention'?'有几份委托，需要你':'今天的小镇';
    document.querySelector('#map-eyebrow').textContent=runtime.state.view==='project'?'工坊内部 / 一份委托的制作过程':runtime.state.view==='attention'?'停下来，把问题看清楚':'一切都在慢慢变好';
    document.querySelector('#map-subtitle').textContent=runtime.state.view==='project'?'沿传送带看流程；点击工位或包裹查看详情。':runtime.projects.length?`${runtime.projects.length} 座院落。拖动地图，或从列表寻找项目。`:'没有项目。调整演示数量，看看小镇的新院落。';
    document.querySelector('#motion-toggle').setAttribute('aria-pressed',String(runtime.state.reduced));
  }

export function runSearch(runtime) {
    const query=document.querySelector('#search').value.trim().toLowerCase();runtime.state.query=query;
    const resultBox=document.querySelector('#search-results');resultBox.hidden=!query;
    if(!query){resultBox.innerHTML='';runtime.needsDraw=true;return;}
    const projectMatches=project=>[project.name,project.id].some(value=>String(value).toLowerCase().includes(query));
    const matched=runtime.tickets.filter(ticket=>[ticket.id,ticket.title].some(value=>String(value).toLowerCase().includes(query))||projectMatches(runtime.projectOf(ticket.project)));
    const matchedProjects=runtime.projects.filter(projectMatches);
    resultBox.innerHTML=matchedProjects.map(project=>`<button class="search-result" data-project="${runtime.escapeText(project.id)}"><strong>${runtime.escapeText(project.name)}</strong><small>${runtime.escapeText(project.id)} · 进入工坊内部</small></button>`).join('')+matched.map(ticket=>`<button class="search-result" data-ticket="${runtime.escapeText(ticket.key ?? ticket.id)}"><strong>${runtime.escapeText(ticket.id)} · ${runtime.escapeText(ticket.title)}</strong><small>${runtime.escapeText(runtime.projectOf(ticket.project).name)} · ${ticket.completed?'已完成':runtime.stageOf(ticket).phase}</small></button>`).join('')||'<div class="empty-state">没有找到匹配的委托或项目。</div>';
    runtime.needsDraw=true;
  }

export function clearSearch(runtime){document.querySelector('#search').value='';runtime.state.query='';document.querySelector('#search-results').hidden=true;document.querySelector('#search-results').innerHTML='';}
