# 服务端部署（Docker）

一台服务器上跑一个**网关**和**每个用户一个容器**。整台服务器只对外开一个端口（默认 2222）：

```
用户的机器 ──ssh──► 网关（唯一对外端口，标准 OpenSSH） ──只放行用户自己的容器──► rcc-<用户>
```

用户容器里有 Claude Code、sshd、sshfs。Claude 在项目之外的文件操作只影响这个容器，不影响宿主机，也不影响别的用户。

## 部署

宿主机需要 `/dev/fuse`。Docker 可以事先装好（含 compose 插件），也可以加 `--install-docker` 让 remote-cc 自动安装。在**自己的电脑**上执行：

```sh
remote-cc setup-server root@新服务器
remote-cc setup-server ubuntu@新服务器                    # 不能用 root 登录、只有一个带 sudo 的账号时
remote-cc setup-server --install-docker ubuntu@新服务器   # 全新的机器，还没装 Docker
```

`setup-server` 会安装网关，把你创建为第一个用户（默认名 `owner`，可以用 `--user` 指定），并配置好本机：
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
remote-cc user add root@服务器 alice               # 建 alice 的容器，输出一条一次性邀请码，私下发给她
remote-cc user invite root@服务器 alice            # 再生成一条邀请码（默认 7 天有效，--ttl 可改）
remote-cc user list root@服务器                    # 每个用户的容器状态、设备数、活跃会话、数据大小
remote-cc user config root@服务器 alice            # 打印手写 ssh 配置所需的内容
remote-cc user remove [--purge] root@服务器 alice  # 立即撤销访问；--purge 连数据一起删除
```

**用户**：

```sh
remote-cc join <邀请码>    # 生成本机的密钥、登记、写好 ssh 配置、固定主机指纹
remote-cc login 服务器-cc  # 登录 Claude（每台机器一次）
remote-cc 服务器-cc        # 开始使用
```

**自己加设备，不用找管理员**：在已经能用的机器上生成邀请码，到新机器上 `join`：

```sh
remote-cc device invite 服务器-cc              # 一次性邀请码，默认 24 小时有效（--ttl，最长 168h）
remote-cc device list 服务器-cc                # 这个用户的所有设备和未使用的邀请码
remote-cc device remove 服务器-cc 设备         # 按 list 显示的名字或指纹移除（丢了的电脑就这样撤销）
```

新设备进入的是**同一个容器**（同一个用户），但默认使用自己的 Claude 登录和自己的对话历史，和直接在新电脑上装 Claude Code 的效果一样；想共用就用 `--account 名字` / `--profile 名字`（见主 README）。

## 更新、移除、迁移

```sh
remote-cc update-server root@服务器     # 拉取最新镜像（包含新版 Claude Code），只重建有变化的容器；
                                        # 有会话正在进行时会拒绝，除非加 --force
remote-cc remove-server root@服务器     # 删除网关和所有用户容器；数据默认保留，--purge 一起删除
remote-cc server init root@服务器       # 只安装网关，不创建用户
```

**从 0.4 及更早的单容器部署迁移**：0.5 不再支持单容器布局，`update-server` 等命令遇到它会提示先迁移：

```sh
remote-cc server migrate --user owner root@服务器
```

迁移把原来的容器变成用户 `owner` 的容器：数据、对话历史、登录、已授权的公钥都保留，端口不变，原来的主机密钥移给网关，所以旧的 `known_hosts` 记录依然有效。迁移前会做备份（`<data 的上级目录>/remote-cc-before-multi-user-<时间>.tgz`），有活跃会话时拒绝执行，中途失败自动回滚。迁移后用 `remote-cc user invite` 给自己发一条邀请码再 `join`，或者按它打印的内容修改 ssh 配置。

**换服务器**：在新服务器上 `setup-server`，再给其他人发邀请码。要带走数据，把旧服务器的 `/var/lib/remote-cc` 复制到新服务器的同一位置后再执行 `setup-server`。

## 安全设计

- **隔离**：每个用户有自己的容器，也就有自己的 root、Claude 登录和对话历史。
  网关是标准的 OpenSSH：每个用户对应一个账号，`PermitOpen` 只放行 `rcc-<用户>:22`；不能分配 TTY，也不能做其他转发。网关账号唯一能执行的程序是 `rcc-gateway-cmd`，它只提供两件事：读取网关的主机公钥，以及为**这个账号自己**登记设备邀请码。
- **公钥**：每个用户的公钥存在宿主机的 `<data>/keys/<用户>/authorized_keys`。网关只读地挂载它用于认证；用户容器挂载自己那一份，用于端到端认证（所以即使有人绕过了网关的路由，别人的容器也不接受他的密钥），`device remove` 也是改这一份。authorized_keys 里的选项只能进一步收紧限制，不能放宽网关的 `PermitOpen`。
- **邀请码**：内含一把一次性密钥，分成两半：
  - 网关这一半在 `<data>/invites/<用户>`，用户容器看不到，只允许跳转到 `rcc-<用户>:22`。管理员的 `user invite` 直接写入；用户的 `device invite` 由网关上的 `rcc-self-invite` 写入，它只接受调用者自己的账号、只接受一把 ed25519 公钥、有效期 1 到 168 小时，每个用户最多 5 条未过期的邀请。
  - 容器这一半只能执行 `rcc-enroll`，没有任何转发权限。
  - 两半都带 `expiry-time`，到期自动失效。`rcc-enroll` 只接受一行不带任何选项的公钥，登记成功后删除这把一次性密钥，所以邀请码只能用一次。
- **容器权限**：sshfs 需要 `/dev/fuse`、`CAP_SYS_ADMIN`，并且要关掉 Docker 默认的 AppArmor 限制（默认规则禁止 mount）。容器没有用 `--privileged`，也没有挂载宿主机的其他目录，只挂了自己的数据目录。
- **sshd 配置**：只允许密钥登录。用户容器只放行 rcc 需要的 unix socket 反向转发；TCP 正向和反向转发、X11、agent 转发、隧道都被禁止。
- **公网端口**：Docker 发布的端口不经过 ufw，容器一启动就会暴露在公网上。如果云服务商有安全组，要在那里放行这个端口。
- **Claude Code 版本**：固定在镜像里，自动更新已关闭。升级 Claude Code 就执行 `update-server`。
- **测试**：CI 每次提交都会在 GitHub 的 runner 上部署一台真实的服务器，跑完整流程（`test/integration.sh`）：部署、邀请和加入、一次性邀请码不能复用、跨用户访问被拒绝、网关账号不能执行其他命令、自助加设备和移除设备、一次真实会话（文件经挂载读写、命令在本机执行、退出码传回）、从单容器部署迁移（数据和主机密钥保留）、更新和移除。

## 构建镜像

镜像由 GitHub Actions 在推送 `v*` 标签时构建，包括 amd64 和 arm64 两个版本（`.github/workflows/image.yml`）。也可以在 Actions 页面手动运行，指定要打包的 Claude Code 版本。
本地构建：`docker build deploy`。注意会临时占用约 2GB 的构建缓存，磁盘紧张的机器不要在本地构建。
