import { test, expect } from 'claude-code/testing'
import { buildThroughput } from './throughput'

// Real `orca automations runs --json` rows (2026-10-06; local paths redacted, only the
// fields the view reads are kept). The mod sandbox has no fs, so the sample is inline;
// the full recording lives in driver/testdata/recorded/orca-automations-runs.json.
// Times are epoch milliseconds and there is no finishedAt. The rate-limited row is
// precheck's own: stderr line starts with 不确定: and exitCode is 3.
const real = [
  {
    "startedAt": 1791202851754,
    "createdAt": 1791202851729,
    "precheckResult": {
      "stdout": "claimable= in_flight=0 max=10\nstale=\nhealth=live=0 max=10 released=0 escalated=0 failed_backlog=0\n",
      "stderr": "ip /home/user/orca/workspaces/orca-factory/ZII-843-main: ticket ZII-843 is not confirmed completed (in flight or missing from Linear snapshot)\nskip /home/user/github/orca-factory/.claude/worktrees/agent-a282baf4415e23c72: head has no matching merged PR\nskip /home/user/github/orca-factory/.claude/worktrees/agent-a2c2615f8e538f49b: head has no matching merged PR\nskip /home/user/github/orca-factory/.claude/worktrees/agent-a5c198c8f95b1db79: head has no matching merged PR\nskip /home/user/github/orca-factory/.claude/worktrees/agent-a8cff1924fd026894: head has no matching merged PR\nskip /home/user/github/orca-factory/.claude/worktrees/agent-ad917f6deffc63602: head has no matching merged PR\nskip agent/ZII-807/board: head has no matching merged PR\nskip orca-factory-setup-script: head has no matching merged PR\nskip worktree-agent-a03bf72b9b124bfa9: head has no matching merged PR\nskip worktree-agent-a08199cb8f7322fb4: head has no matching merged PR\nskip worktree-agent-a0fc2414f81e07fa4: head has no matching merged PR\nskip worktree-agent-a1be12a12c9586710: head has no matching merged PR\nskip worktree-agent-a282baf4415e23c72: head has no matching merged PR\nskip worktree-agent-a2c2615f8e538f49b: head has no matching merged PR\nskip worktree-agent-a37780aec88640c36: head has no matching merged PR\nskip worktree-agent-a48603a9d9b6d9ed9: head has no matching merged PR\nskip worktree-agent-a527c3b10d9c38de4: head has no matching merged PR\nskip worktree-agent-a543cced7ebc04444: head has no matching merged PR\nskip worktree-agent-a5c198c8f95b1db79: head has no matching merged PR\nskip worktree-agent-a62631f9bb53373c4: head has no matching merged PR\nskip worktree-agent-a66d5384a518080b1: head has no matching merged PR\nskip worktree-agent-a67dee6ef95e4226e: head has no matching merged PR\nskip worktree-agent-a6807bf14c9ab3582: head has no matching merged PR\nskip worktree-agent-a722b4612bf0bd6e8: head has no matching merged PR\nskip worktree-agent-a744e60c2e151655b: head has no matching merged PR\nskip worktree-agent-a7bc1966af3ea992f: head has no matching merged PR\nskip worktree-agent-a7c5be6112d67b9f6: head has no matching merged PR\nskip worktree-agent-a7e6803a56c313576: head has no matching merged PR\nskip worktree-agent-a7eed5dbd6d8b8a16: head has no matching merged PR\nskip worktree-agent-a889bc3a6632e4fde: head has no matching merged PR\nskip worktree-agent-a8aceafc30db1a83f: head has no matching merged PR\nskip worktree-agent-a8cff1924fd026894: head has no matching merged PR\nskip worktree-agent-a9776e12212ee8f1c: head has no matching merged PR\nskip worktree-agent-a9ca4fbdfd8aa33d4: head has no matching merged PR\nskip worktree-agent-a9feb76bee7001440: head has no matching merged PR\nskip worktree-agent-aa9e27467b99addaf: head has no matching merged PR\nskip worktree-agent-aab212e32bc0dc128: head has no matching merged PR\nskip worktree-agent-ab1a811516d621303: head has no matching merged PR\nskip worktree-agent-ab3ed27e0a9d550c1: head has no matching merged PR\nskip worktree-agent-ac0dec120b3f0435a: head has no matching merged PR\nskip worktree-agent-ad917f6deffc63602: head has no matching merged PR\nskip worktree-agent-af45bebf579e1ca83: head has no matching merged PR\nskip agent/ZII-807/board: remote head has no matching merged PR\nskip agent/ZII-808/lane2: remote head has no matching merged PR\nskip agent/ZII-810/impl: remote head has no matching merged PR\nskip agent/ZII-813/main: remote head has no matching merged PR\nskip agent/ZII-817/impl: remote head has no matching merged PR\nskip agent/ZII-818/end-reason: remote head has no matching merged PR\nskip agent/ZII-819/main: remote head has no matching merged PR\nskip agent/ZII-822/local-branch-cleanup: remote head has no matching merged PR\nskip agent/ZII-823/main: remote head has no matching merged PR\nskip agent/ZII-825/main: remote head has no matching merged PR\nskip agent/ZII-843/main: ticket ZII-843 is not confirmed completed (in flight or missing from Linear snapshot)\nprune_linear_calls=0\nlinear_calls=2\n",
      "exitCode": 1
    }
  },
  {
    "startedAt": 1791203751790,
    "createdAt": 1791203751734,
    "precheckResult": {
      "stdout": "claimable= in_flight=1 max=10\nstale=\nhealth=live=1 max=10 released=0 escalated=0 failed_backlog=0\n",
      "stderr": "missing from Linear snapshot)\nskip /home/user/github/orca-factory/.claude/worktrees/agent-a282baf4415e23c72: head has no matching merged PR\nskip /home/user/github/orca-factory/.claude/worktrees/agent-a2c2615f8e538f49b: head has no matching merged PR\nskip /home/user/github/orca-factory/.claude/worktrees/agent-a5c198c8f95b1db79: head has no matching merged PR\nskip /home/user/github/orca-factory/.claude/worktrees/agent-a8cff1924fd026894: head has no matching merged PR\nskip /home/user/github/orca-factory/.claude/worktrees/agent-ad917f6deffc63602: head has no matching merged PR\nskip agent/ZII-807/board: head has no matching merged PR\nskip orca-factory-setup-script: head has no matching merged PR\nskip worktree-agent-a03bf72b9b124bfa9: head has no matching merged PR\nskip worktree-agent-a08199cb8f7322fb4: head has no matching merged PR\nskip worktree-agent-a0fc2414f81e07fa4: head has no matching merged PR\nskip worktree-agent-a1be12a12c9586710: head has no matching merged PR\nskip worktree-agent-a282baf4415e23c72: head has no matching merged PR\nskip worktree-agent-a2c2615f8e538f49b: head has no matching merged PR\nskip worktree-agent-a37780aec88640c36: head has no matching merged PR\nskip worktree-agent-a48603a9d9b6d9ed9: head has no matching merged PR\nskip worktree-agent-a527c3b10d9c38de4: head has no matching merged PR\nskip worktree-agent-a543cced7ebc04444: head has no matching merged PR\nskip worktree-agent-a5c198c8f95b1db79: head has no matching merged PR\nskip worktree-agent-a62631f9bb53373c4: head has no matching merged PR\nskip worktree-agent-a66d5384a518080b1: head has no matching merged PR\nskip worktree-agent-a67dee6ef95e4226e: head has no matching merged PR\nskip worktree-agent-a6807bf14c9ab3582: head has no matching merged PR\nskip worktree-agent-a722b4612bf0bd6e8: head has no matching merged PR\nskip worktree-agent-a744e60c2e151655b: head has no matching merged PR\nskip worktree-agent-a7bc1966af3ea992f: head has no matching merged PR\nskip worktree-agent-a7c5be6112d67b9f6: head has no matching merged PR\nskip worktree-agent-a7e6803a56c313576: head has no matching merged PR\nskip worktree-agent-a7eed5dbd6d8b8a16: head has no matching merged PR\nskip worktree-agent-a889bc3a6632e4fde: head has no matching merged PR\nskip worktree-agent-a8aceafc30db1a83f: head has no matching merged PR\nskip worktree-agent-a8cff1924fd026894: head has no matching merged PR\nskip worktree-agent-a9776e12212ee8f1c: head has no matching merged PR\nskip worktree-agent-a9ca4fbdfd8aa33d4: head has no matching merged PR\nskip worktree-agent-a9feb76bee7001440: head has no matching merged PR\nskip worktree-agent-aa9e27467b99addaf: head has no matching merged PR\nskip worktree-agent-aab212e32bc0dc128: head has no matching merged PR\nskip worktree-agent-ab1a811516d621303: head has no matching merged PR\nskip worktree-agent-ab3ed27e0a9d550c1: head has no matching merged PR\nskip worktree-agent-ac0dec120b3f0435a: head has no matching merged PR\nskip worktree-agent-ad917f6deffc63602: head has no matching merged PR\nskip worktree-agent-af45bebf579e1ca83: head has no matching merged PR\nskip agent/ZII-807/board: remote head has no matching merged PR\nskip agent/ZII-808/lane2: remote head has no matching merged PR\nskip agent/ZII-810/impl: remote head has no matching merged PR\nskip agent/ZII-813/main: remote head has no matching merged PR\nskip agent/ZII-817/impl: remote head has no matching merged PR\nskip agent/ZII-818/end-reason: remote head has no matching merged PR\nskip agent/ZII-819/main: remote head has no matching merged PR\nskip agent/ZII-822/local-branch-cleanup: remote head has no matching merged PR\nskip agent/ZII-823/main: remote head has no matching merged PR\nskip agent/ZII-825/main: remote head has no matching merged PR\nskip agent/ZII-843/main: ticket ZII-843 is not confirmed completed (in flight or missing from Linear snapshot)\nskip agent/ZII-884/skeleton: ticket ZII-884 is not confirmed inactive (in flight or missing from Linear snapshot)\nprune_linear_calls=0\nlinear_calls=4\n",
      "exitCode": 1
    }
  },
  {
    "startedAt": 1791203151772,
    "createdAt": 1791203151733,
    "precheckResult": {
      "stdout": "claimable=ZII-884,ZII-885 in_flight=0 max=10\nstale=\nhealth=live=0 max=10 released=0 escalated=0 failed_backlog=0\n",
      "stderr": "ip /home/user/orca/workspaces/orca-factory/ZII-843-main: ticket ZII-843 is not confirmed completed (in flight or missing from Linear snapshot)\nskip /home/user/github/orca-factory/.claude/worktrees/agent-a282baf4415e23c72: head has no matching merged PR\nskip /home/user/github/orca-factory/.claude/worktrees/agent-a2c2615f8e538f49b: head has no matching merged PR\nskip /home/user/github/orca-factory/.claude/worktrees/agent-a5c198c8f95b1db79: head has no matching merged PR\nskip /home/user/github/orca-factory/.claude/worktrees/agent-a8cff1924fd026894: head has no matching merged PR\nskip /home/user/github/orca-factory/.claude/worktrees/agent-ad917f6deffc63602: head has no matching merged PR\nskip agent/ZII-807/board: head has no matching merged PR\nskip orca-factory-setup-script: head has no matching merged PR\nskip worktree-agent-a03bf72b9b124bfa9: head has no matching merged PR\nskip worktree-agent-a08199cb8f7322fb4: head has no matching merged PR\nskip worktree-agent-a0fc2414f81e07fa4: head has no matching merged PR\nskip worktree-agent-a1be12a12c9586710: head has no matching merged PR\nskip worktree-agent-a282baf4415e23c72: head has no matching merged PR\nskip worktree-agent-a2c2615f8e538f49b: head has no matching merged PR\nskip worktree-agent-a37780aec88640c36: head has no matching merged PR\nskip worktree-agent-a48603a9d9b6d9ed9: head has no matching merged PR\nskip worktree-agent-a527c3b10d9c38de4: head has no matching merged PR\nskip worktree-agent-a543cced7ebc04444: head has no matching merged PR\nskip worktree-agent-a5c198c8f95b1db79: head has no matching merged PR\nskip worktree-agent-a62631f9bb53373c4: head has no matching merged PR\nskip worktree-agent-a66d5384a518080b1: head has no matching merged PR\nskip worktree-agent-a67dee6ef95e4226e: head has no matching merged PR\nskip worktree-agent-a6807bf14c9ab3582: head has no matching merged PR\nskip worktree-agent-a722b4612bf0bd6e8: head has no matching merged PR\nskip worktree-agent-a744e60c2e151655b: head has no matching merged PR\nskip worktree-agent-a7bc1966af3ea992f: head has no matching merged PR\nskip worktree-agent-a7c5be6112d67b9f6: head has no matching merged PR\nskip worktree-agent-a7e6803a56c313576: head has no matching merged PR\nskip worktree-agent-a7eed5dbd6d8b8a16: head has no matching merged PR\nskip worktree-agent-a889bc3a6632e4fde: head has no matching merged PR\nskip worktree-agent-a8aceafc30db1a83f: head has no matching merged PR\nskip worktree-agent-a8cff1924fd026894: head has no matching merged PR\nskip worktree-agent-a9776e12212ee8f1c: head has no matching merged PR\nskip worktree-agent-a9ca4fbdfd8aa33d4: head has no matching merged PR\nskip worktree-agent-a9feb76bee7001440: head has no matching merged PR\nskip worktree-agent-aa9e27467b99addaf: head has no matching merged PR\nskip worktree-agent-aab212e32bc0dc128: head has no matching merged PR\nskip worktree-agent-ab1a811516d621303: head has no matching merged PR\nskip worktree-agent-ab3ed27e0a9d550c1: head has no matching merged PR\nskip worktree-agent-ac0dec120b3f0435a: head has no matching merged PR\nskip worktree-agent-ad917f6deffc63602: head has no matching merged PR\nskip worktree-agent-af45bebf579e1ca83: head has no matching merged PR\nskip agent/ZII-807/board: remote head has no matching merged PR\nskip agent/ZII-808/lane2: remote head has no matching merged PR\nskip agent/ZII-810/impl: remote head has no matching merged PR\nskip agent/ZII-813/main: remote head has no matching merged PR\nskip agent/ZII-817/impl: remote head has no matching merged PR\nskip agent/ZII-818/end-reason: remote head has no matching merged PR\nskip agent/ZII-819/main: remote head has no matching merged PR\nskip agent/ZII-822/local-branch-cleanup: remote head has no matching merged PR\nskip agent/ZII-823/main: remote head has no matching merged PR\nskip agent/ZII-825/main: remote head has no matching merged PR\nskip agent/ZII-843/main: ticket ZII-843 is not confirmed completed (in flight or missing from Linear snapshot)\nprune_linear_calls=0\nlinear_calls=8\n",
      "exitCode": 0
    }
  },
  {
    "startedAt": 1791185151579,
    "createdAt": 1791185151554,
    "precheckResult": {
      "stdout": "",
      "stderr": "apshot)\nskip /home/user/github/orca-factory/.claude/worktrees/agent-a282baf4415e23c72: head has no matching merged PR\nskip /home/user/github/orca-factory/.claude/worktrees/agent-a2c2615f8e538f49b: head has no matching merged PR\nskip /home/user/github/orca-factory/.claude/worktrees/agent-a5c198c8f95b1db79: head has no matching merged PR\nskip /home/user/github/orca-factory/.claude/worktrees/agent-a8cff1924fd026894: head has no matching merged PR\nskip /home/user/github/orca-factory/.claude/worktrees/agent-ad917f6deffc63602: head has no matching merged PR\nskip agent/ZII-807/board: head has no matching merged PR\nskip orca-factory-setup-script: head has no matching merged PR\nskip worktree-agent-a03bf72b9b124bfa9: head has no matching merged PR\nskip worktree-agent-a08199cb8f7322fb4: head has no matching merged PR\nskip worktree-agent-a0fc2414f81e07fa4: head has no matching merged PR\nskip worktree-agent-a1be12a12c9586710: head has no matching merged PR\nskip worktree-agent-a282baf4415e23c72: head has no matching merged PR\nskip worktree-agent-a2c2615f8e538f49b: head has no matching merged PR\nskip worktree-agent-a37780aec88640c36: head has no matching merged PR\nskip worktree-agent-a48603a9d9b6d9ed9: head has no matching merged PR\nskip worktree-agent-a527c3b10d9c38de4: head has no matching merged PR\nskip worktree-agent-a543cced7ebc04444: head has no matching merged PR\nskip worktree-agent-a5c198c8f95b1db79: head has no matching merged PR\nskip worktree-agent-a62631f9bb53373c4: head has no matching merged PR\nskip worktree-agent-a66d5384a518080b1: head has no matching merged PR\nskip worktree-agent-a67dee6ef95e4226e: head has no matching merged PR\nskip worktree-agent-a6807bf14c9ab3582: head has no matching merged PR\nskip worktree-agent-a722b4612bf0bd6e8: head has no matching merged PR\nskip worktree-agent-a744e60c2e151655b: head has no matching merged PR\nskip worktree-agent-a7bc1966af3ea992f: head has no matching merged PR\nskip worktree-agent-a7c5be6112d67b9f6: head has no matching merged PR\nskip worktree-agent-a7e6803a56c313576: head has no matching merged PR\nskip worktree-agent-a7eed5dbd6d8b8a16: head has no matching merged PR\nskip worktree-agent-a889bc3a6632e4fde: head has no matching merged PR\nskip worktree-agent-a8aceafc30db1a83f: head has no matching merged PR\nskip worktree-agent-a8cff1924fd026894: head has no matching merged PR\nskip worktree-agent-a9776e12212ee8f1c: head has no matching merged PR\nskip worktree-agent-a9ca4fbdfd8aa33d4: head has no matching merged PR\nskip worktree-agent-a9feb76bee7001440: head has no matching merged PR\nskip worktree-agent-aa9e27467b99addaf: head has no matching merged PR\nskip worktree-agent-aab212e32bc0dc128: head has no matching merged PR\nskip worktree-agent-ab1a811516d621303: head has no matching merged PR\nskip worktree-agent-ab3ed27e0a9d550c1: head has no matching merged PR\nskip worktree-agent-ac0dec120b3f0435a: head has no matching merged PR\nskip worktree-agent-ad917f6deffc63602: head has no matching merged PR\nskip worktree-agent-af45bebf579e1ca83: head has no matching merged PR\nskip agent/ZII-807/board: remote head has no matching merged PR\nskip agent/ZII-808/lane2: remote head has no matching merged PR\nskip agent/ZII-810/impl: remote head has no matching merged PR\nskip agent/ZII-813/main: remote head has no matching merged PR\nskip agent/ZII-817/impl: remote head has no matching merged PR\nskip agent/ZII-818/end-reason: remote head has no matching merged PR\nskip agent/ZII-819/main: remote head has no matching merged PR\nskip agent/ZII-822/local-branch-cleanup: remote head has no matching merged PR\nskip agent/ZII-823/main: remote head has no matching merged PR\nskip agent/ZII-825/main: remote head has no matching merged PR\nskip agent/ZII-843/main: ticket ZII-843 is not confirmed completed (in flight or missing from Linear snapshot)\nskip agent/ZII-877/scope: ticket ZII-877 is not confirmed inactive (in flight or missing from Linear snapshot)\nprune_linear_calls=0\n不确定: linear rate limited\nlinear_calls=7\n",
      "exitCode": 3
    }
  }
]

// SYNTHETIC rows, each a real stdout/stderr shape with one field changed:
// - capped: no real run has ever hit the in-flight cap (in_flight raised to max).
// - prune: prune-run-worktrees' own rate-limit line shares precheck's stderr, yet precheck
//   still claimed a ticket (never seen in the recorded runs; the code path exists at
//   scripts/agent-gates/prune-run-worktrees.sh). Replace both with recordings when seen.
const capped = {
  "startedAt": 1791203752790,
  "createdAt": 1791203752790,
  "precheckResult": {
    "stdout": "claimable= in_flight=10 max=10\nstale=\nhealth=live=10 max=10 released=0 escalated=0 failed_backlog=0\n",
    "stderr": "",
    "exitCode": 0
  }
}
const prune = {
  "startedAt": 1791203753790,
  "createdAt": 1791203753790,
  "precheckResult": {
    "stdout": "claimable=ZII-900 in_flight=3 max=10\nstale=\nhealth=live=3 max=10 released=0 escalated=0 failed_backlog=0\n",
    "stderr": "prune: 不确定: linear rate limited\n",
    "exitCode": 0
  }
}

// prune-only rate limit with NOTHING claimed: the anchored pattern must not treat prune's
// "prune: 不确定: linear rate limited" as precheck's own, so this is a plain no-ticket round.
const pruneIdle = {
  "startedAt": 1791203754790,
  "createdAt": 1791203754790,
  "precheckResult": {
    "stdout": "claimable= in_flight=0 max=10\nstale=\nhealth=live=0 max=10 released=0 escalated=0 failed_backlog=0\n",
    "stderr": "prune: 不确定: linear rate limited\n",
    "exitCode": 0
  }
}

test('throughput view: real automations runs classify into the no-ticket reasons', async () => {
  const latest = Math.max(...real.map(r => r.startedAt))
  const r = buildThroughput({ now: latest + 10_000, ledger: [], runs: [...real, capped, prune, pruneIdle] })
  const reasons = Object.fromEntries(Object.entries(r.reasons))
  // rows that did claim tickets are not counted.
  expect(reasons['无可领票']).toBe(3) // idle + busy-below-cap + prune-only rate limit
  expect(reasons['限流']).toBe(1) // the prune-only rate limit (claimed a ticket) must not count
  expect(reasons['达到上限']).toBe(1)
  // AGENT_SCOPE_AVOID=0 in this repo, so the row stays but reads zero.
  expect(reasons['SCOPE 冲突']).toBe(0)
})

// stage_secs rows below use the real key shape (queue/worker/gate/review/merge, see
// driver/internal/ledger/ledger_test.go). The values are constructed: ZII-885 is the first
// ticket that writes stage_secs, so no recorded ledger row carries it yet.
const NOW = Date.parse('2026-10-06T12:00:00Z')
const row = (secs: Record<string, unknown> | undefined) => ({ outcome: 'merged', finished_at: '2026-10-06T11:30:00Z', stage_secs: secs })

test('throughput view: stage durations use median and p90 over rows that have them', async () => {
  const ledger = [
    row({ queue: 10, worker: 100, gate: 20, review: 30, merge: 40 }),
    row({ queue: 20, worker: 300, gate: 40, review: 60, merge: 80 }),
    row({ queue: 30, worker: 200, gate: 60, review: 90, merge: 120 }),
    row({ queue: 40, worker: 400, gate: 80, review: 120, merge: 160 }),
    { outcome: 'merged', finished_at: '2026-10-06T11:00:00Z' }, // old row without stage_secs: skipped
    row({ queue: 'x', worker: Number.NaN, gate: 1000 }), // non-numeric values: skipped per stage
  ]
  const r = buildThroughput({ now: NOW, ledger, runs: [] })
  const byKey = Object.fromEntries(r.stages.map(s => [s.key, s]))
  expect(r.stages.map(s => s.key)).toEqual(['queue', 'worker', 'gate', 'review', 'merge'])
  expect(byKey.queue).toMatchObject({ label: '等领取', median: 25, p90: 40 }) // 10,20,30,40
  expect(byKey.worker).toMatchObject({ median: 250, p90: 400 }) // 100,200,300,400
  // gate has 5 numeric samples (20,40,60,80,1000): the median ignores the outlier, p90 reports it.
  expect(byKey.gate).toMatchObject({ median: 60, p90: 1000 })
  expect(byKey.merge).toMatchObject({ label: '开 PR 到合并', median: 100, p90: 160 })
})

test('throughput view: no ledger rows with stage_secs means no stage entries', async () => {
  const r = buildThroughput({ now: NOW, ledger: [{ outcome: 'merged', finished_at: '2026-10-06T11:00:00Z' }, row(undefined)], runs: [] })
  expect(r.stages).toEqual([])
})

// In-flight average and cap come from precheck's stdout "in_flight=N max=M" on every run
// inside the 24h window; the cap is the one from the newest run unless passed explicitly.
const at = (ms: number, inFlight: number, max: number) => ({
  startedAt: ms,
  precheckResult: { stdout: `claimable= in_flight=${inFlight} max=${max}\nstale=\n`, stderr: '' },
})

test('throughput view: in-flight average and cap come from runs inside the 24h window', async () => {
  const runs = [
    at(NOW - 48 * 3_600_000, 9, 4), // outside the window: ignored by both the average and the cap
    at(NOW - 3 * 3_600_000, 2, 10),
    at(NOW - 2 * 3_600_000, 4, 10),
    at(NOW - 1 * 3_600_000, 6, 20), // newest run: cap is 20
  ]
  const r = buildThroughput({ now: NOW, ledger: [], runs })
  expect(r.inFlightAvg).toBe(4) // (2 + 4 + 6) / 3
  expect(r.maxInFlight).toBe(20)
})

test('throughput view: an explicit maxInFlight wins over the one parsed from runs', async () => {
  const r = buildThroughput({ now: NOW, ledger: [], runs: [at(NOW - 3_600_000, 3, 10)], maxInFlight: 7 })
  expect(r.maxInFlight).toBe(7)
  expect(r.inFlightAvg).toBe(3)
})

test('throughput view: without any run the in-flight average is 0, not NaN', async () => {
  const r = buildThroughput({ now: NOW, ledger: [], runs: [] })
  expect(r.inFlightAvg).toBe(0)
  expect(r.maxInFlight).toBe(0)
})
