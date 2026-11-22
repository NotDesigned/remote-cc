# 服务端部署（Docker）

服务端是一个容器，里面有 Claude Code、sshd、sshfs。rcc 用 ssh 连进容器；Claude 在项目外的文件操作只影响容器，不影响宿主机。

## 一条命令部署（推荐）

宿主机需要有 `/dev/fuse`。Docker 可以事先装好（含 compose 插件），也可以加 `--install-docker` 让 rcc 自动安装。在**本机**上执行：

```sh
rcc setup-server root@新服务器
rcc setup-server ubuntu@新服务器                    # 不能用 root 登录、只有一个带 sudo 的账号时
rcc setup-server --install-docker ubuntu@新服务器   # 全新的机器，还没装 Docker
```

**只有 sudo 账号时**：如果登录的用户不是 root，rcc 会自动用 sudo 提权。
- 免密 sudo（云服务器的 `ubuntu` 账号通常是这样）不需要任何操作。
- 需要密码时，rcc 会在本机以不回显的方式询问，再通过 stdin 交给 `sudo -S`，密码不会出现在任何命令行或日志里。
- `--keys-from-host` 和 `--migrate-history` 读的是登录用户自己家目录里的文件，在提权之前就读好。

以后日常连接的是**容器里的** root，由容器自己的 sshd 认证，和宿主机能不能用 root 登录无关。

这条命令会通过 ssh 完成以下步骤：
- 检查 Docker 和 `/dev/fuse`。
- 把 `compose.yml` 和 `.env` 写到 `/opt/remote-cc`。
- 把你本机的公钥（优先取 ssh-agent 里的，其次是 `~/.ssh/id_*.pub`）加进容器的 `authorized_keys`。
- 拉取 `ghcr.io/notdesigned/remote-cc-server` 并启动容器。
- 把容器的主机公钥写进你本机的 `known_hosts`。这个公钥是通过已经验证过的 ssh 连接取得的，所以第一次连接容器时不需要盲目信任指纹。

最后它会**在本机实际测试一下容器的端口能否连通**，然后打印对应的 `~/.ssh/config` 配置：
- **端口能连通**：直连配置，`HostName <服务器>`、`Port 2222`。
- **端口不通**（通常是云服务商的安全组或防火墙没有放行）：要么去放行该 TCP 端口，要么使用它打印的跳转配置，通过宿主机原有的 ssh 进入容器：
  ```
  Host cc
      HostName 127.0.0.1
      Port 2222
      User root
      ProxyJump <宿主机，即 setup-server 时用的那个目标>
      HostKeyAlias [<服务器地址>]:2222
  ```
  `HostKeyAlias` 必须带方括号和端口。setup-server 写进 known_hosts 的记录就是 `[服务器]:2222`，而 ssh 用 HostKeyAlias 查找时会原样使用它，不会再拼接端口（已实测）。
  缺点是 Windows 的 ssh 不能复用连接，每次连接都要走两跳，会慢一些。

按提示操作：

```sh
rcc login cc     # 在容器里登录 Claude（每个账号一次）
rcc cc           # 在项目目录里使用
```

常用参数：

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `--port N` | 2222 | 容器 sshd 对外的端口（宿主机原有的 22 端口不受影响） |
| `--bind ADDR` | 0.0.0.0 | 改成 `127.0.0.1` 后不对外开放，只能经 `ProxyJump` 进入 |
| `--data DIR` | /var/lib/remote-cc | 持久数据：Claude 登录、对话历史、rcc 的账号和 profile、sshd 主机密钥 |
| `--key FILE.pub` | 自动查找 | 要授权的公钥，可以重复指定 |
| `--keys-from-host` | 关 | 同时授权宿主机当前用户已经授权的公钥 |
| `--migrate-history` | 关 | 从以前在宿主机上直接运行的 rcc 复制对话历史 profile |
| `--image IMG` | ghcr.io/notdesigned/remote-cc-server:latest | 服务端镜像 |
| `--name NAME` | remote-cc | 容器名，同一台机器可以按不同名字、端口、数据目录跑多个实例 |
| `--install-docker` | 关 | 没有 Docker 时，用官方脚本 get.docker.com 安装 |

**更新**：`rcc update-server user@host` 会拉取最新镜像（包含新版 Claude Code），只有镜像或 compose 配置变化时才重建容器，数据、公钥和端口都保留。
如果有会话正在进行，它会拒绝执行并说明原因，加 `--force` 可以强制更新。更新后会清理被替换掉的旧 remote-cc 镜像，只删除带本项目标签的悬空镜像。
重复执行 `setup-server` 也可以，但它不会主动拉取更新的镜像。

**移除**：`rcc remove-server user@host` 会停止并删除容器、镜像和 `/opt/remote-cc`，同时删除本机 `known_hosts` 里的对应条目。数据目录默认保留，加 `--purge` 才会一起删除，里面包括 Claude 登录、对话历史和主机密钥。和 setup-server 一样支持 sudo 账号。

**客户端配置**：A 连的是**容器**，不是宿主机。`Port` 要写容器的端口（默认 2222），`User` 写 `root`（容器里的 root）：

```
Host cc
    HostName <服务器地址>
    Port 2222
    User root
    IdentityFile ~/.ssh/id_ed25519    # setup-server 时授权的那把密钥
```

用 `ssh cc hostname` 检查，输出 `remote-cc` 就说明连对了。宿主机上不需要安装 Claude Code。

**换服务器**：在新服务器上执行一次 `rcc setup-server`。如果要带走历史和登录，把旧服务器上的 `/var/lib/remote-cc` 复制到新服务器的同一位置，再执行 `setup-server`。

## 手动部署

```sh
mkdir -p /opt/remote-cc && cd /opt/remote-cc
curl -fsSLO https://raw.githubusercontent.com/NotDesigned/remote-cc/main/deploy/compose.yml
cp .env.example .env    # 或者照 .env.example 自己写；公钥放进 <RCC_DATA>/root/.ssh/authorized_keys
docker compose up -d
```

## 安全说明

- **容器权限**：容器需要 `/dev/fuse`、`CAP_SYS_ADMIN`，并且要关掉 Docker 默认的 AppArmor 限制（默认规则禁止 mount），sshfs 才能工作。容器没有用 `--privileged`，也没有挂载宿主机的任何目录，只挂了数据目录。
- **sshd 配置**：只允许密钥登录，最多 3 次认证尝试，登录等待时间 20 秒。转发只放行 rcc 需要的 unix socket 反向转发；TCP 正向和反向转发、X11、agent 转发、隧道都被禁止。这些已经实测过。
- **公网端口**：Docker 发布的端口不经过 ufw，容器一启动就会暴露在公网上。如果云服务商有安全组，要在那里放行这个端口。
- **Claude Code 版本**：固定在镜像里，自动更新已关闭。升级 Claude Code 就拉取新镜像，再执行一次 `setup-server`。

## 构建镜像

镜像由 GitHub Actions 在推送 `v*` 标签时构建，包括 amd64 和 arm64 两个版本（`.github/workflows/image.yml`）。也可以在 Actions 页面手动运行，指定要打包的 Claude Code 版本。
本地构建用 `docker compose build`。注意本地构建会临时占用约 2GB 的构建缓存，磁盘紧张的机器不要在本地构建。
