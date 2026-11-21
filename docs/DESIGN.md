# remote-cc 设计

## 目标

机器 **A**（本地，写代码的地方）没有安装 Claude Code；机器 **B**（远程）装有 Claude Code。
在 A 上运行 `rcc`，通过 `ssh B` 建立连接，在 A 的终端里使用 B 上的 Claude Code，
而 Claude Code 的**所有文件读写和命令执行都落在 A 上**。

## 核心结论：只替换 bash 不够

Claude Code 的工具分两类：

| 工具 | 实际做什么 | 只替换 bash 时会怎样 |
| --- | --- | --- |
| Bash | 起一个 shell 进程执行命令 | ✅ 可以转发到 A |
| Read / Write / Edit | 在 Node 进程里直接读写文件 | ❌ 改的是 B 的磁盘 |
| Glob / Grep | 进程内遍历目录，或者调用内置 ripgrep | ❌ 搜的是 B 的磁盘 |

所以需要两条通道：

1. **命令通道**：Bash 工具的命令转发到 A 执行。
2. **文件通道**：把 A 的项目目录挂载到 B 上，并且**挂载到相同的绝对路径**，这样 Read/Edit 等工具不用改也能用。

挂载到相同路径是整个方案的关键：B 上 `/home/me/proj/src/a.go` 和 A 上的是同一个文件，
Claude 在命令里写的路径、cwd 在两台机器上都成立，所以不需要做任何路径翻译。

## 架构

```
 A (运行 rcc，没有 claude)                       B (有 claude 和 sshfs)
 ┌──────────────────────────┐   一条 ssh 连接   ┌───────────────────────────────────────┐
 │ rcc                      │ ════════════════► │ sshd                                  │
 │  ├─ PTY 透传 ◄───────────┼── 会话 1 ─────────┼─► claude (TUI，显示在 A 的终端上)      │
 │  │                       │                   │     cwd = $P，                        │
 │  │                       │                   │     CLAUDE_CODE_SHELL_PREFIX=rcc-shim │
 │  ├─ SFTP 服务 (服务 $P) ◄┼── 会话 2 ─────────┼── sshfs -o slave :$P $P   (FUSE)     │
 │  │                       │                   │     ▲ Read/Write/Edit/Glob/Grep        │
 │  └─ exec 服务 ◄──────────┼── -R unix socket ─┼── rcc-shim ◄── Bash 工具             │
 └──────────────────────────┘                   └───────────────────────────────────────┘
        $P = A 上的项目绝对路径，B 上也挂在 $P
```

- **会话 1（交互）**：`ssh -t B 'cd $P && CLAUDE_CODE_SHELL_PREFIX=~/.cache/rcc/rcc-shim claude'`，TUI 直接显示在 A 的终端。
- **会话 2（文件）**：`sshfs -o slave` 用 stdin/stdout 跑 SFTP 协议，rcc 把这个会话的标准输入输出接到**自己进程内的 SFTP 服务**上（Go 用 `github.com/pkg/sftp`）。
  这样 A 上**不需要 sshd，也不需要 sftp-server**，这就是"反向 sshfs"。
- **exec 通道**：`ssh -R $SOCK_B:$SOCK_A` 把 B 上的一个 unix socket 反向转发到 A 上 rcc 监听的 socket（OpenSSH 6.7 起支持）。
  B 上的 shim 连这个 socket 就能让 A 执行命令。

在 Linux/macOS 上，几次 ssh 调用通过 ControlMaster 复用同一条连接，只认证一次。Windows 自带的 OpenSSH 不支持连接复用，所以会建立多条连接。

### 一次会话的完整流程（`internal/session`）

1. **probe**：一次 ssh 调用，收集 B 的 OS/arch、uid、HOME、sshfs/unshare/nsenter 是否存在、claude 路径（依次尝试 PATH、`$SHELL -lc`、`$SHELL -ic` 和几个常见位置）、已安装 shim 的 sha256。缺什么就给出明确报错。
2. **prepare**：清理以前崩溃留下的 run 目录；在 umask 077 下创建 `~/.cache/rcc/run/<id>/`；通过 **stdin** 写入 token（不出现在任何命令行里）。如果 hash 不一致，再从 stdin 上传 shim。
3. **agent**：A 上监听一个私有 unix socket（Windows 上是 `127.0.0.1` 的 TCP 端口），要求 token。
4. **文件会话**：`ssh -T -R <run>/s:<agent>` 在 B 上运行 `sshfs -f -o slave`，stdio 接到 A 进程内的 SFTP 服务。收到 sshfs 的第一个字节，说明认证已完成、sshfs 已经启动。
5. **交互会话**：`ssh -t` 运行 run 脚本。脚本先等挂载点和 socket 就绪，再设置 `RCC_SOCK`/`RCC_TOKEN`/`CLAUDE_CODE_SHELL_PREFIX`，进入项目目录后 `exec claude`。`rcc ... -- <args>` 后面的参数会原样传给 claude。
6. **teardown**：claude 退出后关闭 sshfs 的 stdin，sshfs 随之退出，挂载消失。然后执行 cleanup 脚本：卸载、删除 rcc 自己创建的空目录、删除 run 目录和 socket，最后关闭 ControlMaster。
   rcc 收到 SIGTERM/SIGHUP 时也会走同一条清理路径。

B 端的脚本统一用 `sh -c 'eval "$(printf %s <base64> | base64 -d)"'` 发送，所以不管用户的登录 shell 是 bash、zsh 还是 fish 都能运行。

### 挂载模式

| 模式 | 条件 | 做法 |
| --- | --- | --- |
| `ns`（B 为 root 时默认） | root，且有 unshare/nsenter | sshfs 跑在 `unshare -m --propagation private` 创建的私有挂载命名空间里，claude 通过 `nsenter -t <sshfs pid> -m` 进入同一个命名空间。B 的全局文件系统视图完全不受影响，同一路径即使已经在 B 上存在也不会被覆盖，A 和 B 是同一台机器时也能用。 |
| `global` | 非 root | 普通挂载。路径必须能创建，而且必须是空目录（rcc 不会覆盖非空目录）。 |

## 命令通道细节（已在 Claude Code 2.1.330 二进制里核实）

Claude Code 原生支持环境变量 `CLAUDE_CODE_SHELL_PREFIX`。拼命令的代码大致是：

```js
yo = [...setupParts, `eval ${cmd}`, `pwd -P >| ${cwdFile}`].join(" && ")
yo = prefix ? `${quote(prefix)} ${quote(yo)}` : yo     // OIe()
spawn(shell, ["-c", "-l", yo])
```

所以 B 上实际执行的是：

```sh
bash -c -l "'rcc-shim' '<整串命令>'"
```

shim 的 `$1` 就是完整的命令串，里面包括：

- `source '<B 上的 shell 快照>' 2>/dev/null || true`：B 环境的函数、别名和 PATH。在 A 上**应该丢掉**，换成 A 自己的环境（A 上用 `bash -l` 执行）。
- 若干 `export ...`（TMPDIR、BUN_OPTIONS、GIT_CONFIG_PARAMETERS 等）：基本无害，原样带过去就行。
- `eval '<用户命令>'`：真正要执行的命令。
- `pwd -P >| '/tmp/claude-xxxx-cwd'`：**Claude 靠这个文件追踪 cd 之后的 cwd**。这个文件要写在 **B** 上，Claude 才读得到。

shim 的职责：

1. 把 `$1`、`$PWD`、必要的环境变量（`TERM`、`COLUMNS` 等）发给 A。
2. 改写命令：删掉 `source <snapshot>`；把 `pwd -P >| <B 路径>` 换成写到 A 上的临时文件，执行完把内容带回来，由 shim 写到 B 上的原路径。
   因为是同路径挂载，A 上的 pwd 结果在 B 上同样有效。
3. 实时转发 stdout 和 stderr，转发 stdin（多数情况下是 /dev/null），最后用相同的退出码退出。
4. 处理信号：Claude 超时或者用户按 Esc 时会 kill shim 的进程组，shim 收到 SIGTERM/SIGINT 要通知 A 结束对应的进程组。
5. 后台任务（`run_in_background`）就是一条长时间存活的流，不需要特殊处理。

这条路线比 PreToolUse hook 改写命令更好：hook 只拦得住 Bash 工具，PREFIX 还覆盖 `!` 命令和其它需要起 shell 的地方，而且不依赖 hook 的 JSON 协议。

### exec 协议（草案）

用 unix socket 传长度前缀的帧，每帧是 `type(1B) | len(4B) | payload`：

| 方向 | 类型 | 内容 |
| --- | --- | --- |
| B→A | `START` | JSON：`{cmd, cwd, env, cwdCapture}` |
| B→A | `STDIN` / `STDIN_EOF` | 字节流 |
| B→A | `SIGNAL` | 信号编号 |
| A→B | `STDOUT` / `STDERR` | 字节流 |
| A→B | `EXIT` | JSON：`{code, signal, cwd}` |

每条命令一个连接，并发天然隔离。

实现上的取舍：

- A 端监听 `127.0.0.1` 的 TCP 端口，因为 Windows 的 OpenSSH 对 unix socket 转发支持不可靠；B 端用 unix socket。连接必须带 token 认证，防止 A 上其他本地用户连进来。
- A 端用 `bash -c` 执行命令，不加 `-l`，直接继承启动 rcc 时的用户环境。Windows 上用 Git Bash，并且会避开 `System32\bash.exe`（那是 WSL 的启动器）。
- shim 只转发 Claude 为命令设置的变量（`CLAUDE*`、`__CLAUDE*`、`TERM` 等），B 的 PATH 和 HOME 不带过去。
- shim 断开时（被 SIGKILL 杀掉，或者 ssh 断线），A 端会杀掉整个进程组；SIGINT、SIGTERM、SIGHUP 原样转发。

## 文件通道细节

- 实际使用的 sshfs 参数：`-o slave,uid=<B uid>,gid=<B gid>,attr_timeout=1,entry_timeout=1,negative_timeout=1`。
  用 uid/gid 把所有文件显示成 B 当前用户所有，因为 A 和 B 的 uid 不一定相同。缓存时间设得短，因为 A 上的编辑器也会改文件。
- SFTP 服务基于 `pkg/sftp` 的 RequestServer 和自己写的 handler：支持 posix-rename、statvfs、hardlink；相对软链的目标原样保存；忽略 chown。
  已测试的操作：读写、3MB 文件往返、`mv -f`、truncate、chmod、utimes、`df`，以及在挂载点里 `git init/commit` 后到 A 上 `git fsck`。
- **挂到相同路径时的权限问题**：B 上不一定能创建 `/home/me/proj`。按优先级有三种做法：
  1. 路径本来就可写，或者用户有 sudo：直接 `mkdir -p`。
  2. **用户命名空间**：`unshare --user --map-root-user --mount` 后在私有挂载命名空间里挂载，claude 也在这个命名空间里跑。不需要 root，内核 ≥ 4.18 支持非特权 FUSE。
  3. 都不行：挂到 `~/.cache/rcc/mnt/<id>`，shim 对 cwd 做前缀替换。这只是降级方案，命令文本里的绝对路径翻译不了。
- **性能**：Glob/Grep 在 sshfs 上遍历大仓库会慢。后续可以加一个 PreToolUse hook，把 Grep/Glob 重定向成经 exec 通道在 A 上跑 `rg`；
  或者直接禁用这两个工具，让 Claude 用 Bash 里的 `rg`，这样就在 A 上本地执行了。

## 实现选型

- **语言**：Go。`scripts/build.sh` 先编译 linux/amd64 和 linux/arm64 的 `rcc-shim`（约 2.8MB），用 `go:embed` 嵌进各平台的 rcc 里，所以 rcc 不管跑在什么平台都能给 B 装上 shim。
  直接 `go build` 出来的版本不带内嵌 shim，这时只有 A 和 B 同为 linux 且架构相同才能用（会上传 rcc 自身）。
- **ssh**：MVP 直接调用 A 上的系统 `ssh` 命令。这样 `~/.ssh/config`、ssh-agent、ProxyJump 都能用，几乎所有机器都自带。
  以后再考虑用 `golang.org/x/crypto/ssh` 去掉这个依赖。
- **依赖**：A 上需要 ssh 客户端和 bash；B 上需要 claude、sshfs 和 FUSE（`/dev/fuse`）。

## 安全

- B 上的 Claude（以及 B 上的 root 或同用户的其它进程）对 A 拥有完整的执行和读写权限，这本来就是这个工具的目的。**B 必须是你信任的机器。**
- B 上的 socket 放在 `$XDG_RUNTIME_DIR/rcc/` 或者权限为 0700 的目录里，文件权限 0600。会话结束时清理 socket、卸载挂载点。
- SFTP 服务只暴露 `$P` 子树（按路径字面校验，拒绝 `..` 越界）。这只是防止误操作，不是安全边界，因为命令通道本来就能在 A 上执行任何命令。

## 备选方案（不作为主线）

| 方案 | 优点 | 缺点 |
| --- | --- | --- |
| 自定义 MCP server 代理全部工具，再用 `--disallowedTools` 禁用内置工具 | B 上不需要 FUSE（适合容器） | 丢掉内置 Edit/Read 的语义和 diff 界面，Claude 用起来不如原生顺手 |
| 只设置 `SHELL`/`CLAUDE_CODE_SHELL` 指向包装脚本 | 简单 | 要冒充完整的 bash（快照生成等），而且文件工具照样落在 B 上 |
| A 正向 sshfs 挂 B，在 B 上开发 | 无 | 和需求相反：代码和运行环境都在 B 上 |

MCP 方案可以作为"B 上没有 FUSE"时的降级模式，放到后续里程碑。

## 实测记录（B = Ubuntu 24.04 / Claude Code 2.1.330）

- shim 收到的 argv 只有一项，就是完整命令串，格式和上面分析的一致（样本见 `internal/shim/rewrite_test.go`）。
- cwd 文件路径在不需要引号时是裸路径，例如 `/tmp/claude-b614-cwd`。
- Claude 只接受**工作目录范围内**的 cwd 变化，同路径挂载正好满足这个条件。
- shell 快照的生成不经过 PREFIX，所以 shim 只会处理真正的命令。
- 远程启动 claude 需要注意：非登录 ssh 会话的 PATH 里没有 `~/.local/bin`，M3 要用 `bash -lc` 启动或者写绝对路径。
  另外以 root 身份运行时，Claude 不允许 `--dangerously-skip-permissions`。
- 端到端已验证：真实 `ssh -R`（B 上的 unix socket → A 上的 TCP 端口）加 `claude -p`。
  环境变量取的是 A 的，`cd` 在后续调用里保持，退出码正确透传。

## 里程碑

- **M1 exec 通道** ✅：`rcc serve`（A）加 `rcc-shim`（B），手工设置 `ssh -R` 和 `CLAUDE_CODE_SHELL_PREFIX`，验证 Bash 工具在 A 上执行、cwd 追踪、退出码、Ctrl-C。
- **M2 文件通道** ✅：内置 SFTP 服务加 `sshfs -o slave` 同路径挂载，验证 Read/Edit/Write/Glob/Grep。
- **M3 一键启动** ✅：`rcc B [path]` 自动完成：上传 shim、建连接、挂载、起 PTY 跑 claude、退出时清理。
  已验证：rcc 跑在独立的挂载命名空间里，让 A 和 B 在同一路径下看到不同的目录，结果 Read 读到 A 的内容，Edit/Write 写到 A 的磁盘，Bash 在 A 上执行，B 的真实目录没有变化。
  交互式 TUI 用伪终端驱动测试过，`/exit` 后退出码为 0；收到 SIGTERM 时也能完整清理。
- **M4 增强**：非 root 时用用户命名空间挂载、Grep/Glob 重定向到 A、断线重连、MCP 降级模式、在真实的 Windows/macOS A 上验证。

## 已知限制

- Windows 和 macOS 作为 A 端目前只验证到交叉编译通过，还没在真机上跑过。
- 指向项目目录外的软链在 B 上会解析到 B 自己的文件系统。
- 大仓库里 Glob/Grep 走 sshfs 会比较慢（见 M4）。
- claude 在 B 上用的是非登录 ssh 会话的环境。认证一般靠 `~/.claude/.credentials.json`，没有影响；但如果 API key 只写在 `.bashrc` 里，claude 就读不到。
