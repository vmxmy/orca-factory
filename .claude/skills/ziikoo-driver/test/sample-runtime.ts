export const NOW = Date.parse('2026-10-27T12:00:00Z')

export const SAMPLE_STATUS = {
  state: 'ok',
  generatedAt: '2026-10-27T11:59:52Z',
  tickets: [
    { repo: '/home/laoxu/github/orca-factory', identifier: 'OF-1', owner: '执行器', phase: 'gate', backend: 'feishu', title: '流水线冒烟（OF）', since: '2026-10-27T11:56:00Z' },
    { repo: '/home/laoxu/github/orca-factory', identifier: 'OF-2', owner: '执行器', phase: '写 correction', backend: 'feishu', title: '修正审查意见', since: '2026-10-27T11:58:30Z' },
    {
      repo: '/home/laoxu/github/orca-factory',
      identifier: 'OF-3',
      owner: '合并通道',
      phase: '已合并',
      backend: 'feishu',
      title: '自部署验证',
      since: '2026-10-27T09:55:00Z',
      deployment: { queueId: 'queue-1', sha: 'abc1234def567', state: 'smoke-intent' },
    },
    { repo: '/home/laoxu/github/ziikoo-web', identifier: 'ZW-7', owner: '执行器', phase: 'worker 运行中', title: '改登录页', since: '2026-10-27T11:59:30Z' },
    { repo: '/home/laoxu/github/ziikoo-web', identifier: 'ZW-8', owner: '无', phase: '待领', stage: 'candidate', blockedReason: 'blocked by ZW-7' },
  ],
}

export const SAMPLE_DOCTOR = {
  checks: {
    version: { status: 'ok', reason: '运行版本与 factory origin/main 一致', details: {} },
    checkouts: {
      status: 'warn',
      reason: 'ziikoo-web 检出在 feat/login，不在 main；ansible 有未提交改动',
      details: {
        projects: [
          { name: 'orca-factory', path: '/home/laoxu/github/orca-factory', branch: 'main', clean: true, behind: 0 },
          { name: 'ziikoo-web', path: '/home/laoxu/github/ziikoo-web', branch: 'feat/login', clean: true, behind: 0 },
          { name: 'ansible', path: '/home/laoxu/ansible', branch: 'main', clean: false, behind: 2 },
        ],
      },
    },
    sync_prs: { status: 'warn', reason: 'ansible #12 待合并', details: { prs: [{ project: 'ansible', number: 12 }] } },
  },
  errors: [],
}
