# 部署服务器

> 读者：部署者，也就是有服务器 root 或 sudo 权限、管理服务器用户的人

一台服务器上运行一个网关和每个服务器用户一个容器。整台服务器只对外开一个端口（默认 2222，来自 `deploy/compose.gateway.yml`）：

```
用户的电脑 ──ssh──► 网关（唯一的对外端口，标准 OpenSSH） ──只放行这个用户自己的容器──► rcc-<用户名>
```

容器里有 Claude Code、sshd、sshfs 和 git。Claude 在项目目录以外的文件操作只影响这个容器，不影响宿主机，也不影响别的服务器用户。隔离和权限的设计见 [docs/architecture.md](../docs/architecture.md#安全模型)。

下面的命令都在**你自己的电脑**上执行，用 ssh 管理宿主机（命令里写作 `SERVER`，例如 `root@<服务器>`）。每条命令的全部选项见 `remote-cc help <命令>`。

## 部署

宿主机需要 `/dev/fuse`。Docker（含 compose 插件）可以事先装好，也可以加 `--install-docker` 让 remote-cc 用 get.docker.com 的官方脚本安装。

```sh
remote-cc server setup root@<服务器>                     # 能用 root 登录时
remote-cc server setup ubuntu@<服务器>                   # 只有一个带 sudo 的账号时
remote-cc server setup --install-docker ubuntu@<服务器>  # 全新的机器，还没装 Docker
```

`server setup` 安装网关，把你创建为第一个服务器用户（默认名 `owner`，`--user` 可改），并配置好你的电脑：

- 在 `~/.ssh/remote-cc/` 下生成这台服务器专用的密钥，写好 `~/.ssh/remote-cc/config`，并在 `~/.ssh/config` 开头加一行 `Include remote-cc/config`。
- 容器的别名默认是主机名的第一段加 `-cc`（下文写作 `<服务器>-cc`），不会覆盖你已有的、指向宿主机的别名；`--alias` 可改。
- 主机指纹经已经验证过的 ssh 连接取得，写进 `known_hosts`，第一次连接不需要盲目信任。
- 在你的电脑上测试网关端口能否连通。不通时（通常是云服务商的安全组没放行）自动改用经宿主机跳转（ProxyJump）的配置，并提示你放行端口。
- 最后实际连一次，确认配置可用。

常用的调整：`--port` 改网关端口（宿主机原有的 22 端口不受影响），`--bind 127.0.0.1` 让网关不对外开放、只能经宿主机跳转，`--data` 和 `--install-dir` 改目录（见下文「目录」），`--name` 在同一台宿主机上按不同名字、端口和目录运行多个实例，`--public-host` 指定用户实际连接的地址（在宿主机上经 localhost 操作时需要）。只装网关、不创建服务器用户，用 `remote-cc server init`。

- **只有 sudo 账号时**：登录账号不是 root 时，remote-cc 自动用 sudo 提权。免密 sudo 不需要任何操作；需要密码时，remote-cc 在你的电脑上以不回显的方式询问，经 stdin 传到宿主机、再经 `SUDO_ASKPASS` 交给 sudo，密码不会出现在命令行或日志里；密码错误时什么都不执行。日常连接用的是网关和容器自己的 sshd，和宿主机能不能用 root 登录无关。
- **公网端口**：Docker 发布的端口不经过 ufw，网关一启动就暴露在公网上。云服务商有安全组时，要在那里放行这个端口。

<a id="user-names"></a>

## 用户名

服务器用户名只能用小写字母、数字和 `-`，以小写字母开头，最长 31 个字符（`internal/sshconf/sshconf.go` 的 `userRe`，网关的 `deploy/rcc-gateway-sync` 用同一条规则）。

- 每个服务器用户对应容器 `rcc-<用户名>`、网关上的同名账号，以及 `<数据目录>/users/<用户名>` 等目录（见下文「目录」）。
- 宿主机上已经有一个叫 `rcc-<用户名>` 的容器、而它不属于这个 remote-cc 实例时，remote-cc 拒绝创建这个用户，也不会改动那个容器。换一个名字即可。
- 服务器用户名和 RCB 的用户名没有固定关系：每台电脑各自连接 RCB，见 [RCB：电脑和 RCB 账号的对应关系](https://github.com/NotDesigned/remote-claude-browser/blob/main/docs/remote-cc.md#identity)。

## 用户

```sh
remote-cc server user add root@<服务器> <用户名>             # 建容器，输出一条一次性邀请码，私下发给对方
remote-cc server user invite root@<服务器> <用户名>          # 再生成一条邀请码（默认 7 天有效，--ttl 可改，最长 30 天）
remote-cc server user list root@<服务器>                     # 每个服务器用户的容器状态、设备数、活跃会话、数据大小
remote-cc server user config root@<服务器> <用户名>          # 打印手写 ssh 配置所需的内容
remote-cc server user rebuild root@<服务器> <用户名>         # 重建这个用户的容器（数据、登录、密钥都保留）；有会话时加 --force
remote-cc server user remove root@<服务器> <用户名>          # 立即撤销访问，数据保留
remote-cc server user remove --purge root@<服务器> <用户名>  # 连数据一起删除
```

- 对方收到邀请码后执行 `remote-cc device join <邀请码>`，一条命令完成密钥、ssh 配置和主机指纹。之后他可以自己给其他电脑发邀请码，见 [docs/usage.md](../docs/usage.md#设备)。
- 已经有对方的公钥时，`server user add --key <公钥文件>` 直接授权，不发邀请码。
- 邀请码的有效期默认 7 天（`internal/cli/server.go`），至少 1 小时。
- 移除时数据默认保留，之后用同一个名字 `server user add` 会接着用这份数据。

## 更新

```sh
remote-cc server update root@<服务器>         # 拉取最新镜像（包含新版 Claude Code），只重建有变化的容器
remote-cc server update --all                 # 依次更新你的电脑管理过的所有服务器，最后汇总
remote-cc server list                         # 你的电脑管理过的服务器
remote-cc server list --forget root@<服务器>  # 从列表里删掉已经不存在的服务器
```

- 有会话正在进行、更新会断开它时，`server update` 拒绝执行（退出码 5），除非加 `--force`。
- `server update` 会启动停止的容器，检查并修正每个容器到出口主机的挂载，部署了出口主机时也一起更新它（见下文「出口主机」）。
- 你的电脑在 `~/.ssh/remote-cc/servers.json` 里记住 `server setup`、`server init`、`server migrate`、`server update` 操作过的服务器（ssh 目标、命令行上的 ssh 参数、安装目录），`server remove` 时删除。
- 镜像里的 Claude Code 版本固定，自动更新已关闭。升级 Claude Code 就执行 `server update`。

## 移除

```sh
remote-cc server remove root@<服务器>          # 删除网关、所有容器和出口主机，数据保留
remote-cc server remove --purge root@<服务器>  # 连数据一起删除
```

出口主机的令牌随安装目录一起删除；记得在 RCB 里断开这台出口主机。你的电脑上指向这台服务器的 `HOST`（`~/.ssh/remote-cc/config` 里的条目）和它们在 `known_hosts` 里的主机指纹也一并删除。

## 迁移

早期版本的服务器是单容器部署，现在的 remote-cc 不再支持它；`server update` 等命令遇到它会提示先迁移（版本见 [CHANGELOG.md](../CHANGELOG.md)）：

```sh
remote-cc server migrate --user owner root@<服务器>
```

- 原来的容器变成服务器用户 `owner` 的容器：数据、对话历史、登录、已授权的公钥都保留，端口不变。原来的主机密钥交给网关，所以用户电脑上旧的 `known_hosts` 记录依然有效。
- 迁移前先备份到 `<数据目录的上级目录>/remote-cc-before-multi-user-<时间>.tgz`；有活跃会话时拒绝执行；中途失败自动回滚。
- 迁移后用 `remote-cc server user invite` 给自己发一条邀请码再 `device join`，或者按它打印的内容修改 ssh 配置。

**换服务器**：在新服务器上 `server setup`，再给其他人发邀请码。要带走数据，先把旧服务器的 `<数据目录>` 复制到新服务器的同一位置，再执行 `server setup`。

## 目录

| 目录 | 默认 | 内容 |
| --- | --- | --- |
| `<安装目录>` | `/opt/remote-cc`（`internal/server/server.go`） | 网关的 compose 文件和 `.env`；`users/<用户名>/` 下每个容器的 compose 文件和 `.env`；`egress/` 下出口主机的 compose 文件、`.env` 和主机令牌 `token`（0600） |
| `<数据目录>` | `/var/lib/remote-cc`（同上） | 见下表 |

`<数据目录>` 里：

| 路径 | 内容 |
| --- | --- |
| `gateway/ssh/` | 网关的主机密钥 |
| `gateway/etc/users` | 服务器用户名单 |
| `keys/<用户名>/authorized_keys` | 这个服务器用户的公钥（网关只读，容器可写） |
| `invites/<用户名>` | 邀请码在网关这一半的一次性公钥 |
| `users/<用户名>/root/` | 容器的 `/root`：Claude 账号、历史档案、remote-cc 在容器里的状态 |
| `users/<用户名>/ssh/` | 容器的主机密钥，重建容器后指纹不变 |
| `egress/users/<用户名>/` | 出口主机给这个服务器用户的 socket |
| `egress/state/` | 出口主机的状态 |

## 备份

要备份的是 `<数据目录>`：全部服务器用户的公钥、Claude 账号、历史档案和主机密钥都在里面。`<安装目录>` 里的 compose 文件和 `.env` 由 remote-cc 生成，出口主机的令牌可以重新批准；镜像随时可以重新拉取。

恢复时按「换服务器」的做法：先把 `<数据目录>` 放回原位置，再用你原来的服务器用户名执行 `server setup --user <用户名>`，然后对其他服务器用户各执行一次 `server user add`，它们会接着用原来的数据和公钥（顺带输出的邀请码不用发出去）。

<a id="egress-host"></a>

## 出口主机

部署出口主机后，服务器用户可以用 `remote-cc --rcb <配置>` 以 [RCB](https://github.com/NotDesigned/remote-claude-browser) 浏览器配置的身份运行 Claude Code（使用者一侧见 [docs/usage.md](../docs/usage.md#rcb)）。出口主机是宿主机上的 `rcc-egress` 容器（容器名 `<实例名>-egress`，默认 `remote-cc-egress`），为每个会话把该配置的节点放进单独的网络命名空间，原理见 [docs/architecture.md](../docs/architecture.md#egress)。RCB 可以和 remote-cc 装在同一台机器上，也可以分开，两边只通过 HTTPS 联系。

宿主机需要 `/dev/net/tun`；出口主机的容器使用 `NET_ADMIN`、`SYS_ADMIN`，不使用 `--privileged`。

```sh
remote-cc server egress --rcb https://browser.example.com root@<服务器>  # 显示连接码和链接，由 RCB 管理员批准
remote-cc server update root@<服务器>                                    # 有容器还没挂上 socket 时，等会话结束后执行
```

- **批准**：出口主机只能由 RCB 的管理员批准，见 [RCB：批准出口主机](https://github.com/NotDesigned/remote-claude-browser/blob/main/docs/admin.md#egress-hosts)。批准后主机令牌保存在 `<安装目录>/egress/token`（0600），经 stdin 传入，不出现在命令行里。已经有一个主机令牌时，可以放进环境变量 `RCC_EGRESS_TOKEN` 再执行 `server egress`，跳过批准。令牌种类和撤销见 [RCB：令牌种类和权限](https://github.com/NotDesigned/remote-claude-browser/blob/main/docs/remote-cc.md#tokens)。
- **重新批准、换 RCB 地址**：再次运行 `server egress`。它重新请求批准，换上新的主机令牌并重建出口主机，旧令牌在 RCB 里断开。它还会给还没挂上 socket 的容器补上挂载；有会话在进行的服务器用户会被列出来，等会话结束后执行 `server update`。
- **更新**：`server update` 同时更新出口主机，保留它的令牌、RCB 地址和节点上限，只有镜像或 compose 文件变化时才重建，并输出 `egress host updated`。重建会让所有 `--rcb` 会话断网，所以有会话进行时同样拒绝执行，除非加 `--force`。
- **节点上限**：每个（服务器用户, RCB 用户, 节点）一个网络命名空间。整台主机最多 64 个，每个服务器用户最多 8 个（`deploy/compose.egress.yml`，`cmd/rcc-egress/main.go`）。服务器用户超出时停止他最久没用的那个，整台主机超出时拒绝新的。要调整，在 `<安装目录>/egress/.env` 里加 `RCC_EGRESS_MAX_NODES=…` 和 `RCC_EGRESS_MAX_NODES_PER_USER=…`，再在该目录执行 `docker compose up -d`（会断开正在用节点的会话）；之后的 `server egress`、`server update` 都会保留这两行。
- **信任**：出口主机，也就是这台服务器的部署者，能拿到在这里运行过会话的 RCB 用户的节点配置和凭据；`rcb-*` Claude 账号的凭据也保存在服务器用户的容器里。只在使用者信任的服务器上部署出口主机。

## 排查

| 现象 | 原因和处理 |
| --- | --- |
| `/dev/fuse is missing on this host` | 宿主机没有 FUSE。加载 `fuse` 内核模块，或换一台支持 FUSE 的机器。 |
| `server setup` 提示网关端口不通 | 在云服务商的安全组里放行网关端口；在放行之前，用户的电脑经宿主机跳转连接。 |
| `holds a single-container deployment` | 早期版本的单容器部署，按上文「迁移」执行 `server migrate`。 |
| `a container named rcc-<用户名> already exists` | 同名容器属于别的实例或不是 remote-cc 创建的，换一个用户名（见「用户名」）。 |
| `active sessions would be disconnected` | 有会话正在进行。等会话结束再执行，或加 `--force` 断开它们。 |
| 用户报告 `The server's gateway could not reach your container` | 这个用户的容器停了。执行 `remote-cc server update root@<服务器>`。 |
| `remote-claude-browser rejected this egress host` | 主机令牌被撤销，或批准它的管理员不再是启用的管理员。重新执行 `server egress` 并批准。 |
