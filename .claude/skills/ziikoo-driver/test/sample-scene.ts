// 2026-11-05 在 .claude/skills/ziikoo-driver 录制，bun --version 输出 1.3.1，网卡 enp3s0 的 IPv4 为 10.10.10.121。
// 这些输出都由本 mod 的 scene/server.ts 自己写出，不是外部系统输出；插件测试不能读插件目录外文件，按 test/sample-logs.ts 的做法内嵌原文。
// token 是录制专用的假值，不是凭据：T=$(printf 'deadbeef%.0s' {1..8})。
// stdout、stderr、退出码写入当前 worktree 的 .omc/tmp/scene-{listen,no-interface,port-in-use,bad-port}.{out,err,code}。
// 监听成功，退出码 124 来自 timeout，stdout 含行尾：
// ZIIKOO_SCENE_TOKEN=$T ZIIKOO_SCENE_BIND=enp3s0 ZIIKOO_SCENE_PORT=4300 timeout -s INT 3 bun run scene/server.ts
// 接口不存在，退出码 1：
// ZIIKOO_SCENE_TOKEN=$T ZIIKOO_SCENE_BIND=nosuch0 ZIIKOO_SCENE_PORT=4300 timeout -s INT 3 bun run scene/server.ts
// 端口占用，退出码 1，先启动占用进程，录完只按 PID 清理：
// ZIIKOO_SCENE_TOKEN=$T ZIIKOO_SCENE_BIND=enp3s0 ZIIKOO_SCENE_PORT=4300 bun run scene/server.ts >/dev/null 2>&1 & P=$!
// ZIIKOO_SCENE_TOKEN=$T ZIIKOO_SCENE_BIND=enp3s0 ZIIKOO_SCENE_PORT=4300 timeout -s INT 3 bun run scene/server.ts
// kill "$P"
// 非法端口，退出码 1：
// ZIIKOO_SCENE_TOKEN=$T ZIIKOO_SCENE_BIND=enp3s0 ZIIKOO_SCENE_PORT=abc timeout -s INT 3 bun run scene/server.ts
// 缺少 token，退出码 1，stderr 与退出码写入当前 worktree 的 .omc/tmp/scene-no-token.{err,code}：
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
  stderr: `工坊沙盘服务端口必须是 0–65535 的整数，收到 abc
`,
  exitCode: 1,
}

export const SAMPLE_SCENE_NO_TOKEN = {
  stderr: `缺少 ZIIKOO_SCENE_TOKEN
`,
  exitCode: 1,
}
