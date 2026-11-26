# 架构

> 读者：改 remote-cc 代码的开发者

remote-cc 让运行在服务器容器里的 Claude Code 把命令和文件操作都落在电脑上。本文只描述当前版本的设计；版本历史见 [CHANGELOG.md](../CHANGELOG.md)，带日期的实测和复盘见 [docs/records/](#记录)。

## 两条通道

Claude Code 的工具分两类，所以只替换 shell 不够：

| 工具 | 实际做什么 | 怎样落到电脑上 |
| --- | --- | --- |
| Bash | 起一个 shell 进程执行命令 | 命令通道：经 `CLAUDE_CODE_SHELL_PREFIX` 转发到电脑 |
| Read、Write、Edit | 在 Claude Code 进程里直接读写文件 | 文件通道：电脑上的项目目录挂载到容器里 |
| Glob、Grep | 起一个 ripgrep（`rg`）进程遍历、搜索文件 | 命令通道：搜索项目目录的 `rg` 在电脑上执行（见下文 **rg**） |

文件通道把项目目录挂载到容器里**相同的绝对路径**：容器里的 `/home/me/proj/src/a.go` 和电脑上的是同一个文件，Claude 写在命令里的路径和 cwd 在两边都成立，不需要任何路径翻译（Windows 上 Git Bash 形式的 `/c/…` 除外，见下文）。

```
 电脑（remote-cc）                    一条 ssh 通道              容器
 ┌──────────────────────────┐                          ┌──────────────────────────────────────┐
 │ remote-cc                │                          │ 会话守护进程（rcc-shim __rcc-daemon） │
 │  可续接字节流 + yamux ═══╪══ ssh … __rcc-dial ══════╪═ 可续接字节流 + yamux                │
 │  ├─ 终端 ◄───────────────┼── 流 T ─────────────────┼─► 伪终端 ─► claude                   │
 │  ├─ SFTP 服务（$P）◄─────┼── 流 F ─────────────────┼── sshfs -o slave :$P $P（FUSE）       │
 │  └─ 命令服务 ◄───────────┼── 每条命令一个流 ───────┼── 命令 socket ◄── rcc-shim ◄── Bash、git │
 └──────────────────────────┘                          └──────────────────────────────────────┘
          $P = 电脑上项目的绝对路径，容器里也挂在 $P
```

- **会话守护进程**：每个会话一个，脱离 ssh 会话运行（`setsid`），claude 和 sshfs 都挂在它下面，所以断线不会结束它们。它监听运行目录里的两个 socket：`session.sock` 供 remote-cc 连接（经 `rcc-shim __rcc-dial`，它只把 ssh 的 stdin/stdout 接到这个 socket 上），`s` 供 rcc-shim 发命令。
- **一条通道上的三种流**：remote-cc 和守护进程之间是一条可续接字节流（`internal/resume`），上面用 yamux 分出多条流：终端（claude 的伪终端，带窗口尺寸和退出码）、文件（sshfs 的 SFTP），以及每条命令一条流。
- **文件**：`sshfs -o slave` 用 stdin/stdout 跑 SFTP 协议，remote-cc 用自己进程内的 SFTP 服务回答，所以电脑上不需要 sshd，也不需要 sftp-server（「反向 sshfs」）。SFTP 服务只暴露项目目录（按路径校验，拒绝 `..` 越界），这是防误操作，不是安全边界。
- **命令**：rcc-shim 连到守护进程的命令 socket，守护进程为它开一条流，电脑上的命令服务执行。连接要带这个会话的一次性 token。
- **ssh 连接复用**：Linux 和 macOS 上，同一个目标的 ssh 调用经 ControlMaster 复用一条连接；这条连接在最后一次使用后保留 10 分钟（`internal/remote` 的 `MasterPersist`），下一次启动或命令不用重新握手。Windows 自带的 OpenSSH 不支持复用，一次会话建立两条连接（探测、会话）。

## 一次会话

1. **探测**：一次 ssh 调用，收集容器的系统、架构、uid、HOME、所需程序和 claude 的路径，以及已安装 shim 的 sha256。缺什么就给出明确的报错；连到宿主机而不是容器时，报错会指出这一点。同时在电脑上并行进行：在电脑的 shell 里试跑一条命令（失败就停下），以及 `--rcb` 时向 RCB 取会话授权。
2. **上传 shim**：只在容器里还没有这个版本时上传，有 gzip 就压缩，容器里校验 sha256。
3. **启动会话**：一次 ssh 调用运行 `internal/scripts/client/session.sh`：清理以前留下的运行目录，在 umask 077 下创建 `~/.cache/rcc/run/<id>/`，写入启动脚本，启动守护进程，然后在同一个 ssh 通道上连到它。remote-cc 先发 token（守护进程写进运行目录，不出现在命令行里），再同时开文件流和终端流。
4. **守护进程**：收到文件流就在私有挂载命名空间里启动 sshfs；挂载点就绪后，在伪终端里运行启动脚本（`launch.sh`），它设置 `CLAUDE_CODE_SHELL_PREFIX` 等变量，进入项目目录后 `exec claude`。`--` 之后的参数原样交给 claude。电脑的终端切到 raw 模式，按键和窗口尺寸经终端流传过去，`TERM` 和 locale 变量也一起带过去。
5. **结束**：claude 退出后，守护进程把退出码发给 remote-cc，等它确认后结束 sshfs，删除运行目录和创建的挂载点父目录。remote-cc 收到 SIGTERM、SIGHUP 时让守护进程给 claude 发 SIGHUP，再走同一条路径。

容器里的脚本统一以 base64 交给 `sh -c` 执行，不依赖登录 shell 是 bash、zsh 还是 fish。shim 按内容寻址存放在 `~/.cache/rcc/shims/<sha256>/`，不同版本的 remote-cc 不会互相覆盖，30 天没用过的版本会被清理（`internal/scripts/client/session.sh`）。

## 断线和续接

remote-cc 和守护进程之间的字节流可以换底下的 ssh 连接（`internal/resume`）：

- 两边都保留已经发出、对方还没确认收到的数据。接上新连接时，两边先说自己总共收到了多少字节，各自丢掉对方已有的部分，再补发其余的。每个字节恰好送达一次，顺序不变。所以断线时正在执行的命令、正在进行的文件读写和终端输出，重连后都接着进行；断线期间新发起的命令和按键排队，重连后送达。
- 两边每 5 秒发一次心跳，15 秒收不到对方的任何数据就判定断线（`resume.Options`），比依赖 TCP 更快，也能发现对方被冻结、连接却没断的情况。ssh 本身用 `ServerAliveInterval=5`、`ServerAliveCountMax=3`。
- 判定断线后，remote-cc 在终端上提示，按 1、2、4、8、10 秒的间隔重新连接（每次连接最多等 15 秒、握手最多 20 秒），连上后经 `dial.sh` 接回同一个守护进程。按 Ctrl-C 放弃重连。守护进程在重连后把伪终端的尺寸改一下再改回来，让 claude 重画界面。
- 没有连接时守护进程保留会话 10 分钟（`internal/session` 的 `DefaultGrace`），超时后结束 claude 和 sshfs 并清理。会话已经结束时，`dial.sh` 回答「the session on the server has ended」，remote-cc 不再重试。

## 命令通道

Claude Code 支持环境变量 `CLAUDE_CODE_SHELL_PREFIX`：设置后，它把整串命令作为一个参数交给这个前缀程序。前缀是 `rcc-shim`，它：

1. 把命令串、cwd 和 Claude 为命令设置的变量（`CLAUDE*`、`TERM` 等）发给电脑。容器的 PATH、HOME 不带过去。
2. 改写命令：去掉加载容器 shell 快照的那一句（电脑上用自己的环境）；Claude 用来追踪 cwd 的 `pwd -P >| <文件>` 改为在电脑上记录，执行完由 shim 写回容器里的原路径。因为是同路径挂载，电脑上的 cwd 在容器里同样有效。
3. 实时转发 stdout、stderr 和 stdin，用相同的退出码退出；SIGINT、SIGTERM、SIGHUP 原样转发，shim 断开时电脑上结束整个进程组。

电脑上用 `bash -c` 执行（不加 `-l`），直接继承启动 remote-cc 时的环境。Windows 上用 Git for Windows 的 bash，避开 `System32\bash.exe`（那是 WSL 的启动器）；命令有自己的隐藏控制台，并先切换到 UTF-8 代码页。

**git**：Claude Code 自己调用 git 时不经过 `CLAUDE_CODE_SHELL_PREFIX`。会话的 PATH 最前面放一个 `git`，它是 `rcc-shim` 的链接：调用作用在项目目录内（考虑 `-C`、`--git-dir`、`--work-tree`）时，经命令通道把参数数组直接交给电脑上的 git，不经过 shell；作用在项目目录外时执行容器自己的 git。Windows 上双向转换 `/c/x` 和 `C:/x` 形式的路径。`--remote-git` 关闭这一层。

**rg**：Claude Code 的 Glob、Grep 工具和 `@` 文件补全（不在 git 仓库里时）起 ripgrep 遍历目录；经 sshfs，每个目录、每个文件都要一个网络往返，中等大小的仓库要几十秒。启动脚本设置 `USE_BUILTIN_RIPGREP=0`，让 Claude Code 从 PATH 取 `rg`，并在 PATH 最前面放一个 `rg`，它也是 `rcc-shim` 的链接（`internal/shim/rg.go`）：

- 它解析 rg 的选项，找出要搜索的路径。所有路径都在项目目录内、cwd 也在项目目录内（或者路径全是绝对路径）时，经命令通道把参数数组原样交给电脑上的 `rg`，在同一个 cwd 里执行，stdout、stderr 和退出码原样传回。两边路径相同，所以输出和在容器里搜索一字不差。电脑上的 rg 加 `--no-config`，不受电脑上 `RIPGREP_CONFIG_PATH` 的影响（Claude Code 自带的 rg 也这样运行）。Windows 上路径参数转换成 `C:/x` 形式，并加 `--path-separator=/`，输出里的路径再转回 `/c/x`。
- Claude Code 在 Linux 上先打开要搜索的目录，以 `/proc/self/fd/<n>` 为 cwd、`.` 为目标启动 rg；shim 看到的 cwd 是这个目录的真实路径。搜索单个文件时，Claude Code 把文件作为文件描述符 3 交给 rg（目标 `/proc/self/fd/3`），这种搜索和其余情况都在容器里执行：路径在项目目录外、读 stdin（`-`）、`-f`/`--file`、`--pre`、`--ignore-file`、没有路径参数（如 `--version`）。容器里用 Claude Code 原生版自带的 rg（以 `rg` 为 argv0 运行 claude 本身，和 Claude Code 自己的做法相同），没有时用启动时 PATH 里的 `rg`（`RCC_REAL_RG`）。
- 电脑上没有 `rg`，或者连不上电脑、命令服务回答错误（在输出任何内容之前）时，同一个搜索改在容器里执行；没有 `rg` 这一点记在运行目录里，这个会话里之后的搜索不再先问电脑。
- Claude Code 原生版默认不提供 Glob、Grep 工具，用 Bash 里的 `grep`、`find` 搜索；这些命令本来就在电脑上执行。用 `--allowedTools` 或 `--tools` 写明 Glob、Grep 时才有这两个工具。

**进程清理**：会话结束时，电脑上由 remote-cc 启动的所有进程都要结束，即使 remote-cc 被 `kill -9`。Windows 上 remote-cc 启动时把自己放进一个带 `KILL_ON_JOB_CLOSE` 的 Job，之后启动的进程都继承它；Unix 上 remote-cc 派生一个看门狗进程，经一条只有 remote-cc 持有写端的管道登记每个进程组，管道一关闭就先 TERM、1 秒后 KILL，再删除临时目录。命令进程与用户的终端隔离（Unix 上 `setsid`，Windows 上 `CREATE_NO_WINDOW`）。

**告诉 Claude 工具在哪里执行**：Claude 系统提示词里的环境信息描述的是容器。remote-cc 用 `--append-system-prompt` 追加一段固定的说明：Bash 在哪台电脑、什么系统和 shell 上执行，项目目录内外的文件分别在哪里，Windows 上 Git Bash 的路径写法。同一台电脑、同一个项目的文本不变，不破坏提示缓存。

## 文件通道

- sshfs 跑在 `unshare -m` 创建的私有挂载命名空间里，claude 经 `nsenter` 进入同一个命名空间。容器的全局文件系统视图不受影响，同一路径即使已经存在也不会被覆盖。这要求容器里是 root，并有 `unshare`、`nsenter`。
- 挂载参数见 `internal/session/daemon_linux.go`：用 `uid`、`gid` 把文件显示为容器当前用户所有；属性缓存 1 秒，「不存在」不缓存（Claude 经 git 在电脑上建好的目录要马上可见），目录缓存 5 秒（一次 READDIR 带回所有条目的属性，关掉会让大目录慢很多）。
- SFTP 服务支持 posix-rename、statvfs、hardlink，相对链接的目标原样保存，忽略 chown。

## Claude 账号和历史档案

- **Claude 账号**：一个 Claude 账号除了凭据，还有 `.claude.json` 里的账号信息和组织策略文件，只换凭据会身份不一致。所以每个账号在容器里有自己的目录 `~/.local/share/rcc/accounts/<账号>`，claude 经 `CLAUDE_CONFIG_DIR` 使用它；个人设置通过链接共用（清单在 `internal/scripts/lib/account.sh`）。长期令牌存在账号目录的 `.rcc-oauth-token`（0600），会话里导出为 `CLAUDE_CODE_OAUTH_TOKEN`。不指定账号时用这台电脑自己的账号，名字和它的历史档案相同。
- **历史档案**：同路径挂载让两台电脑上路径相同的项目在容器里重合。挂载脚本在私有命名空间里把 `~/.local/share/rcc/profiles/<历史档案>/projects` 和 `history.jsonl` bind mount 到账号配置目录的同名位置，只有这个会话里的 claude 看得到。命名账号下历史档案名加 `@<账号>` 后缀。
- RCB 浏览器配置的 Claude 账号（`rcb-*`，见 [docs/usage.md](usage.md#rcb)）只能在该配置的节点里使用，容器里的脚本会拒绝在节点之外使用它。

## 服务器布局

- **一个镜像**：网关和用户容器用同一个镜像（`deploy/Dockerfile`，debian-slim 加 openssh-server、sshfs、fuse3、git 和固定版本的 Claude Code），靠 `RCC_ROLE=gateway` 区分角色。Claude Code 的自动更新经 managed settings 关闭。
- **网关**：标准 OpenSSH，独占唯一的对外端口。`rcc-gateway-sync` 按服务器用户名单维护网关上的 Linux 账号，每个账号只有 `PermitOpen rcc-<用户名>:22`，不能分配 TTY，也不能做其他转发。账号唯一能执行的程序是 `rcc-gateway-cmd`，它只能读网关的主机公钥，以及为调用者自己登记设备邀请码（经 sudo 调用 `rcc-self-invite`，sudoers 只放行这一个程序）。
- **用户容器**：每个服务器用户一个容器 `rcc-<用户名>`，挂在私有网络上，没有发布的端口。它只挂载自己的数据目录、公钥文件和出口主机 socket 目录。sshd 只允许密钥登录，只放行 unix socket 的反向转发：`AllowTcpForwarding remote` 加 `PermitListen none`，因为 `AllowTcpForwarding no` 会连 unix socket 的反向转发一起拒绝。
- **公钥**：每个服务器用户的公钥在宿主机上只有一份。网关只读地挂载它用于认证；用户容器也用它认证，所以即使绕过网关的路由，别人的容器也不接受这把密钥。
- **邀请码**：内含一把一次性密钥，分成两半，都带 `expiry-time`。网关这一半在用户容器看不到的 `<数据目录>/invites/<用户名>`，只能跳转到 `rcc-<用户名>:22`；容器这一半只能执行 `rcc-enroll`，没有任何转发。`rcc-enroll` 只接受一行不带选项的公钥，登记后删除这把一次性密钥。
- **compose 项目名**：每个容器显式使用项目名 `rcc-<用户名>`，网关使用实例的网络名；所有操作前检查同名容器是否属于本实例，从不使用 `--remove-orphans`。

<a id="egress"></a>

## 出网

`--rcb` 会话的全部网络流量经 RCB 浏览器配置的节点出去。出口主机 `rcc-egress` 是宿主机上的一个容器（部署见 [deploy/README.md](../deploy/README.md#egress-host)）。

```
RCB 工作台：浏览器配置（Claude 账号、节点、出口、时区），签发会话授权
  ▲ 主机令牌：兑换会话授权、复查节点          ▲ 客户端令牌：读配置、要会话授权、一键登录
  │                                            │
rcc-egress ◄── 每个服务器用户自己的 socket ── rcc-<用户名> ◄──ssh── 电脑
  每个（服务器用户, RCB 用户, 节点）            claude 进入命名空间后启动      remote-cc --rcb <配置>
  一个网络命名空间
```

- **会话授权**：出口主机手里没有节点列表。每次启动会话，remote-cc 用客户端令牌向 RCB 要一份会话授权，经用户容器交给 `rcc-egress`；`rcc-egress` 用主机令牌向 RCB 兑换，得到这个节点的完整配置。授权的内容、有效期和兑换规则由 RCB 定义，见 [RCB：会话授权](https://github.com/NotDesigned/remote-claude-browser/blob/main/docs/remote-cc.md#grants) 和 [RCB：接口](https://github.com/NotDesigned/remote-claude-browser/blob/main/docs/remote-cc.md#api)。
- **命名空间**：每个（服务器用户, RCB 用户, 节点）一个网络命名空间，各有自己的 Mihomo，不同服务器用户即使用同一个节点也不共用连接和 DNS 缓存。命名空间里唯一的默认路由是连到节点的 Mihomo TUN；只有 Mihomo 自己的连接（带路由标记）能用通往外网的 veth，防火墙拒绝其他一切，所以 Mihomo 停止时命名空间没有任何出口，不会退回直连。DNS 由 TUN 接管，经节点解析；IPv6 只允许 Mihomo 自己使用（`internal/egress/netns.go`）。数量上限见 [deploy/README.md](../deploy/README.md#egress-host)。
- **进入命名空间**：会话开始时，`rcc-shim` 从该服务器用户自己的 socket 取得命名空间的文件描述符，进入后做启动检查（见 [docs/usage.md](usage.md#rcb-checks)），再启动 claude。所以不依赖 `HTTPS_PROXY`：claude、它启动的 MCP 服务器、WebFetch 和遥测都只能经节点出网。Bash 命令和文件照常在电脑上；Claude 的登录凭据（`CLAUDE_CODE_OAUTH_TOKEN`、`ANTHROPIC_API_KEY` 等）不随命令转发到电脑。
- **socket**：每个用户容器只挂载自己的 `<数据目录>/egress/users/<用户名>`。socket 先在只有 `rcc-egress` 能写的目录里建好，再移进去，用户放进去的符号链接不会被跟随。
- **复查**：运行中的会话不依赖 RCB。`rcc-egress` 每 5 分钟（`internal/egress/daemon.go` 的 `RefreshEvery`）只为正在运行的节点、按 RCB 用户逐个复查：用户被停用、配置不再绑定这个节点、节点被删除或配置变了时，停止这个用户的这个节点，正在用它的会话断网；RCB 连不上或回答不明确时保持运行。主机令牌连续两轮被拒绝（`deniedLimit`）后，停止所有节点。
- 会话结束后命名空间和 Mihomo 继续运行，直到复查让它停止、为新节点腾位置时被停止，或出口主机重启。

## 安全模型

- **服务器必须可信**：容器里的 Claude（以及容器里的 root）对电脑拥有完整的执行和读写权限，这本来就是这个工具的目的。
- **服务器用户之间**：每个服务器用户有自己的容器，也就有自己的 root、Claude 账号和历史档案。网关只放行用户自己的容器，用户容器只接受这个用户的公钥。同一个服务器用户的几台电脑共用一个容器，Claude 账号分开只是为了不混用，不是安全边界。
- **容器权限**：sshfs（项目目录）、`unshare`/`nsenter`（私有挂载命名空间）和进入 `--rcb` 会话的网络命名空间，都需要 `/dev/fuse`、`CAP_SYS_ADMIN`，并且要关掉 Docker 默认的 AppArmor 限制（默认规则禁止 mount）。不需要的 `NET_RAW`（伪造同一网络上别的容器的报文）和 `MKNOD`（创建设备文件）被去掉；不用 `--privileged`，不挂载宿主机的其他目录。
- **容器边界因此变弱**：用户是容器里的 root，又有 `CAP_SYS_ADMIN`、没有 AppArmor，容器和宿主机之间只剩 Docker 默认的 seccomp 规则和命名空间。内核漏洞或配置不当（例如宿主机用 cgroup v1）时，用户有可能逃出容器、成为宿主机的 root。所以服务器用户之间的隔离只适合彼此大致可信的人（同一个团队），不能当作抵御恶意用户的安全边界。要更强的隔离，部署者可以给 Docker 开启 `userns-remap`（容器里的 root 映射成宿主机上的普通用户；数据目录的属主要随之改成映射后的用户，remote-cc 不自动处理），或者给不可信的人一台单独的宿主机。
- **宿主机上的脚本不碰容器能写的文件**：管理命令以宿主机 root 执行，而用户的公钥目录、`/root` 和 `/etc/ssh/keys` 是挂进容器、容器里的 root 能写的宿主机目录（网关的目录同理）。这些目录里的文件只在对应容器里经 `docker exec` 读写，宿主机只创建挂载点本身，所以容器里放的符号链接、FIFO 或设备文件只能指向容器自己的文件（`internal/scripts/lib/docker.sh`）。
- **邀请码**：一次性、有时限；网关一半和容器一半分开存放，未使用的邀请码不能打开任何转发。用户自己发的邀请码由 `rcc-self-invite` 写入，只接受调用者自己的账号和一把 ed25519 公钥，有效期和未用邀请码的数量都有上限（见 [docs/usage.md](usage.md#设备)）。
- **出口主机**：能拿到在这里运行过会话的 RCB 用户的节点配置和凭据；`rcb-*` 账号的 Claude 凭据保存在用户容器里。两种令牌各自能做什么、怎样撤销，见 [RCB：令牌种类和权限](https://github.com/NotDesigned/remote-claude-browser/blob/main/docs/remote-cc.md#tokens) 和 [RCB：撤销](https://github.com/NotDesigned/remote-claude-browser/blob/main/docs/remote-cc.md#revocation)。

## 已知限制

- 项目目录里指向目录外的链接，在容器里解析到容器自己的文件系统。
- 电脑上没有 `rg` 时，Glob、Grep 在容器里经 sshfs 遍历，大仓库里会很慢。电脑上的 rg 版本太旧、不认识 Claude Code 用的选项时，搜索报错而不是改在容器里执行。
- 断线超过 10 分钟，会话结束。电脑上的 remote-cc 自己退出后（比如关掉了终端），不能再接回服务器上仍在运行的会话。
- claude 在容器里用的是非登录 ssh 会话的环境，只写在 `.bashrc` 里的变量读不到。

## 决策

- **同路径挂载**：项目挂到容器里相同的绝对路径，Claude 原生的文件工具不用改就能用，命令里的路径和 cwd 两边都成立。代价是容器里要能创建任意路径，所以 claude 跑在私有挂载命名空间里。
- **`CLAUDE_CODE_SHELL_PREFIX` 而不是 hook**：PreToolUse hook 只拦得住 Bash 工具；前缀还覆盖 `!` 命令和其他起 shell 的地方，也不依赖 hook 的 JSON 协议。
- **不用 MCP 代理全部工具**：那样容器里不需要 FUSE，但会丢掉内置 Edit、Read 的语义和 diff 界面。
- **调用系统 ssh**：`~/.ssh/config`、ssh-agent、ProxyJump 都能直接用，几乎所有电脑都自带 ssh。
- **服务端是容器**：Claude 在项目外的读写、WebFetch、MCP 只作用于容器，不作用于宿主机；容器里是 root，私有挂载命名空间不需要额外的提权程序。
- **一个网关、每个服务器用户一个容器**：共用一个容器做不到隔离；每个容器一个端口又要逐个去云防火墙放行。网关用标准 OpenSSH，只依赖它自带的 `authorized_keys` 选项。
- **邀请码分两半**：一次性密钥的转发权限如果写在网关和容器共用的文件里，在容器里也会生效。分开存放后，容器那一半没有任何转发。
- **默认每台电脑一个 Claude 账号和历史档案**：同一个人换电脑的效果和在新电脑上直接装 Claude Code 一样；需要共用时显式用 `--account`、`--profile`。
- **镜像由 CI 构建**：本地构建镜像会临时占用大量构建缓存（见 [CONTRIBUTING.md](../CONTRIBUTING.md#镜像)），服务器只拉取镜像。
- **出网用网络命名空间而不是代理变量**：`HTTPS_PROXY` 拦不住不读它的程序；命名空间里没有别的出口，Mihomo 停止时就是断网。
- **自己做续接，不用 tmux 或 mosh**：tmux 只能保住终端，命令和文件通道照样断；mosh 要求开放 UDP 端口，也不承载文件和命令。在 ssh 之上加一层可续接字节流，三种流一起保住，也不用多开端口。
- **会话守护进程放在 rcc-shim 里**：rcc-shim 本来就按版本上传到容器，守护进程跟着它走，不需要改服务端镜像，不同版本的 remote-cc 可以同时连同一个容器。
- **会话授权而不是节点列表**：出口主机只拿到正在启动的会话要用的节点，被撤销的用户和节点在下一轮复查时停止。

## 记录

带日期的实测、复盘和早期计划，写成后不再修改：

- [2026-11-21 初始设计和实测](records/2026-11-21-初始设计和实测.md)
- [2026-11-21 与 cc-desk-tunnel 的对比](records/2026-11-21-与-cc-desk-tunnel-的对比.md)
- [2026-11-24 compose 项目名冲突事故](records/2026-11-24-compose-项目名冲突事故.md)
- [2026-11-24 服务端和多用户的实测](records/2026-11-24-服务端和多用户的实测.md)
