# 服务端部署（Docker）

一台服务器上跑一个**网关**和**每个用户一个容器**。整台服务器只对外开一个端口（默认 2222）：

```
用户的机器 ──ssh──► 网关（唯一对外端口，标准 OpenSSH） ──只放行用户自己的容器──► rcc-<用户>
```

用户容器里有 Claude Code、sshd、sshfs。Claude 在项目之外的文件操作只影响这个容器，不影响宿主机，也不影响别的用户。

## 部署

宿主机需要 `/dev/fuse`。Docker 可以事先装好（含 compose 插件），也可以加 `--install-docker` 让 remote-cc 自动安装。在**自己的电脑**上执行：

```sh
remote-cc server setup root@新服务器
remote-cc server setup ubuntu@新服务器                   # 不能用 root 登录、只有一个带 sudo 的账号时
remote-cc server setup --install-docker ubuntu@新服务器  # 全新的机器，还没装 Docker
```

`server setup` 会安装网关，把你创建为第一个用户（默认名 `owner`，可以用 `--user` 指定），并配置好本机：
- 在 `~/.ssh/remote-cc/` 下生成这台服务器专用的密钥，写好 `~/.ssh/remote-cc/config`，并在 `~/.ssh/config` 开头加一行 `Include remote-cc/config`。
- 别名默认是 `<主机名第一段>-cc`（不会覆盖你已有的、指向宿主机的别名），可以用 `--alias` 指定。
- 主机指纹通过已经验证过的 ssh 连接取得并写进 `known_hosts`，第一次连接不需要盲目信任。
- 在本机测试网关端口能否连通。不通（通常是云服务商的安全组没放行）时，自动改用经宿主机 ssh 跳转（ProxyJump）的配置，并提示你放行端口。
- 最后实际连一次，确认配置可用。

**只有 sudo 账号时**：登录用户不是 root 时，remote-cc 会自动用 sudo 提权。免密 sudo 不需要任何操作；需要密码时，remote-cc 会在本机以不回显的方式询问，再通过 stdin 交给 `sudo -S`，密码不会出现在命令行或日志里。日常连接用的是网关和容器自己的 sshd，和宿主机能不能用 root 登录无关。

常用参数：

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `--user NAME` | owner | 你自己的用户名 |
| `--alias NAME` | `<主机名第一段>-cc` | 本机 ssh 配置里的别名 |
| `--port N` | 2222 | 网关对外的端口（宿主机原有的 22 端口不受影响） |
| `--bind ADDR` | 所有地址（IPv4 + IPv6） | 改成 `127.0.0.1` 后不对外开放，只能经宿主机跳转 |
| `--data DIR` | /var/lib/remote-cc | 持久数据：每个用户的 Claude 登录、对话历史、主机密钥，以及公钥 |
| `--key FILE.pub` | | 额外授权的公钥，可以重复指定 |
| `--image IMG` | ghcr.io/notdesigned/remote-cc-server:latest | 服务端镜像 |
| `--public-host ADDR` | ssh 解析出的地址 | 用户实际连接用的地址（在服务器本机通过 localhost 操作时要指定） |
| `--name NAME` | remote-cc | 实例名（网关容器 `<名字>-gateway`、网络 `<名字>`）；同一台机器可以按不同名字、端口、目录跑多个实例 |
| `--install-docker` | 关 | 没有 Docker 时，用官方脚本 get.docker.com 安装 |

## 用户和设备

**管理员**：

```sh
remote-cc server user add root@服务器 alice               # 建 alice 的容器，输出一条一次性邀请码，私下发给她
remote-cc server user invite root@服务器 alice            # 再生成一条邀请码（默认 7 天有效，--ttl 可改）
remote-cc server user list root@服务器                    # 每个用户的容器状态、设备数、活跃会话、数据大小
remote-cc server user config root@服务器 alice            # 打印手写 ssh 配置所需的内容
remote-cc server user remove [--purge] root@服务器 alice  # 立即撤销访问；--purge 连数据一起删除
```

**用户**：

```sh
remote-cc device join <邀请码>     # 生成本机的密钥、登记、写好 ssh 配置、固定主机指纹
remote-cc account login 服务器-cc  # 登录 Claude（每台机器一次）
remote-cc 服务器-cc                # 开始使用
```

**自己加设备，不用找管理员**：在已经能用的机器上生成邀请码，到新机器上 `device join`：

```sh
remote-cc device invite 服务器-cc              # 一次性邀请码，默认 24 小时有效（--ttl，最长 168h）
remote-cc device list 服务器-cc                # 这个用户的所有设备和未使用的邀请码
remote-cc device remove 服务器-cc 设备         # 按 list 显示的名字或指纹移除（丢了的电脑就这样撤销）
```

新设备进入的是**同一个容器**（同一个用户），但默认使用自己的 Claude 登录和自己的对话历史，和直接在新电脑上装 Claude Code 的效果一样；想共用就用 `--account 名字` / `--profile 名字`（见主 README）。

## 更新、移除、迁移

```sh
remote-cc server update root@服务器  # 拉取最新镜像（包含新版 Claude Code），只重建有变化的容器；
                                     # 有会话正在进行时会拒绝，除非加 --force
remote-cc server update --all        # 本机管理过的所有服务器依次更新；某台失败不影响其他，最后汇总
remote-cc server list                # 列出这些服务器；--forget root@服务器 删掉已经不存在的
remote-cc server remove root@服务器  # 删除网关和所有用户容器；数据默认保留，--purge 一起删除
remote-cc server init root@服务器    # 只安装网关，不创建用户
```

本机会在 `~/.ssh/remote-cc/servers.json` 里记住你用 `server setup`、`server init`、`server migrate`、`server update` 操作过的服务器（ssh 目标、命令行上的 ssh 参数、安装目录），`server remove` 时删除。0.5.2 之前部署的服务器，对每台执行一次 `server update 主机` 就会记上。

**从 0.4 及更早的单容器部署迁移**：0.5 不再支持单容器布局，`server update` 等命令遇到它会提示先迁移：

```sh
remote-cc server migrate --user owner root@服务器
```

迁移把原来的容器变成用户 `owner` 的容器：数据、对话历史、登录、已授权的公钥都保留，端口不变，原来的主机密钥移给网关，所以旧的 `known_hosts` 记录依然有效。迁移前会做备份（`<data 的上级目录>/remote-cc-before-multi-user-<时间>.tgz`），有活跃会话时拒绝执行，中途失败自动回滚。迁移后用 `remote-cc server user invite` 给自己发一条邀请码再 `device join`，或者按它打印的内容修改 ssh 配置。

**换服务器**：在新服务器上 `server setup`，再给其他人发邀请码。要带走数据，把旧服务器的 `/var/lib/remote-cc` 复制到新服务器的同一位置后再执行 `server setup`。

## 安全设计

- **隔离**：每个用户有自己的容器，也就有自己的 root、Claude 登录和对话历史。
  网关是标准的 OpenSSH：每个用户对应一个账号，`PermitOpen` 只放行 `rcc-<用户>:22`；不能分配 TTY，也不能做其他转发。网关账号唯一能执行的程序是 `rcc-gateway-cmd`，它只提供两件事：读取网关的主机公钥，以及为**这个账号自己**登记设备邀请码。
- **公钥**：每个用户的公钥存在宿主机的 `<data>/keys/<用户>/authorized_keys`。网关只读地挂载它用于认证；用户容器挂载自己那一份，用于端到端认证（所以即使有人绕过了网关的路由，别人的容器也不接受他的密钥），`device remove` 也是改这一份。authorized_keys 里的选项只能进一步收紧限制，不能放宽网关的 `PermitOpen`。
- **邀请码**：内含一把一次性密钥，分成两半：
  - 网关这一半在 `<data>/invites/<用户>`，用户容器看不到，只允许跳转到 `rcc-<用户>:22`。管理员的 `server user invite` 直接写入；用户的 `device invite` 由网关上的 `rcc-self-invite` 写入，它只接受调用者自己的账号、只接受一把 ed25519 公钥、有效期 1 到 168 小时，每个用户最多 5 条未过期的邀请。
  - 容器这一半只能执行 `rcc-enroll`，没有任何转发权限。
  - 两半都带 `expiry-time`，到期自动失效。`rcc-enroll` 只接受一行不带任何选项的公钥，登记成功后删除这把一次性密钥，所以邀请码只能用一次。
- **容器权限**：sshfs 需要 `/dev/fuse`、`CAP_SYS_ADMIN`，并且要关掉 Docker 默认的 AppArmor 限制（默认规则禁止 mount）。容器没有用 `--privileged`，也没有挂载宿主机的其他目录，只挂了自己的数据目录。
- **sshd 配置**：只允许密钥登录。用户容器只放行 rcc 需要的 unix socket 反向转发；TCP 正向和反向转发、X11、agent 转发、隧道都被禁止。
- **公网端口**：Docker 发布的端口不经过 ufw，容器一启动就会暴露在公网上。如果云服务商有安全组，要在那里放行这个端口。
- **Claude Code 版本**：固定在镜像里，自动更新已关闭。升级 Claude Code 就执行 `server update`。
- **测试**：CI 每次提交都会在 GitHub 的 runner 上部署一台真实的服务器，跑完整流程（`test/integration.sh`）：部署、邀请和加入、一次性邀请码不能复用、跨用户访问被拒绝、网关账号不能执行其他命令、自助加设备和移除设备、一次真实会话（文件经挂载读写、命令在本机执行、退出码传回）、从单容器部署迁移（数据和主机密钥保留）、更新和移除。

## 构建镜像

镜像由 GitHub Actions 在推送 `v*` 标签时构建，包括 amd64 和 arm64 两个版本（`.github/workflows/image.yml`）。也可以在 Actions 页面手动运行，指定要打包的 Claude Code 版本。
本地构建：`docker build deploy`。注意会临时占用约 2GB 的构建缓存，磁盘紧张的机器不要在本地构建。

## 经 remote-claude-browser 的节点出网（可选）

[remote-claude-browser](https://github.com/NotDesigned/remote-claude-browser)（下称 RCB）的每个浏览器配置绑定一个 Claude 账号和一个代理节点。部署**出口主机**后，`remote-cc --rcb 配置` 启动的 Claude Code 使用该配置对应的 Claude 账号，并且**全部网络流量**都经该配置的节点出去，和网页版是同一个出口和时区。RCB 可以和 remote-cc 装在同一台机器上，也可以分开部署，两边只通过 HTTPS 接口联系。RCB 一侧的说明见它的 `docs/remote-cc.md`。

```
RCB 工作台：配置（Claude 账号、节点、出口、时区）、签发会话授权
  ▲ 主机令牌：兑换会话授权、复查节点        ▲ 客户端令牌：读配置、要会话授权、一键登录
  │                                          │
rcc-egress 容器 ◄── 每个用户自己的 socket ── rcc-<用户> 容器 ◄──ssh── 你的电脑
  每个（用户, RCB 用户, 节点）一个网络命名空间   claude 进入命名空间后启动       remote-cc --rcb 配置
```

### 部署

```sh
remote-cc server egress --rcb https://browser.example.com root@服务器  # 显示连接码和链接，由 RCB 管理员批准
remote-cc server update root@服务器                                    # 有会话没挂上时，等会话结束后再执行
```

在**你的电脑**上：

```sh
remote-cc rcb login https://browser.example.com  # 显示连接码和链接，你本人在 RCB 里批准这台电脑
remote-cc rcb profiles                           # 你的配置：Claude 账号、节点、出口、时区
cd 项目 && remote-cc rcb use 配置                 # 记住这个目录用哪个配置、哪台服务器
remote-cc account login --rcb 配置               # 第一次：在该配置的浏览器里一键登录 Claude
remote-cc                                         # 之后在这个目录（及子目录）直接运行
```

**批准连接**：`rcb login` 和 `server egress` 都向 RCB 发起连接请求，终端显示连接码（如 `ABCD-EFGH`）和链接，`rcb login` 还会尝试在本机浏览器打开链接。打开链接（或在 RCB 的「代理与 API → remote-cc」输入连接码），核对连接码与终端一致、来源 IP 和时间是自己刚才的请求，再点「批准」。

- 电脑由**用户本人**批准，令牌属于批准的人：批准别人发来的链接，等于把自己的配置和节点交给对方。
- 出口主机只能由 RCB 的**管理员**批准；主机令牌也只在批准它的管理员仍是启用的管理员时有效。
- 连接码 10 分钟内有效。批准后令牌只交给发起请求的机器一次：客户端令牌保存在本机的用户配置目录（`rcc/rcb.json`，0600），主机令牌保存在服务器的 `<安装目录>/egress/token`（0600），经 stdin 传入，不出现在命令行里。
- 再次运行 `server egress` 会重新请求批准，得到一个新的主机令牌；旧的在 RCB 的「出口主机」列表里断开。

**配置怎么选**：`--rcb` 接受配置的 id、Claude 邮箱或名称，名称重复时会让你选。`rcb use 配置 [服务器]` 把配置和服务器记在这个目录上（`rcc/rcb-dirs.json`），之后在这个目录和子目录里：

- 直接运行 `remote-cc`（或 `remote-cc -- --continue` 等）就以这个配置启动会话；
- `remote-cc account login --rcb 配置`、`account status --rcb 配置` 等可以省略服务器。

没有用 `rcb use` 时，省略服务器只在本机只加入了一台 remote-cc 服务器时可行，否则要写出服务器。

### 一键登录 Claude Code

`remote-cc account login --rcb 配置` 在服务器上、经该配置的节点运行 `claude auth login`。remote-cc 从 Claude Code 的输出里认出登录链接，交给 RCB：

1. 该配置的浏览器正开着时，RCB 在其中新开标签页打开登录链接；没开时，你在 RCB 里打开这个配置后自动打开。
2. 在那个浏览器里点「授权」。RCB 取得授权码交还 remote-cc，remote-cc 把它输入 Claude Code；你已经在终端里打过字时，改为打印授权码让你粘贴。
3. 登录后核对 Claude Code 的账号邮箱与配置的 Claude 邮箱，不一致时提示重新登录。

等待中的登录在 RCB 的「等待完成的 Claude Code 登录」里，可以取消。**手动粘贴**始终可用：认不出链接、RCB 连不上、登录被取消，或 15 分钟没完成时，remote-cc 会提示你自己打开链接（最好在该配置的浏览器里），把授权码粘贴回终端。

配置的账号固定为 `rcb-<配置 id>`，凭据跟配置走，几台电脑共用同一份登录。这些账号只能通过 `--rcb` 使用：`--account rcb-…` 和 `RCC_ACCOUNT` 都不会选中它们，`--rcb` 与不同的 `--account` 同时给出时报错；`account list` 也不会在节点之外查询它们。`account status`、`account logout`、`account setup-token` 加 `--rcb` 时同样经节点运行；`account token --rcb 配置` 保存长期令牌。

### 会话授权和出口主机

出口主机手里没有节点列表。每次启动会话，remote-cc 先用客户端令牌向 RCB 要一份**会话授权**（RCB 签名，10 分钟有效，说明「这个用户可以经节点 N 运行配置 P」），经用户容器交给 `rcc-egress`；`rcc-egress` 用主机令牌向 RCB 兑换，RCB 确认这个用户仍然启用、仍然有绑定该节点的配置后，返回节点的完整配置（含凭据）和 RCB 用户 id。

- **启动会话要能连上 RCB**（要授权、兑换授权）。连不上时新会话启动失败。
- **运行中的会话不依赖 RCB**：`rcc-egress` 每 5 分钟只为正在运行的节点、按 RCB 用户逐个复查。用户被停用、配置不再绑定这个节点、节点被删除时，停止这个用户的这个节点；节点配置变了也停止，下次启动用新配置；RCB 连不上或回答不明确时保持运行。被停止的节点上，正在运行的会话断网，需要重新启动。
- 主机令牌被撤销（或批准它的管理员被停用、降级）时，连续两轮复查（最多约 10 分钟）被拒绝后，`rcc-egress` 停止所有节点，需要重新运行 `server egress` 并批准。
- 网络命名空间按「remote-cc 用户（socket 所在的目录）+ RCB 用户 + 节点」分开，每个都有自己的 Mihomo：不同用户即使用同一个节点，也不共用连接和 DNS 缓存，撤销一个用户只影响他自己。整台主机最多 64 个、每个 remote-cc 用户最多 8 个：用户超出时停止他最久没用的那个，主机超出时拒绝新的。要调整，在宿主机的 `<安装目录>/egress/.env` 里加 `RCC_EGRESS_MAX_NODES=…`、`RCC_EGRESS_MAX_NODES_PER_USER=…`，再在该目录运行 `docker compose up -d`（会断开正在用节点的会话）；之后的 `server egress`、`server update` 都会保留这两行。
- 每个 remote-cc 用户的容器只挂载自己的 `<数据目录>/egress/users/<用户>`，里面是 `rcc-egress` 为他创建的 socket；socket 先在只有 `rcc-egress` 能写的目录里建好，再移进去，用户放进去的符号链接不会被跟随。

**原理**：每个网络命名空间里唯一的默认路由是连到该节点的 Mihomo TUN。只有 Mihomo 自己的连接（带路由标记）能用通往外网的 veth，防火墙拒绝其他一切；Mihomo 停止时命名空间没有任何出口，不会退回直连。DNS 由 TUN 接管，经节点解析；IPv6 只允许 Mihomo 自己使用。会话开始时，`rcc-shim` 从该用户自己的 socket 取得命名空间（文件描述符），进入后才启动 claude。所以不依赖 `HTTPS_PROXY`：claude、它启动的 MCP 服务器、WebFetch、遥测都只能经节点出网。Bash 工具的命令和文件照常在你的电脑上执行；Claude 的登录凭据（`CLAUDE_CODE_OAUTH_TOKEN`、`ANTHROPIC_API_KEY` 等）不会随命令传到你的电脑上。

### 启动时的检查

进入命名空间后、启动 claude 之前：

1. 经节点查询出口 IP，必须等于 RCB 记录的出口；
2. `TZ` 设为 RCB 记录的时区（没有记录时用查询到的）；
3. 配置的浏览器已经登录 Claude 时，Claude Code 的登录邮箱必须相同；没有登录时拒绝，用 setup-token 登录、没有邮箱时只提示不比较。

RCB 还没有这个配置的出口记录时，remote-cc 先请 RCB 经该节点检查一次，检查失败就拒绝启动。出口与记录不同时，remote-cc 请 RCB 重新检查：出口确实变了，就用新的记录和新的授权重试一次；RCB 看到的仍是旧出口，说明出口主机到节点的路径和 RCB 不同，拒绝启动。出口 IP 由节点服务器决定，例如节点服务器优先用 IPv6 时，出口就是它的 IPv6 地址，与 RCB 浏览器一致。

### 两种令牌能读到什么

| 令牌 | 在哪里 | 能做什么 | 不能做什么 |
| --- | --- | --- | --- |
| 客户端 `rcck_` | 你的电脑 | 读你的配置：名称、Claude 邮箱、节点名、出口 IP、时区；为你的配置要会话授权、请 RCB 重新检查出口；在你配置的浏览器里打开 Claude Code 登录链接并取回授权码 | 读不到 Cookie、浏览器数据、节点配置和凭据；不能让浏览器打开登录链接以外的网址 |
| 主机 `rcch_` | 服务器的 `<安装目录>/egress/token` | 兑换会话授权，得到该会话节点的完整配置（含凭据）；复查自己兑换过的节点 | 没有会话授权就拿不到任何节点，不能列出节点或配置 |

### 撤销

- 在 RCB 的「代理与 API → remote-cc」里「断开」一台电脑或出口主机，立即生效。本机的 `remote-cc rcb logout` 只删除本机保存的令牌，不会让它失效。
- RCB 管理员修改用户的密码或角色、停用或删除用户时，这个用户的所有令牌一起删除，电脑和出口主机都要重新批准。
- 断开电脑后它不能再启动会话，但已经在运行的会话不会中断；要结束它们，停用该用户或改掉配置的节点，出口主机在下一轮复查（最多 5 分钟）时停止。
- 断开出口主机的效果见上文：最多约 10 分钟后所有节点停止。

### 更新

`server update` 同时更新出口主机：保留它的令牌、RCB 地址和设置，拉取新镜像，只有镜像或 compose 文件变化时才重建，输出 `egress host updated`。重建出口主机会让所有 `--rcb` 会话断网，所以有会话进行时同样拒绝执行，除非加 `--force`。`server update` 还会检查每个用户容器的 socket 挂载，缺少或指向别处时修正 `.env` 并重建这个容器（即使没有新镜像）。`server remove` 连同出口主机一起删除（主机令牌随安装目录删除，记得在 RCB 里断开它）。

再次运行 `server egress` 会重新请求批准，换上新的主机令牌并重建出口主机（换 RCB 地址或令牌时用它；只更新镜像用 `server update`）。它还会给没有挂上 socket 的用户容器补上挂载；有会话在进行的用户会列出来，等会话结束后执行 `server update`。

### 限制

- 出口主机（也就是这台服务器的管理员）能拿到在这里运行过会话的 RCB 用户的节点配置和凭据；`rcb-*` 账号的 Claude 凭据也保存在服务器上的用户容器里。只在你信任的服务器上使用。
- 会话授权不绑定某一台出口主机：任何已批准的出口主机都能在 10 分钟内兑换它。
- 只核对出口 IP，时区按记录设置，不比较浏览器指纹等其他信号；RCB 记录的出口来自最近一次检查。
- 会话结束后命名空间和 Mihomo 继续运行，直到复查让它停止、腾位置时被停止，或出口主机重启。
- 一键登录要求在该配置的浏览器里完成授权；在别的浏览器里授权时只能手动粘贴授权码。
- 宿主机需要 `/dev/net/tun`；容器使用 `NET_ADMIN`、`SYS_ADMIN`，不使用 `--privileged`。命名空间、防火墙和 Mihomo 的实际行为只在真实部署里验证，单元测试使用替身。
