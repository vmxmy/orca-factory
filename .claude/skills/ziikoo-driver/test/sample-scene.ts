// 2026-11-05 在 .claude/skills/ziikoo-driver 录制，bun --version 输出 1.3.1，网卡 enp3s0 的 IPv4 为 10.10.10.121。
// 录制原文的权威副本待放入 driver/testdata/recorded/（见 CONTRACTS.md），本文件内嵌同一原文，由 driver 侧 Go 测试对照。
// token 是录制专用的假值，不是凭据：T=$(printf 'deadbeef%.0s' {1..8})。
// stdout、stderr、退出码写入当前 worktree 的 .omc/tmp/scene-{listen,no-interface,port-in-use,bad-port}.{out,err,code}。
// 监听成功，退出码 124 来自 timeout，stdout 含行尾：
// 目标录制文件：driver/testdata/recorded/ziikoo-scene-listening.out
// ZIIKOO_SCENE_TOKEN=$T ZIIKOO_SCENE_BIND=enp3s0 ZIIKOO_SCENE_PORT=4300 timeout -s INT 3 bun run scene/server.ts
// 接口不存在，退出码 1：
// 目标录制文件：driver/testdata/recorded/ziikoo-scene-no-interface.err
// ZIIKOO_SCENE_TOKEN=$T ZIIKOO_SCENE_BIND=nosuch0 ZIIKOO_SCENE_PORT=4300 timeout -s INT 3 bun run scene/server.ts
// 端口占用，退出码 1，先启动占用进程，录完只按 PID 清理：
// 目标录制文件：driver/testdata/recorded/ziikoo-scene-port-in-use.err
// ZIIKOO_SCENE_TOKEN=$T ZIIKOO_SCENE_BIND=enp3s0 ZIIKOO_SCENE_PORT=4300 bun run scene/server.ts >/dev/null 2>&1 & P=$!
// ZIIKOO_SCENE_TOKEN=$T ZIIKOO_SCENE_BIND=enp3s0 ZIIKOO_SCENE_PORT=4300 timeout -s INT 3 bun run scene/server.ts
// kill "$P"
// 非法端口，退出码 1：
// 目标录制文件：driver/testdata/recorded/ziikoo-scene-bad-port.err
// ZIIKOO_SCENE_TOKEN=$T ZIIKOO_SCENE_BIND=enp3s0 ZIIKOO_SCENE_PORT=abc timeout -s INT 3 bun run scene/server.ts
// 缺少 token，退出码 1，stderr 与退出码写入当前 worktree 的 .omc/tmp/scene-no-token.{err,code}：
// 目标录制文件：driver/testdata/recorded/ziikoo-scene-no-token.err
// env -u ZIIKOO_SCENE_TOKEN bun run scene/server.ts
export const SAMPLE_SCENE_LISTENING = String.raw`listening http://10.10.10.121:4300/?t=deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef
`

export const SAMPLE_SCENE_NO_INTERFACE = {
  stderr: `接口 nosuch0 不存在
`,
  exitCode: 1,
}

export const SAMPLE_SCENE_PORT_IN_USE = {
  stderr: `端口 4300 被占用
`,
  exitCode: 1,
}

export const SAMPLE_SCENE_BAD_PORT = {
  stderr: `3D 服务端口必须是 0–65535 的整数，收到 abc
`,
  exitCode: 1,
}

export const SAMPLE_SCENE_NO_TOKEN = {
  stderr: ` 5 | import { parseConfig } from '../hooks/config'
 6 | import { parseRuntime } from '../hooks/runtime'
 7 | import { sceneOf } from '../hooks/scene'
 8 | 
 9 | const token = process.env.ZIIKOO_SCENE_TOKEN
10 | if (!token) throw new Error('缺少 ZIIKOO_SCENE_TOKEN')
                           ^
error: 缺少 ZIIKOO_SCENE_TOKEN
      at /home/laoxu/orca/workspaces/orca-factory/OF-388-executor/.claude/skills/ziikoo-driver/scene/server.ts:10:23
      at loadAndEvaluateModule (2:1)

Bun v1.3.1 (Linux x64)
`,
  exitCode: 1,
}
