import { test, expect } from 'claude-code/testing'
import { buildThroughput, renderThroughput, type LedgerRow, type Run } from './throughput'

test('counts merged completions in 24 rolling hourly buckets', () => {
  const now = Date.parse('2026-10-05T12:00:00Z')
  const r = buildThroughput({
    now,
    ledger: [
      { outcome: 'merged', finished_at: '2026-10-05T11:10:00Z' },
      { outcome: 'merged', finished_at: '2026-10-05T11:40:00Z' },
    ],
    runs: [],
  })
  expect(r.hourly.length).toBe(24)
  expect(r.hourly[23]).toBe(2)

  const boundaries = buildThroughput({
    now,
    ledger: [
      { outcome: 'merged', finished_at: '2026-10-05T12:00:00Z' },
      { outcome: 'merged', finished_at: '2026-10-05T11:00:00Z' },
      { outcome: 'merged', finished_at: '2026-10-04T12:00:00.001Z' },
      { outcome: 'merged', finished_at: '2026-10-04T12:00:00Z' },
      { outcome: 'merged', finished_at: '2026-10-05T12:00:00.001Z' },
      { outcome: 'merged', finished_at: 'invalid' },
      { outcome: 'escalated', finished_at: '2026-10-05T11:40:00Z' },
    ],
    runs: [],
  })
  expect(boundaries.hourly[23]).toBe(1)
  expect(boundaries.hourly[22]).toBe(1)
  expect(boundaries.hourly[0]).toBe(1)
  expect(boundaries.hourly.reduce((sum, count) => sum + count, 0)).toBe(3)

  const ledger: LedgerRow[] = [
    { stage_secs: { queue: 10, worker: 30, gate: 4, review: 5, merge: 6 } },
    { stage_secs: { queue: 20, worker: 50, gate: NaN, review: Infinity } },
    { stage_secs: { queue: 30 } },
    { stage_secs: { queue: 40 } },
    { outcome: 'merged' },
  ]
  for (const row of ledger) {
    if (row.stage_secs) Object.freeze(row.stage_secs)
    Object.freeze(row)
  }
  Object.freeze(ledger)
  const data = buildThroughput({ now: 0, ledger, runs: [] })
  expect(data.stages).toEqual({
    queue: { p50: 25, p90: 40, n: 4 },
    worker: { p50: 40, p90: 50, n: 2 },
    gate: { p50: 4, p90: 4, n: 1 },
    review: { p50: 5, p90: 5, n: 1 },
    merge: { p50: 6, p90: 6, n: 1 },
  })
})

test('keeps null legacy stages, four reasons and recent precheck averages', () => {
  const legacy = buildThroughput({ now: 0, ledger: [{ outcome: 'merged' }], runs: [] })
  expect(legacy.stages).toBeNull()
  expect(legacy.noTicket).toEqual({ noClaimable: 0, scopeConflict: 0, atCap: 0, rateLimited: 0 })
  expect(legacy.inFlightAvg).toBeNull()
  expect(buildThroughput({ now: 0, ledger: [{ stage_secs: { queue: NaN, worker: Infinity } }], runs: [] }).stages)
    .toBeNull()
  // Recorded CLI fixture mirrored here because mod tests cannot import node:fs.
  const fixture: { result: { runs: Run[] } } = {
 "ok": true,
 "result": {
  "runs": [
   {
    "_note": "idle: no claimable ticket, nothing in flight",
    "id": "9a26de78-55e1-459b-b581-0525cf5700d9",
    "status": "skipped_precheck",
    "trigger": "scheduled",
    "createdAt": 1791202851729,
    "scheduledFor": 1791202800000,
    "startedAt": 1791202851754,
    "dispatchedAt": null,
    "precheckResult": {
     "completedAt": 1791202873866,
     "durationMs": 22083,
     "exitCode": 1,
     "startedAt": 1791202851783,
     "stderr": "ip /home/user/orca/workspaces/orca-factory/ZII-843-main: ticket ZII-843 is not confirmed completed (in flight or missing from Linear snapshot)\nskip /home/user/github/orca-factory/.claude/worktrees/agent-a282baf4415e23c72: head has no matching merged PR\nskip /home/user/github/orca-factory/.claude/worktrees/agent-a2c2615f8e538f49b: head has no matching merged PR\nskip /home/user/github/orca-factory/.claude/worktrees/agent-a5c198c8f95b1db79: head has no matching merged PR\nskip /home/user/github/orca-factory/.claude/worktrees/agent-a8cff1924fd026894: head has no matching merged PR\nskip /home/user/github/orca-factory/.claude/worktrees/agent-ad917f6deffc63602: head has no matching merged PR\nskip agent/ZII-807/board: head has no matching merged PR\nskip orca-factory-setup-script: head has no matching merged PR\nskip worktree-agent-a03bf72b9b124bfa9: head has no matching merged PR\nskip worktree-agent-a08199cb8f7322fb4: head has no matching merged PR\nskip worktree-agent-a0fc2414f81e07fa4: head has no matching merged PR\nskip worktree-agent-a1be12a12c9586710: head has no matching merged PR\nskip worktree-agent-a282baf4415e23c72: head has no matching merged PR\nskip worktree-agent-a2c2615f8e538f49b: head has no matching merged PR\nskip worktree-agent-a37780aec88640c36: head has no matching merged PR\nskip worktree-agent-a48603a9d9b6d9ed9: head has no matching merged PR\nskip worktree-agent-a527c3b10d9c38de4: head has no matching merged PR\nskip worktree-agent-a543cced7ebc04444: head has no matching merged PR\nskip worktree-agent-a5c198c8f95b1db79: head has no matching merged PR\nskip worktree-agent-a62631f9bb53373c4: head has no matching merged PR\nskip worktree-agent-a66d5384a518080b1: head has no matching merged PR\nskip worktree-agent-a67dee6ef95e4226e: head has no matching merged PR\nskip worktree-agent-a6807bf14c9ab3582: head has no matching merged PR\nskip worktree-agent-a722b4612bf0bd6e8: head has no matching merged PR\nskip worktree-agent-a744e60c2e151655b: head has no matching merged PR\nskip worktree-agent-a7bc1966af3ea992f: head has no matching merged PR\nskip worktree-agent-a7c5be6112d67b9f6: head has no matching merged PR\nskip worktree-agent-a7e6803a56c313576: head has no matching merged PR\nskip worktree-agent-a7eed5dbd6d8b8a16: head has no matching merged PR\nskip worktree-agent-a889bc3a6632e4fde: head has no matching merged PR\nskip worktree-agent-a8aceafc30db1a83f: head has no matching merged PR\nskip worktree-agent-a8cff1924fd026894: head has no matching merged PR\nskip worktree-agent-a9776e12212ee8f1c: head has no matching merged PR\nskip worktree-agent-a9ca4fbdfd8aa33d4: head has no matching merged PR\nskip worktree-agent-a9feb76bee7001440: head has no matching merged PR\nskip worktree-agent-aa9e27467b99addaf: head has no matching merged PR\nskip worktree-agent-aab212e32bc0dc128: head has no matching merged PR\nskip worktree-agent-ab1a811516d621303: head has no matching merged PR\nskip worktree-agent-ab3ed27e0a9d550c1: head has no matching merged PR\nskip worktree-agent-ac0dec120b3f0435a: head has no matching merged PR\nskip worktree-agent-ad917f6deffc63602: head has no matching merged PR\nskip worktree-agent-af45bebf579e1ca83: head has no matching merged PR\nskip agent/ZII-807/board: remote head has no matching merged PR\nskip agent/ZII-808/lane2: remote head has no matching merged PR\nskip agent/ZII-810/impl: remote head has no matching merged PR\nskip agent/ZII-813/main: remote head has no matching merged PR\nskip agent/ZII-817/impl: remote head has no matching merged PR\nskip agent/ZII-818/end-reason: remote head has no matching merged PR\nskip agent/ZII-819/main: remote head has no matching merged PR\nskip agent/ZII-822/local-branch-cleanup: remote head has no matching merged PR\nskip agent/ZII-823/main: remote head has no matching merged PR\nskip agent/ZII-825/main: remote head has no matching merged PR\nskip agent/ZII-843/main: ticket ZII-843 is not confirmed completed (in flight or missing from Linear snapshot)\nprune_linear_calls=0\nlinear_calls=2\n",
     "stdout": "claimable= in_flight=0 max=10\nstale=\nhealth=live=0 max=10 released=0 escalated=0 failed_backlog=0\n",
     "timedOut": false
    }
   },
   {
    "_note": "busy: no claimable ticket, 1 in flight (below cap)",
    "id": "83a13f7b-af3d-4bfb-9545-5a0e60b878fa",
    "status": "skipped_precheck",
    "trigger": "scheduled",
    "createdAt": 1791203751734,
    "scheduledFor": 1791203700000,
    "startedAt": 1791203751790,
    "dispatchedAt": null,
    "precheckResult": {
     "completedAt": 1791203772720,
     "durationMs": 20877,
     "exitCode": 1,
     "startedAt": 1791203751843,
     "stderr": "missing from Linear snapshot)\nskip /home/user/github/orca-factory/.claude/worktrees/agent-a282baf4415e23c72: head has no matching merged PR\nskip /home/user/github/orca-factory/.claude/worktrees/agent-a2c2615f8e538f49b: head has no matching merged PR\nskip /home/user/github/orca-factory/.claude/worktrees/agent-a5c198c8f95b1db79: head has no matching merged PR\nskip /home/user/github/orca-factory/.claude/worktrees/agent-a8cff1924fd026894: head has no matching merged PR\nskip /home/user/github/orca-factory/.claude/worktrees/agent-ad917f6deffc63602: head has no matching merged PR\nskip agent/ZII-807/board: head has no matching merged PR\nskip orca-factory-setup-script: head has no matching merged PR\nskip worktree-agent-a03bf72b9b124bfa9: head has no matching merged PR\nskip worktree-agent-a08199cb8f7322fb4: head has no matching merged PR\nskip worktree-agent-a0fc2414f81e07fa4: head has no matching merged PR\nskip worktree-agent-a1be12a12c9586710: head has no matching merged PR\nskip worktree-agent-a282baf4415e23c72: head has no matching merged PR\nskip worktree-agent-a2c2615f8e538f49b: head has no matching merged PR\nskip worktree-agent-a37780aec88640c36: head has no matching merged PR\nskip worktree-agent-a48603a9d9b6d9ed9: head has no matching merged PR\nskip worktree-agent-a527c3b10d9c38de4: head has no matching merged PR\nskip worktree-agent-a543cced7ebc04444: head has no matching merged PR\nskip worktree-agent-a5c198c8f95b1db79: head has no matching merged PR\nskip worktree-agent-a62631f9bb53373c4: head has no matching merged PR\nskip worktree-agent-a66d5384a518080b1: head has no matching merged PR\nskip worktree-agent-a67dee6ef95e4226e: head has no matching merged PR\nskip worktree-agent-a6807bf14c9ab3582: head has no matching merged PR\nskip worktree-agent-a722b4612bf0bd6e8: head has no matching merged PR\nskip worktree-agent-a744e60c2e151655b: head has no matching merged PR\nskip worktree-agent-a7bc1966af3ea992f: head has no matching merged PR\nskip worktree-agent-a7c5be6112d67b9f6: head has no matching merged PR\nskip worktree-agent-a7e6803a56c313576: head has no matching merged PR\nskip worktree-agent-a7eed5dbd6d8b8a16: head has no matching merged PR\nskip worktree-agent-a889bc3a6632e4fde: head has no matching merged PR\nskip worktree-agent-a8aceafc30db1a83f: head has no matching merged PR\nskip worktree-agent-a8cff1924fd026894: head has no matching merged PR\nskip worktree-agent-a9776e12212ee8f1c: head has no matching merged PR\nskip worktree-agent-a9ca4fbdfd8aa33d4: head has no matching merged PR\nskip worktree-agent-a9feb76bee7001440: head has no matching merged PR\nskip worktree-agent-aa9e27467b99addaf: head has no matching merged PR\nskip worktree-agent-aab212e32bc0dc128: head has no matching merged PR\nskip worktree-agent-ab1a811516d621303: head has no matching merged PR\nskip worktree-agent-ab3ed27e0a9d550c1: head has no matching merged PR\nskip worktree-agent-ac0dec120b3f0435a: head has no matching merged PR\nskip worktree-agent-ad917f6deffc63602: head has no matching merged PR\nskip worktree-agent-af45bebf579e1ca83: head has no matching merged PR\nskip agent/ZII-807/board: remote head has no matching merged PR\nskip agent/ZII-808/lane2: remote head has no matching merged PR\nskip agent/ZII-810/impl: remote head has no matching merged PR\nskip agent/ZII-813/main: remote head has no matching merged PR\nskip agent/ZII-817/impl: remote head has no matching merged PR\nskip agent/ZII-818/end-reason: remote head has no matching merged PR\nskip agent/ZII-819/main: remote head has no matching merged PR\nskip agent/ZII-822/local-branch-cleanup: remote head has no matching merged PR\nskip agent/ZII-823/main: remote head has no matching merged PR\nskip agent/ZII-825/main: remote head has no matching merged PR\nskip agent/ZII-843/main: ticket ZII-843 is not confirmed completed (in flight or missing from Linear snapshot)\nskip agent/ZII-884/skeleton: ticket ZII-884 is not confirmed inactive (in flight or missing from Linear snapshot)\nprune_linear_calls=0\nlinear_calls=4\n",
     "stdout": "claimable= in_flight=1 max=10\nstale=\nhealth=live=1 max=10 released=0 escalated=0 failed_backlog=0\n",
     "timedOut": false
    }
   },
   {
    "_note": "claimable tickets exist",
    "id": "6639a79f-a90c-4044-8f37-2c196203d0f6",
    "status": "completed",
    "trigger": "scheduled",
    "createdAt": 1791203151733,
    "scheduledFor": 1791203100000,
    "startedAt": 1791203151772,
    "dispatchedAt": 1791203190229,
    "precheckResult": {
     "completedAt": 1791203188462,
     "durationMs": 36644,
     "exitCode": 0,
     "startedAt": 1791203151818,
     "stderr": "ip /home/user/orca/workspaces/orca-factory/ZII-843-main: ticket ZII-843 is not confirmed completed (in flight or missing from Linear snapshot)\nskip /home/user/github/orca-factory/.claude/worktrees/agent-a282baf4415e23c72: head has no matching merged PR\nskip /home/user/github/orca-factory/.claude/worktrees/agent-a2c2615f8e538f49b: head has no matching merged PR\nskip /home/user/github/orca-factory/.claude/worktrees/agent-a5c198c8f95b1db79: head has no matching merged PR\nskip /home/user/github/orca-factory/.claude/worktrees/agent-a8cff1924fd026894: head has no matching merged PR\nskip /home/user/github/orca-factory/.claude/worktrees/agent-ad917f6deffc63602: head has no matching merged PR\nskip agent/ZII-807/board: head has no matching merged PR\nskip orca-factory-setup-script: head has no matching merged PR\nskip worktree-agent-a03bf72b9b124bfa9: head has no matching merged PR\nskip worktree-agent-a08199cb8f7322fb4: head has no matching merged PR\nskip worktree-agent-a0fc2414f81e07fa4: head has no matching merged PR\nskip worktree-agent-a1be12a12c9586710: head has no matching merged PR\nskip worktree-agent-a282baf4415e23c72: head has no matching merged PR\nskip worktree-agent-a2c2615f8e538f49b: head has no matching merged PR\nskip worktree-agent-a37780aec88640c36: head has no matching merged PR\nskip worktree-agent-a48603a9d9b6d9ed9: head has no matching merged PR\nskip worktree-agent-a527c3b10d9c38de4: head has no matching merged PR\nskip worktree-agent-a543cced7ebc04444: head has no matching merged PR\nskip worktree-agent-a5c198c8f95b1db79: head has no matching merged PR\nskip worktree-agent-a62631f9bb53373c4: head has no matching merged PR\nskip worktree-agent-a66d5384a518080b1: head has no matching merged PR\nskip worktree-agent-a67dee6ef95e4226e: head has no matching merged PR\nskip worktree-agent-a6807bf14c9ab3582: head has no matching merged PR\nskip worktree-agent-a722b4612bf0bd6e8: head has no matching merged PR\nskip worktree-agent-a744e60c2e151655b: head has no matching merged PR\nskip worktree-agent-a7bc1966af3ea992f: head has no matching merged PR\nskip worktree-agent-a7c5be6112d67b9f6: head has no matching merged PR\nskip worktree-agent-a7e6803a56c313576: head has no matching merged PR\nskip worktree-agent-a7eed5dbd6d8b8a16: head has no matching merged PR\nskip worktree-agent-a889bc3a6632e4fde: head has no matching merged PR\nskip worktree-agent-a8aceafc30db1a83f: head has no matching merged PR\nskip worktree-agent-a8cff1924fd026894: head has no matching merged PR\nskip worktree-agent-a9776e12212ee8f1c: head has no matching merged PR\nskip worktree-agent-a9ca4fbdfd8aa33d4: head has no matching merged PR\nskip worktree-agent-a9feb76bee7001440: head has no matching merged PR\nskip worktree-agent-aa9e27467b99addaf: head has no matching merged PR\nskip worktree-agent-aab212e32bc0dc128: head has no matching merged PR\nskip worktree-agent-ab1a811516d621303: head has no matching merged PR\nskip worktree-agent-ab3ed27e0a9d550c1: head has no matching merged PR\nskip worktree-agent-ac0dec120b3f0435a: head has no matching merged PR\nskip worktree-agent-ad917f6deffc63602: head has no matching merged PR\nskip worktree-agent-af45bebf579e1ca83: head has no matching merged PR\nskip agent/ZII-807/board: remote head has no matching merged PR\nskip agent/ZII-808/lane2: remote head has no matching merged PR\nskip agent/ZII-810/impl: remote head has no matching merged PR\nskip agent/ZII-813/main: remote head has no matching merged PR\nskip agent/ZII-817/impl: remote head has no matching merged PR\nskip agent/ZII-818/end-reason: remote head has no matching merged PR\nskip agent/ZII-819/main: remote head has no matching merged PR\nskip agent/ZII-822/local-branch-cleanup: remote head has no matching merged PR\nskip agent/ZII-823/main: remote head has no matching merged PR\nskip agent/ZII-825/main: remote head has no matching merged PR\nskip agent/ZII-843/main: ticket ZII-843 is not confirmed completed (in flight or missing from Linear snapshot)\nprune_linear_calls=0\nlinear_calls=8\n",
     "stdout": "claimable=ZII-884,ZII-885 in_flight=0 max=10\nstale=\nhealth=live=0 max=10 released=0 escalated=0 failed_backlog=0\n",
     "timedOut": false
    }
   },
   {
    "_note": "rate limited",
    "id": "815d2886-656f-4a88-b3c5-18511fa3614f",
    "status": "skipped_precheck",
    "trigger": "scheduled",
    "createdAt": 1791185151554,
    "scheduledFor": 1791185100000,
    "startedAt": 1791185151579,
    "dispatchedAt": null,
    "precheckResult": {
     "completedAt": 1791185176726,
     "durationMs": 25121,
     "exitCode": 3,
     "startedAt": 1791185151605,
     "stderr": "apshot)\nskip /home/user/github/orca-factory/.claude/worktrees/agent-a282baf4415e23c72: head has no matching merged PR\nskip /home/user/github/orca-factory/.claude/worktrees/agent-a2c2615f8e538f49b: head has no matching merged PR\nskip /home/user/github/orca-factory/.claude/worktrees/agent-a5c198c8f95b1db79: head has no matching merged PR\nskip /home/user/github/orca-factory/.claude/worktrees/agent-a8cff1924fd026894: head has no matching merged PR\nskip /home/user/github/orca-factory/.claude/worktrees/agent-ad917f6deffc63602: head has no matching merged PR\nskip agent/ZII-807/board: head has no matching merged PR\nskip orca-factory-setup-script: head has no matching merged PR\nskip worktree-agent-a03bf72b9b124bfa9: head has no matching merged PR\nskip worktree-agent-a08199cb8f7322fb4: head has no matching merged PR\nskip worktree-agent-a0fc2414f81e07fa4: head has no matching merged PR\nskip worktree-agent-a1be12a12c9586710: head has no matching merged PR\nskip worktree-agent-a282baf4415e23c72: head has no matching merged PR\nskip worktree-agent-a2c2615f8e538f49b: head has no matching merged PR\nskip worktree-agent-a37780aec88640c36: head has no matching merged PR\nskip worktree-agent-a48603a9d9b6d9ed9: head has no matching merged PR\nskip worktree-agent-a527c3b10d9c38de4: head has no matching merged PR\nskip worktree-agent-a543cced7ebc04444: head has no matching merged PR\nskip worktree-agent-a5c198c8f95b1db79: head has no matching merged PR\nskip worktree-agent-a62631f9bb53373c4: head has no matching merged PR\nskip worktree-agent-a66d5384a518080b1: head has no matching merged PR\nskip worktree-agent-a67dee6ef95e4226e: head has no matching merged PR\nskip worktree-agent-a6807bf14c9ab3582: head has no matching merged PR\nskip worktree-agent-a722b4612bf0bd6e8: head has no matching merged PR\nskip worktree-agent-a744e60c2e151655b: head has no matching merged PR\nskip worktree-agent-a7bc1966af3ea992f: head has no matching merged PR\nskip worktree-agent-a7c5be6112d67b9f6: head has no matching merged PR\nskip worktree-agent-a7e6803a56c313576: head has no matching merged PR\nskip worktree-agent-a7eed5dbd6d8b8a16: head has no matching merged PR\nskip worktree-agent-a889bc3a6632e4fde: head has no matching merged PR\nskip worktree-agent-a8aceafc30db1a83f: head has no matching merged PR\nskip worktree-agent-a8cff1924fd026894: head has no matching merged PR\nskip worktree-agent-a9776e12212ee8f1c: head has no matching merged PR\nskip worktree-agent-a9ca4fbdfd8aa33d4: head has no matching merged PR\nskip worktree-agent-a9feb76bee7001440: head has no matching merged PR\nskip worktree-agent-aa9e27467b99addaf: head has no matching merged PR\nskip worktree-agent-aab212e32bc0dc128: head has no matching merged PR\nskip worktree-agent-ab1a811516d621303: head has no matching merged PR\nskip worktree-agent-ab3ed27e0a9d550c1: head has no matching merged PR\nskip worktree-agent-ac0dec120b3f0435a: head has no matching merged PR\nskip worktree-agent-ad917f6deffc63602: head has no matching merged PR\nskip worktree-agent-af45bebf579e1ca83: head has no matching merged PR\nskip agent/ZII-807/board: remote head has no matching merged PR\nskip agent/ZII-808/lane2: remote head has no matching merged PR\nskip agent/ZII-810/impl: remote head has no matching merged PR\nskip agent/ZII-813/main: remote head has no matching merged PR\nskip agent/ZII-817/impl: remote head has no matching merged PR\nskip agent/ZII-818/end-reason: remote head has no matching merged PR\nskip agent/ZII-819/main: remote head has no matching merged PR\nskip agent/ZII-822/local-branch-cleanup: remote head has no matching merged PR\nskip agent/ZII-823/main: remote head has no matching merged PR\nskip agent/ZII-825/main: remote head has no matching merged PR\nskip agent/ZII-843/main: ticket ZII-843 is not confirmed completed (in flight or missing from Linear snapshot)\nskip agent/ZII-877/scope: ticket ZII-877 is not confirmed inactive (in flight or missing from Linear snapshot)\nprune_linear_calls=0\n不确定: linear rate limited\nlinear_calls=7\n",
     "stdout": "",
     "timedOut": false
    }
   },
   {
    "_note": "scope conflict: candidate overlaps an in-flight ticket",
    "id": "zii888-scope-conflict",
    "status": "skipped_precheck",
    "trigger": "scheduled",
    "createdAt": 1791203751800,
    "startedAt": 1791203751810,
    "precheckResult": {
     "stdout": "claimable= in_flight=1 max=10\n",
     "stderr": "skip ZII-900: scope overlaps mods/swarm-board/hooks/views/throughput.tsx\n",
     "exitCode": 1,
     "timedOut": false
    }
   },
   {
    "_note": "at cap: all ten in-flight slots are occupied",
    "id": "zii888-at-cap",
    "status": "skipped_precheck",
    "trigger": "scheduled",
    "createdAt": 1791203751820,
    "startedAt": 1791203751830,
    "precheckResult": {
     "stdout": "claimable= in_flight=10 max=10\n",
     "stderr": "",
     "exitCode": 1,
     "timedOut": false
    }
   }
  ]
 }
}
  const now = 1791203751734 + 60_000
  const data = buildThroughput({ now, ledger: [], runs: fixture.result.runs, maxInFlight: 10 })
  expect(data.noTicket).toEqual({ noClaimable: 2, scopeConflict: 1, atCap: 1, rateLimited: 1 })
  expect(data.inFlightAvg).toBe(12 / 5)
  expect(data.maxInFlight).toBe(10)

  const recent = new Date(now - 1000).toISOString()
  const runs: Run[] = [
    { createdAt: recent, precheckResult: { stdout: 'claimable= in_flight=10 max=10' } },
    { startedAt: recent, precheckResult: {
      stdout: 'claimable= in_flight=10 max=10', stderr: 'skip ZII-888: scope overlaps path',
    } },
    { createdAt: now - 500, precheckResult: {
      stdout: 'claimable= in_flight=10 max=10',
      stderr: 'skip ZII-888: scope overlaps\n不确定: linear rate limited',
    } },
    { createdAt: recent, precheckResult: { timedOut: true, stderr: 'rate limited' } },
    { createdAt: now - 500, precheckResult: {
      stdout: 'claimable= in_flight=0 max=0',
      stderr: 'skip /path/scope: cleanup\ncleanup mentions rate limit',
    } },
    { createdAt: now - 500, precheckResult: {
      stdout: 'claimable=ZII-889 in_flight=2 max=10',
      stderr: 'skip ZII-888: scope overlaps\n不确定: linear rate limited',
    } },
    { createdAt: now - 86_400_000, precheckResult: { stdout: 'claimable= in_flight=100 max=10' } },
    { createdAt: now + 1, precheckResult: { stdout: 'claimable= in_flight=100 max=10' } },
    { createdAt: 'invalid', precheckResult: { stdout: 'claimable= in_flight=100 max=10' } },
    { createdAt: now },
    { createdAt: now - 86_400_000, startedAt: recent, precheckResult: {
      stdout: 'diagnostic output\nclaimable= in_flight=1 max=10',
      stderr: 'skip OTHER-900: scope overlaps mods/swarm-board/hooks/views/throughput.tsx',
    } },
    { createdAt: now, startedAt: now + 1, precheckResult: { stdout: 'claimable= in_flight=100 max=10' } },
    { createdAt: now, precheckResult: { stdout: 'diagnostic output\nclaimable= in_flight=2 max=12' } },
    { createdAt: recent, precheckResult: { stdout: 'in_flight=100 max=99' } },
    { createdAt: now, startedAt: 'invalid', precheckResult: { stdout: 'claimable= in_flight=100 max=10' } },
  ]
  const classified = buildThroughput({ now, ledger: [], runs, maxInFlight: 7 })
  expect(classified.noTicket).toEqual({ noClaimable: 1, scopeConflict: 1, atCap: 3, rateLimited: 2 })
  expect(classified.inFlightAvg).toBe(5)
  expect(classified.maxInFlight).toBe(7)
  expect(buildThroughput({ now, ledger: [], runs }).maxInFlight).toBe(12)
  expect(buildThroughput({ now, ledger: [], runs: [...runs].reverse() }).maxInFlight).toBe(12)
  expect(buildThroughput({ now, ledger: [], runs: [], maxInFlight: 10 }).maxInFlight).toBe(10)

  const Text = (props: { children?: string | unknown[] }) =>
  Array.isArray(props.children) ? props.children.join('') : props.children
  const empty = renderThroughput(Text)
  expect(empty).toBe('数据未就绪')
  const lines = renderThroughput(Text, classified)
  expect(lines).toContain('SCOPE 冲突：1')
  expect(lines).toContain('在途均值 5.0 / 上限 7')
})
