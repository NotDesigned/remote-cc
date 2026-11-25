# 2026-11-21 与 cc-desk-tunnel 的对比

> 读者：维护者

状态：存档，写成后不再修改。对应版本：0.1.0。对比对象是 [sun168567/cc-desk-tunnel](https://github.com/sun168567/cc-desk-tunnel) 2026-11-20 的 e5386a8 版本，它的目标相同：Claude Code 跑在 Linux 上，命令在 Windows 电脑上执行。

| | remote-cc | cc-desk-tunnel |
| --- | --- | --- |
| 让工具作用到电脑上的方式 | 透明替换：Bash 走 `CLAUDE_CODE_SHELL_PREFIX`，文件经 sshfs 挂到同一路径，原生 Read、Edit、Grep 直接可用 | 提示词：原生工具仍然作用在 Linux 上，系统提示词让 Claude 用 `ssh device-pwsh <<'EOF' … EOF` 操作 Windows；PATH 最前面放一个带 `-F` 的 ssh 包装 |
| 服务器要求 | sshfs 和 FUSE（root 时用私有挂载命名空间） | Docker 容器，不需要 FUSE |
| 连接 | 电脑经系统 ssh 连接服务器 | 客户端主动建 WSS 到服务端（单端口，证书指纹固定），每条通道承载一条真实的 SSH 连接，接到 Windows 上随包带的回环 sshd |
| 界面 | 原版 Claude Code 终端界面 | Electron 图形客户端（基于 Agent SDK）加终端客户端 ccdt（node-pty） |
| 断线 | 会话结束 | CLI 在服务端继续运行；设备离线时新命令最多等 1 分钟；有宽限期，支持换电脑继续 |
| 历史分开 | 每台电脑一个历史档案，用 bind mount 替换 `projects/` | 服务端按设备加项目分配固定的工作目录 |

## 借鉴并实现了的

- Windows 上用 Job Object 保证 remote-cc 结束时它启动的进程全部结束，以及命令与用户的控制台隔离、先执行 `chcp.com 65001` 避免中文系统按 GBK 输出乱码。Unix 上用看门狗进程实现同样的保证。

## 当时认为以后可以借鉴的

- 断线后保持会话：服务器上的 claude 放进一个能脱离连接继续运行的终端，shim 在 socket 不在时等待一段时间。难点是 slave 模式的 sshfs 会随连接一起断掉。
- 不能用 FUSE 时，退回它这种提示词方式。
