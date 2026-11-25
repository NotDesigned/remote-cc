# 使用

> 读者：在自己电脑上用 remote-cc 的使用者

本文假设你的电脑已经装好 remote-cc（见 [README](../README.md#安装)），并且已经是某台服务器的服务器用户：自己部署了服务器，或者用部署者发来的邀请码执行过 `remote-cc device join`。

每条命令的全部选项以 `remote-cc help <命令>` 为准（例如 `remote-cc help account login`），任何命令后加 `-h` 也可以；`remote-cc help` 列出所有命令。本文只给常用的例子。

## 两种主机

命令里出现两种主机，不要混用：

- **容器**（命令里写作 `HOST`）：`server setup` 或 `device join` 在你电脑上写好的 ssh 别名，经服务器的网关进入你自己的容器。别名默认是服务器主机名的第一段加 `-cc`，下文写作 `<服务器>-cc`。会话、`account …`、`device …` 都用它。
- **宿主机**（命令里写作 `SERVER`）：服务器本身，以 root 或有 sudo 的账号连接，例如 `root@<服务器>`。只有 `server …` 管理命令用它，见 [deploy/README.md](../deploy/README.md)。

`remote-cc host list` 列出这台电脑上的所有容器别名，以及各自的服务器地址和服务器用户；当前目录用 `rcb use` 记住了哪一个，也会标出来。

## 会话

```sh
cd ~/proj && remote-cc <服务器>-cc               # 在当前目录启动 Claude Code
remote-cc <服务器>-cc ~/proj                     # 指定项目目录
remote-cc <服务器>-cc -- --continue              # 继续这个目录上次的对话（--resume 选择历史对话）
remote-cc <服务器>-cc -- remote-control          # -- 后面的参数原样交给 claude
remote-cc <服务器>-cc -- -w feature-x            # 在新的 git worktree 里工作
remote-cc <服务器>-cc -- --dangerously-skip-permissions
remote-cc -p 2222 -i ~/.ssh/key root@<服务器> .  # 不用 ssh 别名，直接写 ssh 参数
remote-cc -v <服务器>-cc                         # 显示过程；-vv、-vvv、-vvvv 同时让 ssh 输出调试信息
```

- Claude Code 的 Bash 命令在你的电脑上执行，用的是电脑上的 shell 和环境；Read、Edit、Glob、Grep 等文件工具读写的是电脑上的项目目录。项目目录以外的路径指向容器里的文件。
- remote-cc 会给 Claude 追加一段说明，告诉它命令在哪台电脑、什么系统上执行，以及项目目录内外的文件分别在哪里。`--no-prompt` 关闭这段说明；你自己传了 `--append-system-prompt-file`，或者 `--` 后第一个参数是 `remote-control`、`mcp` 这类子命令时，remote-cc 不追加。
- 容器里是 root。remote-cc 默认给 claude 设置 `IS_SANDBOX=1`，所以 `--dangerously-skip-permissions` 可用；`--no-is-sandbox` 关闭。
- Windows 上命令在 Git for Windows 的 bash 里执行。remote-cc 启动前会先在电脑的 shell 里试跑一条命令，失败就停下并显示原因；可以用环境变量 `RCC_SHELL` 指定 bash 的路径。
- 会话结束时，remote-cc 在电脑上启动的命令（包括后台命令）都会被结束，容器里的挂载也会卸载。关闭终端窗口时同样如此。remote-cc 被 `kill -9` 时，电脑上的命令立即结束，容器里的 Claude Code 在 10 分钟后结束。
- 同一台服务器 10 分钟内再次启动（会话或 `account`、`device` 命令）会复用上一次的 ssh 连接，不用重新握手。Windows 上没有这一项。

### 断线

网络中断（换网络、电脑休眠、信号不好）时会话不会结束。remote-cc 在 15 秒内发现断线，在终端上显示 `rcc: connection lost; reconnecting (Ctrl-C ends the session) …`，然后自动重新连接：

- 重连后一切接着进行：容器里的 Claude Code 没有被打断，断线时正在执行的命令照常完成，断线期间输入的内容也会送到。界面会重画一次。
- 按 Ctrl-C 放弃重连，remote-cc 退出。
- 10 分钟内没有重连上，容器里的会话结束。这之后可以重新启动，用 `-- --continue` 接着这个目录上次的对话。

### git

Claude Code 自己调用的 git（判断仓库、读状态、`-w`/`--worktree` 建 worktree 等）也在你的电脑上执行，用的是电脑上的 git、配置、凭据和 hooks，速度和本地一样。worktree 建在电脑上的 `<项目>/.claude/worktrees/<名字>`。作用在项目目录以外的 git 调用（例如插件市场）在容器里执行。加 `--remote-git` 可以让 Claude Code 自己的 git 调用全部改回在容器里执行。

## Claude 账号

默认情况下，每台电脑在容器里用自己的 Claude 账号，所以几台电脑不会共用 Claude 凭据；新电脑执行一次 `account login` 即可。

```sh
remote-cc account login <服务器>-cc                     # 登录这台电脑的账号：在电脑的浏览器里打开链接，把授权码粘贴回终端
remote-cc account login --account work <服务器>-cc      # 登录一个命名账号，几台电脑可以共用
remote-cc --account work <服务器>-cc                    # 用命名账号启动会话（也可以设置环境变量 RCC_ACCOUNT=work）
remote-cc account status <服务器>-cc
remote-cc account list <服务器>-cc                      # 所有账号的登录状态
remote-cc account setup-token --account ci <服务器>-cc  # 生成长期令牌，然后：
remote-cc account token --account ci <服务器>-cc        # 把令牌存进 ci 账号（隐藏输入）；--clear 删除
remote-cc account logout <服务器>-cc
```

- `--account default` 使用容器里共用的 `~/.claude`。
- 每个 Claude 账号在容器里有自己的目录 `~/.local/share/rcc/accounts/<账号>`，里面是凭据、组织策略和对话历史。`settings.json`、`CLAUDE.md`、`keybindings.json`、`skills`、`agents`、`commands`、`plugins`、`output-styles` 链接到 `~/.claude` 里的同名项，几个账号共用（来自 `internal/scripts/lib/account.sh`）。把某个链接换成真实文件，这个账号就单独使用这一项。用户级的 MCP 服务器不共用，每个账号要单独添加。
- 账号分开是为了不混用，不是安全边界：能进这个容器的人都是容器里的 root。不信任的人请部署者给他单独建一个服务器用户。

## 历史档案

Claude Code 按项目路径保存对话历史。项目在容器里挂载在和电脑上相同的路径，所以两台电脑上路径相同的项目会被当成同一个项目。为此每台电脑有自己的历史档案，两台电脑互相看不到对方的对话。

- 默认的历史档案名是 `<主机名>-<随机 8 位十六进制>`，第一次运行时生成，保存在电脑上（位置见下文「电脑上的文件」）。
- `--profile <名字>` 指定历史档案，几台电脑可以共用一份；`--profile none` 使用 Claude 账号自己目录里的历史。
- 用命名账号（`--account`）时，历史档案按账号分开，同一台电脑在不同账号下的历史互不可见。
- 还没有分开的：Claude Code 在 `.claude.json` 里按项目路径记录的状态（是否信任该目录、项目级允许的工具等）仍然按账号共用。

## 设备

你的每台电脑都是这个服务器用户的一台设备，进入的是同一个容器。在已经能用的电脑上就能给自己的另一台电脑发邀请码，不用找部署者：

```sh
remote-cc device invite <服务器>-cc         # 在能用的电脑上：输出一条一次性邀请码，默认 24 小时有效（--ttl，最长 168h）
remote-cc device join <邀请码>              # 在新电脑上：生成密钥、登记、写好 ssh 配置、固定主机指纹
remote-cc device list <服务器>-cc           # 这个服务器用户的所有设备和还没用的邀请码
remote-cc device remove <服务器>-cc <设备>  # 按 list 显示的名字或指纹移除，丢了的电脑就这样撤销
```

- 邀请码只能用一次，到期自动失效；每个服务器用户同时最多 5 条没用的邀请码（来自 `deploy/rcc-self-invite`）。
- 不能移除最后一台设备；移除正在用的这台电脑要加 `--force`。
- 新设备默认用自己的 Claude 账号和历史档案，效果和在新电脑上直接装 Claude Code 一样；想共用就用 `--account` 和 `--profile`。

## 更新客户端

```sh
remote-cc update                   # 换成最新 release（按 SHA256SUMS 校验；Windows 上也能替换正在运行的程序）
remote-cc version --check          # 当前版本和最新版本
remote-cc update --version vX.Y.Z  # 换成指定版本
```

Claude Code 本身装在服务器的镜像里，由部署者执行 `remote-cc server update` 升级。升级时要注意的变化见 [CHANGELOG.md](../CHANGELOG.md)。

## 电脑上的文件

| 内容 | 位置 |
| --- | --- |
| ssh 配置、每台服务器专用的密钥 | `~/.ssh/remote-cc/`，`~/.ssh/config` 开头有一行 `Include remote-cc/config` |
| 这台电脑的历史档案名 | `<用户配置目录>/rcc/profile` |
| 和 RCB 的连接 | `<用户配置目录>/rcc/rcb.json`（权限 0600） |
| 各目录用哪个配置和服务器（`rcb use`） | `<用户配置目录>/rcc/rcb-dirs.json` |
| 会话日志 | `<用户缓存目录>/rcc/last.log`，上一次会话的在同一目录的 `prev.log` |

`<用户配置目录>` 在 Linux 上是 `~/.config`，macOS 上是 `~/Library/Application Support`，Windows 上是 `%APPDATA%`；`<用户缓存目录>` 在 Linux 上是 `~/.cache`，macOS 上是 `~/Library/Caches`，Windows 上是 `%LOCALAPPDATA%`（Go 的 `os.UserConfigDir`、`os.UserCacheDir`）。会话日志记录了 Claude 在电脑上执行的命令和用到的 shell。

<a id="rcb"></a>

## 连接 RCB

连接 [Remote Claude Browser（RCB）](https://github.com/NotDesigned/remote-claude-browser)后，`remote-cc --rcb <配置>` 以该浏览器配置的身份运行 Claude Code：用这个配置的 Claude 账号，全部网络流量经这个配置的节点出去，和网页版是同一个出口和时区。前提是服务器上已经部署了出口主机（部署者执行 `server egress`，见 [deploy/README.md](../deploy/README.md#egress-host)）。

```sh
remote-cc rcb login https://browser.example.com    # 终端显示连接码和链接，你本人在 RCB 里批准这台电脑
remote-cc rcb profiles                             # 你的浏览器配置：Claude 账号、节点、出口、时区
cd ~/proj && remote-cc rcb use                     # 列出你的配置，选一个；这个目录及子目录默认用它和服务器
remote-cc account login                            # 第一次：在该配置的浏览器里一键登录 Claude
remote-cc                                          # 之后在这个目录里直接运行；也可以写 remote-cc --rcb <配置> <服务器>-cc
```

- **批准这台电脑**：`rcb login` 显示连接码和链接，并尝试在电脑的浏览器里打开链接。在 RCB 里核对连接码和终端一致、请求是你自己刚发起的，再批准。只批准自己发起的请求：批准别人发来的链接，等于把你的配置和节点交给对方。批准流程和连接码的有效期见 [RCB：批准流程](https://github.com/NotDesigned/remote-claude-browser/blob/main/docs/remote-cc.md#approval)，工作台里的操作见 [RCB：在工作台里批准电脑](https://github.com/NotDesigned/remote-claude-browser/blob/main/docs/usage.md#remote-cc)。已经有一个客户端令牌时，可以放进环境变量 `RCC_RCB_TOKEN` 再执行 `rcb login`，跳过批准。
- **选配置**：`--rcb` 和 `rcb use` 接受 `rcb profiles` 里的序号（如 `--rcb 2`）、配置的 id、Claude 邮箱或名称，也接受其中任意一段，只要只有一个配置含有它（如 `--rcb sola`）；有几个配置都含有时让你选。`rcb use` 不写配置时列出全部配置让你选。
- **记在目录上**：`rcb use [<配置> [<服务器>-cc]]` 把配置和服务器记在当前目录上；之后在这个目录和子目录里，直接运行 `remote-cc`（或 `remote-cc -- --continue` 等）就以这个配置启动会话，`account login`、`account status` 等也不用写 `--rcb` 和服务器。没有用 `rcb use` 时，只有电脑只加入了一台服务器才能省略服务器。
- **Claude 账号**：每个配置在容器里有固定的 Claude 账号 `rcb-<配置 id>`，凭据跟着配置走，几台电脑共用同一份登录。这些账号只能通过 `--rcb` 使用：`--account rcb-…` 和 `RCC_ACCOUNT` 都选不中它们，`--rcb` 和不同的 `--account` 同时给出时报错。`account status`、`account logout`、`account setup-token` 加 `--rcb` 时同样经节点运行，`account token --rcb <配置>` 保存长期令牌。
- **启动时要能连上 RCB**：每次启动会话，remote-cc 都向 RCB 要一份会话授权（见 [RCB：会话授权](https://github.com/NotDesigned/remote-claude-browser/blob/main/docs/remote-cc.md#grants)）。连不上 RCB 时新会话启动失败，已经在运行的会话不受影响。
- **断开**：在 RCB 的工作台里断开这台电脑（见 [RCB：撤销](https://github.com/NotDesigned/remote-claude-browser/blob/main/docs/remote-cc.md#revocation)）。`remote-cc rcb logout` 只删除电脑上保存的连接，不会让它失效。
- 这台电脑能从 RCB 读到什么、不能读到什么，见 [RCB：令牌种类和权限](https://github.com/NotDesigned/remote-claude-browser/blob/main/docs/remote-cc.md#tokens)；电脑和 RCB 账号的对应关系见 [RCB：对应关系](https://github.com/NotDesigned/remote-claude-browser/blob/main/docs/remote-cc.md#identity)。

### 一键登录 Claude Code

`remote-cc account login --rcb <配置>` 在容器里经该配置的节点运行 Claude Code 的登录。remote-cc 认出登录链接后交给 RCB：

1. RCB 在该配置的浏览器里打开登录链接（浏览器没开时，你在 RCB 里打开这个配置后再打开；见 [RCB：登录横幅](https://github.com/NotDesigned/remote-claude-browser/blob/main/docs/remote-cc.md#login)）。
2. 在那个浏览器里点「授权」。授权码经 RCB 交回 remote-cc，由 remote-cc 输入 Claude Code；你已经在终端里打过字时，改为打印授权码让你粘贴。
3. 登录后核对 Claude Code 的账号邮箱和配置的 Claude 邮箱，不一致时提示重新登录。

手动粘贴始终可用：认不出链接、连不上 RCB、在 RCB 里取消了登录，或者 15 分钟内没有完成（`internal/client/login.go`）时，remote-cc 提示你自己打开链接（最好在该配置的浏览器里），把授权码粘贴回终端。在别的浏览器里授权时只能手动粘贴。

<a id="rcb-checks"></a>

### 连接 RCB 后启动时的检查

会话进入该节点的网络之后、启动 claude 之前，remote-cc 依次检查：

1. 经节点查询出口 IP，必须等于 RCB 记录的出口。RCB 还没有这个配置的出口记录时，先请 RCB 经该节点检查一次，检查失败就拒绝启动。出口和记录不同时请 RCB 重新检查：出口确实变了，就用新的记录和新的会话授权重试一次；RCB 看到的仍是旧出口，说明出口主机到节点的路径和 RCB 不同，拒绝启动。
2. `TZ` 设为 RCB 记录的出口时区；没有记录时用查询到的时区。
3. 配置的浏览器已经登录 Claude 时，Claude Code 的登录邮箱必须相同，否则拒绝启动；用长期令牌登录、没有邮箱时只提示，不比较。

出口 IP 由节点服务器决定，例如节点服务器优先用 IPv6 时，出口就是它的 IPv6 地址，和 RCB 浏览器一致。只核对出口 IP 和时区，不比较浏览器指纹等其他信号。出网的原理见 [docs/architecture.md](architecture.md#egress)。

## 排查

| 现象 | 原因和处理 |
| --- | --- |
| `claude was not found on …` | 连到了宿主机而不是你的容器。用 `server setup` 或 `device join` 写好的别名 `<服务器>-cc`。 |
| `claude is missing inside the remote-cc container` | 容器里的 Claude Code 缺失，请部署者执行 `remote-cc server update`。 |
| `The server's gateway could not reach your container` | 你的容器停了或不存在，请部署者执行 `remote-cc server update`，它会启动停止的容器。 |
| `Claude's commands could not run on this machine` | 电脑上的 shell 不能用。Windows 上安装 Git for Windows，或用 `RCC_SHELL` 指定 `bash.exe` 的路径。 |
| `… is now: remote-cc …` | 用了旧的命令名，按提示换成新名字，见 [CHANGELOG.md](../CHANGELOG.md)。 |
| `--rcb needs this machine linked to remote-claude-browser` | 先执行 `remote-cc rcb login`。 |
| `could not check profile …'s exit` 或 `still sees exit …` | 见上文「连接 RCB 后启动时的检查」。 |
| 刚在电脑上改的文件，Claude 读到的还是旧内容 | 容器里对目录列表有几秒的缓存（见 [docs/architecture.md](architecture.md#文件通道)），稍等再试。 |
| 其他问题 | 加 `-v`（或 `-vv` 等）重新运行，并查看会话日志 `last.log`。 |
