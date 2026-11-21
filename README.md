# remote-cc (rcc)

本机不用安装 Claude Code，也能用 Claude Code 写本机的代码。

```
A (本机，运行 rcc)  ──ssh──►  B (远程，装有 claude)
```

`rcc` 从 A ssh 到 B，在 B 上启动 Claude Code，界面显示在 A 的终端里：

- **命令**：通过 `CLAUDE_CODE_SHELL_PREFIX` 加反向 unix socket，Bash 工具的命令在 A 上执行。
- **文件**：通过反向 sshfs（`sshfs -o slave` 加 rcc 内置的 SFTP 服务），把 A 的项目目录挂载到 B 上相同的绝对路径，
  Read/Write/Edit/Glob/Grep 实际读写的是 A 上的文件。

详细设计见 [docs/DESIGN.md](docs/DESIGN.md)。

## 用法

```sh
rcc user@B                     # 在当前目录启动 Claude Code
rcc user@B ~/proj              # 指定项目目录
rcc -p 2222 -i ~/.ssh/key user@B . -- --resume   # ssh 参数，-- 后面的参数传给 claude
rcc user@B -- --continue       # 继续这个目录上次的对话（--resume 选择历史对话）
rcc --profile work user@B      # 指定历史 profile（默认每台本机各自一份；none = 用 B 原本的历史）
rcc user@B -- --dangerously-skip-permissions   # B 是 root 时也能用（rcc 会设置 IS_SANDBOX=1，--no-is-sandbox 可关闭）
rcc -v ...                     # 显示过程
```

对话历史保存在 B 上，按本机隔离：两台机器即使项目路径相同，也不会看到对方的对话（需要 B 用 root 运行）。

A 上执行的命令会记录到 `~/.cache/rcc/last.log`（Linux；其它平台在对应的用户缓存目录下）。

## 状态

- [x] M1 命令通道（`CLAUDE_CODE_SHELL_PREFIX` → `rcc-shim` → 反向 socket → A）
- [x] M2 文件通道（内置 SFTP 加 `sshfs -o slave`，挂到同一路径）
- [x] M3 一键启动，退出或收到信号时自动清理
- [ ] M4 增强（见 docs/DESIGN.md）

## 构建

```sh
./scripts/build.sh        # 输出 dist/rcc-{linux,darwin,windows}-{amd64,arm64}，内嵌 B 端 shim
go test ./...             # 包括真实 sshfs 挂载测试（需要 root、sshfs 和 /dev/fuse，否则跳过）
```

## 依赖

- A：`ssh` 客户端、`bash`（Windows 上是 Git Bash）
- B：Linux、`claude`、`sshfs`、FUSE；用 root 时还需要 `unshare`/`nsenter`（util-linux）
