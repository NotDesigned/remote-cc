# 更新日志

> 读者：升级 remote-cc 的使用者和部署者

0.6.0 及更早的条目是 2026-11-25 根据 git 标签和提交说明补写的；那时的命令名是旧名字，按原样写出。

## 未发布

### 新增

- `remote-cc server user rebuild SERVER NAME`：重建一个服务器用户的容器，数据、Claude 登录、密钥和设置都保留；有会话时拒绝，除非 `--force`。
- 选配置更省事：`--rcb` 和 `rcb use` 接受 `rcb profiles` 里的序号（这个列表多了「#」一列），以及配置 id、名称或 Claude 邮箱中任意一段，只要只有一个配置含有它；`rcb use` 不写配置时列出全部配置让你选；在用 `rcb use` 记过的目录里，`account login` 等账号命令不写 `--rcb` 和服务器也会用这个目录的配置。
- `remote-cc host list`：列出这台电脑上的容器别名（`HOST`），以及各自的服务器地址和服务器用户；标出当前目录用 `rcb use` 记住的那一个。

### 变更

- 修复：Windows 上 `server update`、`server setup`、`server migrate`、`server egress` 等管理命令报「The filename or extension is too long」。发给服务器的长脚本改为压缩后传送，服务器需要有 `gzip`。

### 移除

无

### 升级步骤

无

## 0.7.0（2026-11-25）

### 新增

- `remote-cc help <命令>`（或任何命令后加 `-h`）显示这条命令的全部选项；`remote-cc help` 是按用途分组的总览。
- 只输入一组命令的名词（`remote-cc server`、`server user`、`account`、`device`、`rcb`）时，列出这一组的子命令。
- 断线不再结束会话：网络中断后 remote-cc 自动重连（Ctrl-C 放弃），容器里的 Claude Code 不受影响，断线时正在执行的命令和文件读写在重连后接着完成。服务器保留断线的会话 10 分钟。见 [docs/usage.md](docs/usage.md#断线)。
- 断线在 15 秒内发现（以前约 45 秒），也能发现服务器被冻结、连接却没断开的情况。
- 文档按读者重新整理（`docs/usage.md`、`deploy/README.md`、`docs/architecture.md`、`CONTRIBUTING.md`），`docs/DESIGN.md` 拆分进 `docs/architecture.md` 和 `docs/records/`；新增文档检查 `go test ./internal/docs`。

### 变更

- 帮助里区分两种主机：`HOST` 是你的容器（`server setup` 或 `device join` 写好的 ssh 别名），`SERVER` 是 Docker 宿主机（root 或 sudo 账号）。
- 启动更快：到同一台服务器的 ssh 连接在最后一次使用后保留 10 分钟，之后的会话和 `account`、`device` 命令不用重新握手；会话只用两次 ssh 调用（以前四次，Windows 上每次都要重新连接）；电脑上的检查、`--rcb` 的会话授权和探测同时进行；rcc-shim 压缩后上传。
- 会话在服务器上由一个守护进程运行（随 rcc-shim 上传，不需要更新服务端镜像）；Claude Code 的终端、文件和命令走同一条 ssh 通道，不再用 `ssh -t` 和 `-R` 反向转发。
- 每条命令只接受自己的选项。以前被悄悄忽略的选项现在会报错，例如 `server remove --install-docker`、`device list -p 2222`、`version --bogus`。命令行写错时退出码为 2，并打印这条命令的用法。

### 移除

- 旧命令名不再可用，统一为「名词 动词」。用旧名字时报错并给出新名字（退出码 2）：

  | 旧名字 | 新名字 |
  | --- | --- |
  | `setup-server` | `server setup` |
  | `update-server` | `server update` |
  | `remove-server` | `server remove` |
  | `servers` | `server list` |
  | `login` / `logout` / `status` / `setup-token` / `token` | `account login` / `account logout` / `account status` / `account setup-token` / `account token` |
  | `accounts` | `account list` |
  | `join` | `device join` |
  | `user add` / `user invite` / `user list` / `user config` / `user remove` | `server user add` / `server user invite` / `server user list` / `server user config` / `server user remove` |

- `account list` 不再接受 `--account` 和 `--rcb`（它列出所有账号，这两个选项本来就不起作用）。

### 升级步骤

- 把脚本、别名和文档里的旧命令名按上表换成新名字。服务器不需要操作。
- 本版本和旧版本的 remote-cc 可以同时连同一个容器；旧版本的会话不受影响。
- 和 RCB 一起升级：RCB 配置卡片上的首次登录提示从下一个 RCB 版本起才是 `account login --rcb`，旧版 RCB 提示的 `login --rcb` 在本版本会报错（报错里会给出新名字）。

## 0.6.0（2026-11-25）

### 新增

- `--rcb <配置>`：以 RCB 浏览器配置的身份运行 Claude Code，用它的 Claude 账号，全部流量经它的节点出去。新增 `rcb login`、`rcb profiles`、`rcb use`、`rcb logout`。
- `server egress`：在服务器上部署出口主机（新镜像 `remote-cc-egress`），由 RCB 管理员批准。
- `login --rcb`：在该配置的浏览器里一键登录 Claude Code，手动粘贴仍然可用。

### 变更

- `update-server` 同时更新出口主机，并检查、修正每个容器到出口主机的挂载。

### 移除

无

### 升级步骤

- 对每台服务器执行一次 `update-server`，给容器加上出口主机的挂载；之后才能部署出口主机、使用 `--rcb`。
- 需要支持 remote-cc 接口的 RCB 版本，见 [RCB：兼容版本](https://github.com/NotDesigned/remote-claude-browser/blob/main/docs/remote-cc.md#compatibility)。

## 0.5.4（2026-11-24）

### 新增

- 经网关连接时容器停止或不存在，报错会说明原因，并提示部署者怎样启动它。

### 变更

- `update-server` 先启动停止的容器，再读取版本、比较镜像。

### 移除

无

### 升级步骤

无

## 0.5.3（2026-11-24）

### 新增

无

### 变更

- sshfs 的目录缓存恢复，时间缩短为 5 秒；「不存在」仍然不缓存。0.5.1 关掉目录缓存后，大目录明显变慢。

### 移除

无

### 升级步骤

无

## 0.5.2（2026-11-24）

### 新增

- `update-server --all`：依次更新这台电脑管理过的所有服务器，最后汇总。
- `servers`：列出这台电脑管理过的服务器（记在 `~/.ssh/remote-cc/servers.json`）。

### 变更

无

### 移除

无

### 升级步骤

- 更早部署的服务器，对每台执行一次 `update-server <宿主机>`，它就会被记进列表。

## 0.5.1（2026-11-24）

### 新增

- Claude Code 自己调用的 git 在电脑上执行，`--worktree` 可用；`--remote-git` 改回在容器里执行。服务端镜像加入 git。
- 启动前先在电脑的 shell 里试跑一条命令，失败时停下并说明原因。
- 上一次会话的日志保留为 `prev.log`。

### 变更

- sshfs 不再缓存「不存在」，也关掉了目录缓存（0.5.3 恢复目录缓存）。

### 移除

无

### 升级步骤

- 执行 `update-server`，换上带 git 的镜像。

## 0.5.0（2026-11-24）

### 新增

- `device invite`、`device list`、`device remove`：服务器用户自己给另一台电脑发邀请码、查看和移除设备，不用找部署者。

### 变更

- 端到端测试改在 GitHub 的 runner 上运行。

### 移除

- 单容器部署（`setup-server --single`）、`--keys-from-host`、`--migrate-history`，以及非 root 的普通挂载方式。

### 升级步骤

- 单容器部署的服务器先执行一次 `server migrate --user owner <宿主机>`，见 [deploy/README.md](deploy/README.md#迁移)。

## 0.4.1（2026-11-24）

### 新增

无

### 变更

- `setup-server` 默认部署多用户服务器：安装网关、为你创建第一个用户（默认 `owner`），并在电脑上写好 ssh 配置；网关端口不通时自动改用经宿主机跳转。
- 每个容器显式使用 compose 项目名 `rcc-<用户名>`，不再触碰不属于本实例的同名容器。

### 移除

- 不再使用 `--remove-orphans`。

### 升级步骤

- 执行一次 `update-server`，迁移容器的项目名（有活跃会话时会拒绝）。

## 0.4.0（2026-11-24）

### 新增

- 多用户服务器：一个网关、每个用户一个容器；`server init`、`user add`、`user invite`、`user list`、`user config`、`user remove`。
- 一次性邀请码和 `join`。
- `server migrate`：把单容器部署转换为多用户部署。

### 变更

- `update-server`、`remove-server` 支持多用户服务器。

### 移除

无

### 升级步骤

无

## 0.3.3（2026-11-23）

### 新增

- 一行命令的安装脚本 `install.sh`（macOS、Linux）和 `install.ps1`（Windows），默认命令名 `remote-cc`。

### 变更

无

### 移除

无

### 升级步骤

无

## 0.3.2（2026-11-22）

### 新增

无

### 变更

- 容器端口同时在 IPv4 和 IPv6 上发布。

### 移除

无

### 升级步骤

- 执行 `update-server`，它会改写旧的设置并重建容器。

## 0.3.1（2026-11-22）

### 新增

- `-vv`、`-vvv`、`-vvvv` 同时让 ssh 输出调试信息。

### 变更

无

### 移除

无

### 升级步骤

无

## 0.3.0（2026-11-22）

### 新增

- `update`：把电脑上的 remote-cc 换成新的 release；`version --check` 查看最新版本。
- `update-server`：拉取新镜像，有变化才重建容器，有会话时拒绝（除非 `--force`）。

### 变更

- 不指定 `--account` 时，每台电脑用自己的 Claude 账号；`--account default` 使用共用的 `~/.claude`。
- 服务器上的 shim 按内容寻址，不同版本互不覆盖。
- 提示文字使用实际调用的程序名。

### 移除

无

### 升级步骤

- 更早的版本没有 `update` 命令，重新下载安装。
- 每台电脑执行一次 `login` 登录自己的 Claude 账号，或者用 `--account default` 继续用共用的登录。

## 0.2.2（2026-11-22）

### 新增

- `remove-server`。
- 连到宿主机而不是容器时，找不到 claude 的报错会指出这一点。

### 变更

无

### 移除

无

### 升级步骤

无

## 0.2.1（2026-11-22）

### 新增

- `setup-server` 可以经带 sudo 的账号部署；`--install-docker`、`--name`。

### 变更

无

### 移除

无

### 升级步骤

无

## 0.2.0（2026-11-22）

### 新增

- 服务端 Docker 镜像和一条命令的部署 `setup-server`。
- 推送 `v*` 标签时 CI 构建各平台的二进制并发布 release。

### 变更

无

### 移除

无

### 升级步骤

无

## 0.1.0（2026-11-22）

### 新增

- 第一个版本：命令通道、文件通道、一条命令启动会话；每台电脑的历史档案；多个 Claude 账号；结束时清理电脑上启动的所有进程。

### 变更

无

### 移除

无

### 升级步骤

无
