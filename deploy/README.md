# 服务端部署（Docker）

服务端是一个容器，里面有 Claude Code、sshd、sshfs。rcc 用 ssh 连进容器；Claude 在项目外的文件操作只影响容器，不影响宿主机。

## 一条命令部署（推荐）

宿主机只需要装好 Docker（含 compose 插件），并且有 `/dev/fuse`。在**本机**上执行：

```sh
rcc setup-server root@新服务器
```

这条命令会通过 ssh 完成以下步骤：
- 检查 Docker 和 `/dev/fuse`。
- 把 `compose.yml` 和 `.env` 写到 `/opt/remote-cc`。
- 把你本机的公钥（优先取 ssh-agent 里的，其次是 `~/.ssh/id_*.pub`）加进容器的 `authorized_keys`。
- 拉取 `ghcr.io/notdesigned/remote-cc-server` 并启动容器。
- 把容器的主机公钥写进你本机的 `known_hosts`。这个公钥是通过已经验证过的 ssh 连接取得的，所以第一次连接容器时不需要盲目信任指纹。

最后它会打印一段 `~/.ssh/config` 配置，按提示操作：

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

重复执行 `setup-server` 就是更新：更新配置、拉取镜像、重建容器，数据保留。

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
