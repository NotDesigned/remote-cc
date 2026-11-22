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

## 告诉 Claude 工具实际作用在哪里

Claude 的系统提示词里有一段环境信息（平台、OS 版本），描述的是 B。如果不另外说明，Claude 会按 B 的系统来写命令，比如在 Windows 或 macOS 的 A 上用 `apt`、GNU 版的 `sed -i`。
rcc 默认通过 `--append-system-prompt` 追加一段固定的说明（`internal/session/prompt.go`），内容如下：
- Bash 在哪台机器上执行：A 的 OS 和版本、架构、主机名、shell。
- 上面那段环境信息描述的是远程主机。
- 项目目录内的文件工具作用于 A；目录外的路径作用于 B，访问 A 上的其它位置要用 Bash。
- A 是 Windows 时，补充 Git Bash 的路径写法（`/c/...` ↔ `C:\...`）和 `cmd //c` 的用法。

文本对同一台机器、同一个项目是确定的，不会破坏提示缓存。用户自己传的 `--append-system-prompt` 会接在后面合并；
如果传了 `--append-system-prompt-file`，或者第一个参数是子命令（`remote-control`、`mcp` 等），rcc 不追加。`--no-prompt` 可以关闭这个功能。
已实测：Claude 能正确回答命令在哪台机器执行，也知道项目目录外的 Read 读的是远程主机上的文件；用户传的追加提示词同时生效。

## 对话历史按 A 隔离

Claude Code 在 `~/.claude/projects/<编码后的路径>/` 下按项目路径保存对话（`--resume`/`--continue` 读的就是这里），
另外在 `~/.claude/history.jsonl` 里追加保存输入历史（上箭头调出的内容）。
因为挂载用的是同一路径，两台 A 上路径相同的项目，在 B 上会被当成同一个项目。

解决办法：每台 A 有一个 profile，默认名字是 `<hostname>-<随机8位hex>`，第一次运行时生成，保存在 A 的 `<UserConfigDir>/rcc/profile`；也可以用 `--profile NAME` 手动指定。
在 ns 模式下，mount 脚本在私有命名空间里多做两次 bind mount：

```
~/.local/share/rcc/profiles/<profile>/projects       → ~/.claude/projects
~/.local/share/rcc/profiles/<profile>/history.jsonl  → ~/.claude/history.jsonl
```

只有这个会话里的 claude 能看到替换后的目录。登录凭据、settings、插件、skills 照常共用，所以不需要每台 A 分别登录。
B 上直接运行的 claude 看到的还是原来的目录。`--profile none` 表示不隔离，直接用 B 原本的历史。

已实测：用两个 profile、相同路径跑三次。A1 让 Claude 记住一个暗号；A2 执行 `--continue` 时没有接上 A1 的对话；A1 执行 `--continue` 正确答出了暗号。B 原本的 `projects/` 没有新增内容。

还没隔离的部分：`~/.claude.json` 里按项目路径记录的状态（是否已信任该目录、项目级允许的工具等）仍然共用。
global 模式（B 非 root）没有私有命名空间，无法做 bind mount，会打印警告，然后使用共享的历史。

## 多账号

一个 Claude 账号不只有凭据：`.credentials.json`、`.claude.json` 里的 `oauthAccount`（邮箱、组织），以及 `policy-limits.json`、`remote-settings.json`（组织策略）都和账号绑定，所以只替换凭据会出现身份不一致。
解决办法是每个账号在 B 上有自己的配置目录 `~/.local/share/rcc/accounts/<name>/`，claude 通过 `CLAUDE_CONFIG_DIR` 使用它。
已从二进制代码和实测两方面确认：设置 `CLAUDE_CONFIG_DIR` 后，`.claude.json` 和 `.credentials.json` 都会放到这个目录里；指向空目录时 `auth status` 显示 `loggedIn: false`，默认账号不受影响。

- 选择账号：`--account NAME`，或者 A 上的环境变量 `RCC_ACCOUNT`。不指定或指定为 `default` 时，使用 B 原来的配置目录，和以前一样。非 root 的 global 模式同样支持。
- 共享的个人配置：`settings.json`、`CLAUDE.md`、`keybindings.json`、`skills/`、`agents/`、`commands/`、`plugins/`、`output-styles/` 通过软链接指向默认配置目录，只链接实际存在的项，已有的文件不会被覆盖。
  所以把某个链接换成真实文件，就能让这个账号单独使用该配置。注意：如果 Claude 用"写临时文件再改名"的方式保存 `settings.json`，链接会被替换成真实文件，之后这一项就不再共享（不会损坏数据）。
  用户级的 MCP 服务器保存在 `.claude.json` 里，不会共享，每个账号要单独添加。
- 历史：在 ns 模式下 profile 变成 `<profile>@<account>`，同一台 A 在不同账号下的历史互不可见。
- 账号命令都只通过 ssh 直接执行，不挂载目录、不启动 agent：
  `rcc login|logout|status|setup-token [--account N] host`，对应 `claude auth …` / `claude setup-token`；
  `rcc token --account N host` 在 A 上以隐藏方式读入令牌，通过 stdin 存到账号目录的 `.rcc-oauth-token`（权限 0600），之后会话里导出为 `CLAUDE_CODE_OAUTH_TOKEN`，`-- --clear` 删除；
  `rcc accounts host` 列出所有账号的登录状态、邮箱、组织、套餐，以及是否保存了令牌。
- 已实测：新账号目录的建立和链接、令牌的保存与清除（权限 600）、accounts 列表、会话里 `CLAUDE_CONFIG_DIR` 和令牌的传递以及历史 profile（用一个打印环境的假 claude 验证）、`login` 能在 A 的终端显示 `https://claude.ai/oauth/authorize` 并等待粘贴授权码。
  真实账号的授权和使用没有测，因为需要在浏览器里完成。`auth status` 对令牌只检查是否配置，不检查是否有效（假令牌也会显示 loggedIn），这一点和 cc-desk-tunnel 文档里的说明一致。

## 服务端容器化

问题：Claude 以 root 身份直接运行在 B 上，它在项目外的 Read/Write、WebFetch、MCP 都作用于宿主机。改用专用用户也不行：这台 Ubuntu 24.04 设置了 `apparmor_restrict_unprivileged_userns=1`，普通用户不能创建挂载命名空间，同路径挂载和历史隔离都需要额外写一个 root 辅助程序。
所以服务端改成 Docker 容器（`deploy/`）：容器内是 root，现有的 ns 模式、历史隔离、多账号都原样可用；宿主机文件对 Claude 不可见。

- **镜像**：debian-slim，装 openssh-server、sshfs、fuse3、procps，外加固定版本的 Claude Code 原生二进制（多阶段构建，只复制二进制）。
  用 managed settings 设置 `DISABLE_AUTOUPDATER=1`，任何账号都生效。镜像由 GitHub Actions 构建 amd64 和 arm64 两个版本并推送到 GHCR。
- **容器权限**：`/dev/fuse` + `CAP_SYS_ADMIN` + `apparmor:unconfined`（Docker 默认的 AppArmor 规则禁止 mount），不加 `--privileged`。
  数据卷 `<data>/root` 挂到容器的 `/root`，保存 Claude 登录、历史、rcc 状态；`<data>/ssh` 保存 sshd 主机密钥，重建容器后指纹不变。
- **sshd**：只允许密钥登录。有一个 OpenSSH 的特殊行为：`AllowTcpForwarding no` 会连 unix socket 的反向转发一起拒绝，因为两者共用远程转发的权限检查。
  所以改用 `AllowTcpForwarding remote` + `PermitListen none`：unix socket 路径不受 PermitListen 限制，TCP 监听全部被拒绝；local 转发本来就不在 `remote` 允许范围内。
  实测：`-R` unix socket 可用，`-R` TCP 端口被拒绝，`-L` 在实际使用时被拒绝（administratively prohibited）。
- **`rcc setup-server`**：一条命令完成部署和更新。compose.yml 通过 go:embed 打包在 rcc 里；公钥直接写进数据卷（不放 .env）。
  容器的主机公钥通过已经验证过的 root ssh 连接取得，并写进本机 `known_hosts`，以 `[host]:port` 为键。`--migrate-history` 会复制宿主机上以前的 rcc 历史 profile。
- **已实测**（A 和 B 是同一台机器，用本地构建的镜像，容器只监听 127.0.0.1）：setup-server 部署完成、known_hosts 写入，并在开启主机密钥校验的情况下连接成功。
  在容器里通过挂载读写了 A 的文件，通过 shim 在 A 上执行了命令，迁移的历史 profile 也挂上了；会话结束后容器里没有残留。
  容器里的 claude 还没登录，所以会话这部分用一个假 claude 测试；真实 claude 只测了 `--version` 和 `auth status`。
- **非 root 宿主机**：setup 脚本发现登录用户不是 root 时会提权重跑。脚本把自己（参数加正文）以 base64 嵌进去，再用 `sudo -n env … sh -c` 执行；需要密码时用 `sudo -S`，密码由 rcc 在本机以隐藏方式读入后经 stdin 传入。
  脚本第一次运行时如果发现需要密码，会输出一个标记后退出，rcc 看到标记再询问密码重跑。登录用户自己的 `authorized_keys` 和 rcc 历史在提权前读出，或者通过 `ORIG_HOME` 传给提权后的脚本。
  实测用了两个临时用户：免密 sudo 时，`--keys-from-host` 和 `--migrate-history` 都取自该用户的家目录；需要密码时，未提供密码、密码错误、密码正确三种情况都符合预期，密码没有出现在进程列表和日志里。
- **磁盘**：在 B 上本地构建会临时产生约 2GB 的构建缓存，开发时一度把这台机器的磁盘写满了。所以镜像改由 CI 构建，服务器只拉取。

## 凭据默认按机器分开、shim 按内容寻址、更新命令（v0.3.0）

- **默认账号**：不指定 `--account` 时，使用本机专属账号，名字和机器 profile 相同（`<hostname>-<随机>`），配置目录是 `accounts/<名字>`；这种情况下历史 profile 不加 `@账号` 后缀，已有的历史位置不变。
  `--account default|shared` 使用 B 上共用的 `~/.claude`。实测：两台模拟的 A 同时连同一个容器、使用同一个项目路径，账号目录、历史、命令执行都各自正确。
- **shim 按内容寻址**：并发测试发现两个 bug。一是两台 A 同时首次连接时，上传 shim 共用了同一个 `rcc-shim.tmp`，导致其中一个 `mv` 失败；二是不同版本的 rcc 会互相覆盖 B 上唯一的 `rcc-shim`。
  现在 shim 放在 `~/.cache/rcc/shims/<sha256>/rcc-shim`，上传时用会话独有的临时文件名，每次使用时更新时间戳，30 天没用过的版本会被清理。prepare 失败时也会执行清理。
- **`rcc update`**：从 GitHub Release 下载本平台的二进制，用 SHA256SUMS 校验，先写到可执行文件同目录的临时文件，再原子改名替换；Windows 上先把正在运行的 exe 改名为 `.old`，下次启动时删除。`rcc version --check` 用于查看最新版本。
- **`rcc update-server`**：总是先拉取镜像，再比较容器当前的镜像 ID 和新镜像 ID，以及 compose 文件内容。都没变就不碰容器；有变化而容器里还有会话在运行时，拒绝执行（退出码 5），除非 `--force`。
  更新后按 `org.opencontainers.image.source` 标签清理悬空镜像。只有容器 ID 真的变了才报告"已更新"。实测了五种情况：无变化、镜像有更新、只改注释、有活跃会话被拒绝、`--force`。
- **端口可达性**：setup-server 最后从 A 直接连一次容器端口，连不上时打印 ProxyJump 配置，其中 `HostKeyAlias [host]:port` 是实测确认过的写法。
- **命令名**：提示文字使用实际调用的程序名。有用户的 PATH 里有 Anaconda/Qt 的 `rcc.exe`，按我们的提示执行 `rcc cc` 时，运行的是那个程序。

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

## 与 cc-desk-tunnel 的对比

[sun168567/cc-desk-tunnel](https://github.com/sun168567/cc-desk-tunnel) 的目标相同：Claude Code 跑在 Linux 上，命令在 Windows 本机执行。（以下内容来自 2026-11-20 的 e5386a8 版本。）

| | remote-cc | cc-desk-tunnel |
| --- | --- | --- |
| 让工具作用到本机的方式 | **透明替换**：Bash 走 `CLAUDE_CODE_SHELL_PREFIX`；文件通过 sshfs 挂到同一路径，原生 Read/Edit/Grep 直接可用 | **提示词**：原生工具仍然作用在 Linux 上，系统提示词告诉 Claude 用 `ssh device-pwsh <<'EOF' … EOF` 操作 Windows；PATH 最前面放一个带 `-F` 的 ssh 包装 |
| B 端要求 | sshfs + FUSE（root 时用私有挂载命名空间） | Docker 容器，不需要 FUSE |
| 连接 | A 通过系统 ssh 连接 B（需要 B 上的 ssh 账号） | 客户端主动建 WSS 到服务端（单端口，证书指纹固定），WSS 里每条通道承载一条真实的 SSH 连接，接到 Windows 上随包带的回环 sshd |
| 界面 | 原版 Claude Code TUI，显示在 A 的终端 | Electron 图形客户端（基于 Agent SDK）+ 终端客户端 ccdt（node-pty） |
| 断线 | 会话结束 | CLI 在服务端继续运行；设备离线时新命令在入口处最多等 1 分钟；有宽限期，支持换电脑继续 |
| 历史隔离 | 每台 A 一个 profile，用 bind mount 替换 `projects/` | 服务端按设备加项目分配固定的工作目录 |

**借鉴并已实现**：
- Windows 上用 Job Object（`internal/procjob`）。rcc 启动时把**自己**放进一个带 `KILL_ON_JOB_CLOSE` 的 Job，之后启动的命令、它们的子进程和 ssh 都自动继承这个 Job，不存在逐个加入时的空档。
  rcc 无论怎么结束（正常退出、关闭控制台窗口、崩溃、被任务管理器结束），内核都会关闭 Job 句柄并结束 Job 里的所有进程。
  这个句柄不可继承，所以子进程不会让 Job 一直存活。Windows 8 起支持嵌套 Job，即使终端本身已经把 rcc 放在某个 Job 里，也能正常工作。
  测试：`go test ./internal/procjob/`（仅在 Windows 上运行）。测试会用 TerminateProcess 结束父进程，然后检查孙进程是否在 5 秒内退出。目前只验证到交叉编译通过。
- Unix 上用看门狗进程实现同样的保证（`internal/procjob`）。rcc 启动时派生 `rcc __rcc_guard`：它在独立会话里，忽略 HUP/INT/TERM，通过一条管道和 rcc 相连，写端只有 rcc 持有。
  rcc 每启动一个命令进程组或一个 ssh 进程，就通过管道登记一次；rcc 的私有临时目录也会登记，cwd 临时文件同样放在这个目录里。
  rcc 不管以什么方式结束（包括 `kill -9`），内核都会关闭管道。看门狗读到 EOF 后，先对仍然存在的目标发 TERM，1 秒后再发 KILL，然后删除临时目录。
  发 TERM 让 ssh 有机会把终端从 raw 模式恢复过来。看门狗每 10 秒剔除一次已经消失的目标，避免 PID 被复用后误杀无关进程。
  测试：`TestCommandsDieWithRcc` 用 `kill -9` 结束 `rcc serve`，检查命令的 shell 及其后台子进程都被结束、看门狗也退出了；关掉看门狗时这个测试会失败。
  真实会话里也做了 `kill -9` 测试：A 上的后台命令、看门狗、临时目录全部被清理；B 上的 sshfs 和 claude 也都结束了。
  B 上的 run 目录要等下一次启动时才清理：有 pid 文件且进程已结束的立即清理；没有 pid 文件的（可能有另一个 rcc 正在启动）要超过 2 分钟才清理。
命令子进程与用户的终端/控制台隔离。
- Unix 上用 `Setsid`，命令没有控制终端。
- Windows 上用 `CREATE_NO_WINDOW`，命令有自己的隐藏控制台，并且先执行 `chcp.com 65001`，避免中文系统上按 GBK 输出导致乱码。cc-desk-tunnel 的做法是 `chcp.com 65001 >nul & bash -s`，子进程同样不带窗口。

**以后可以借鉴的**：
- 断线后保持会话：B 上的 claude 放进一个能脱离连接继续运行的 PTY，shim 在 socket 不在时等待一段时间。难点是 slave 模式的 sshfs 会随连接一起断掉。
- 不能用 FUSE 时，可以退回它这种提示词方式。

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
- 远程启动 claude 需要注意：非登录 ssh 会话的 PATH 里没有 `~/.local/bin`，所以 probe 会主动查找 claude 的绝对路径。
  另外以 root 身份运行时，Claude 不允许 `--dangerously-skip-permissions`，除非设置了 `IS_SANDBOX=1`。
  因此 B 是 root 时，rcc 默认给 claude 设置 `IS_SANDBOX=1`，`--no-is-sandbox` 可以关闭。
  这个变量在 Claude Code 里还有其它小影响：遇到 529 过载会一直重试、会话来源标记为 `is_sandbox`、少数功能被关闭。
  它不会经 shim 转发到 A。已实测：默认可以用 skip-permissions，命令在 A 上执行；加上 `--no-is-sandbox` 后被拒绝。
- 端到端已验证：真实 `ssh -R`（B 上的 unix socket → A 上的 TCP 端口）加 `claude -p`。
  环境变量取的是 A 的，`cd` 在后续调用里保持，退出码正确透传。

## 里程碑

- **M1 exec 通道** ✅：`rcc serve`（A）加 `rcc-shim`（B），手工设置 `ssh -R` 和 `CLAUDE_CODE_SHELL_PREFIX`，验证 Bash 工具在 A 上执行、cwd 追踪、退出码、Ctrl-C。
- **M2 文件通道** ✅：内置 SFTP 服务加 `sshfs -o slave` 同路径挂载，验证 Read/Edit/Write/Glob/Grep。
- **M3 一键启动** ✅：`rcc B [path]` 自动完成：上传 shim、建连接、挂载、起 PTY 跑 claude、退出时清理。
  已验证：rcc 跑在独立的挂载命名空间里，让 A 和 B 在同一路径下看到不同的目录，结果 Read 读到 A 的内容，Edit/Write 写到 A 的磁盘，Bash 在 A 上执行，B 的真实目录没有变化。
  交互式 TUI 用伪终端驱动测试过，`/exit` 后退出码为 0；收到 SIGTERM 时也能完整清理。
- **M4 增强**：断线保持会话（参考 cc-desk-tunnel）、非 root 时用用户命名空间挂载、Grep/Glob 重定向到 A、断线重连、MCP 降级模式、在真实的 Windows/macOS A 上验证。

## 已知限制

- Windows 和 macOS 作为 A 端目前只验证到交叉编译通过，还没在真机上跑过。
- 指向项目目录外的软链在 B 上会解析到 B 自己的文件系统。
- 大仓库里 Glob/Grep 走 sshfs 会比较慢（见 M4）。
- B 非 root（global 模式）时，对话历史不能按 A 隔离。
- claude 在 B 上用的是非登录 ssh 会话的环境。认证一般靠 `~/.claude/.credentials.json`，没有影响；但如果 API key 只写在 `.bashrc` 里，claude 就读不到。
