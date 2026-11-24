# remote-cc

本机不用安装 Claude Code，也能用 Claude Code 写本机的代码。

```
A (本机，运行 remote-cc)  ──ssh──►  B (服务器，Docker 容器里跑 Claude Code)
```

`remote-cc` 从 A ssh 到 B，在 B 上启动 Claude Code，界面显示在 A 的终端里：

- **命令**：通过 `CLAUDE_CODE_SHELL_PREFIX` 加反向 unix socket，Bash 工具的命令在 A 上执行。
- **文件**：通过反向 sshfs（`sshfs -o slave` 加 remote-cc 内置的 SFTP 服务），把 A 的项目目录挂载到 B 上相同的绝对路径，
  Read/Write/Edit/Glob/Grep 实际读写的是 A 上的文件。

详细设计见 [docs/DESIGN.md](docs/DESIGN.md)。

## 安装

### 本机（A）

**Windows**（PowerShell）：

```powershell
irm https://raw.githubusercontent.com/NotDesigned/remote-cc/main/install.ps1 | iex
```

**macOS / Linux**：

```sh
curl -fsSL https://raw.githubusercontent.com/NotDesigned/remote-cc/main/install.sh | sh
```

安装脚本会下载本平台最新的 release，用 `SHA256SUMS` 校验，安装为 `remote-cc`：
- Windows：装到 `%USERPROFILE%\.local\bin`，并加入用户 PATH。
- macOS / Linux：装到 `~/.local/bin`。如果该目录不在 PATH 里，脚本会提示怎么添加。

可以用环境变量调整：`RCC_VERSION`（例如 `v0.5.0`，默认为最新版）、`RCC_INSTALL_DIR`、`RCC_NAME`（命令名，默认 `remote-cc`）。用法示例：

```powershell
$env:RCC_VERSION = 'v0.5.0'; irm https://raw.githubusercontent.com/NotDesigned/remote-cc/main/install.ps1 | iex
```
```sh
curl -fsSL https://raw.githubusercontent.com/NotDesigned/remote-cc/main/install.sh | RCC_INSTALL_DIR=/usr/local/bin sh
```

本机还需要：`ssh` 客户端，以及 `bash`（Windows 上是 Git for Windows 自带的 Git Bash，Claude 的命令在它里面执行）。缺少时安装脚本会提示。

之后升级：`remote-cc update`。

> 默认命令名是 `remote-cc`，不用 `rcc`，因为 `rcc` 很容易和别的程序重名，比如 Anaconda / Qt 自带的资源编译器 `rcc.exe`。remote-cc 打印的所有提示，都会使用你实际调用的命令名。

### 服务端（B）

在**本机**上执行一条命令就能部署：默认部署的是多用户服务器，包括一个网关和每人一个容器，你自己是第一个用户。

```sh
remote-cc setup-server root@服务器                    # 或 ubuntu@服务器（有 sudo 即可）
remote-cc setup-server --install-docker ubuntu@服务器 # 全新的机器，顺便安装 Docker
```

宿主机需要 `/dev/fuse`。Docker 已经装好就直接用；没有的话，加 `--install-docker` 自动安装。

`setup-server` 会完成以下几件事：
- 在服务器上安装网关，并为你创建用户（默认名为 `owner`，可以用 `--user` 指定）。
- 在本机生成这台服务器专用的密钥，并写好 `~/.ssh/remote-cc/config`（别名是 `<主机名第一段>-cc`），同时固定主机指纹。
- 测试网关端口能否连通；如果不通，自动改用经宿主机 ssh 跳转的配置。
- 最后实际连一次，确认配置可用。

详见 [deploy/README.md](deploy/README.md)。

B 端不需要单独安装别的东西：remote-cc 每次连接时都会自动上传匹配架构的 `rcc-shim`。

**多人共用一台服务器**：每人一个容器，整台服务器只开一个端口，用一次性邀请码加人；用户可以自己给新电脑发邀请码，不用找管理员。

```sh
remote-cc user add root@服务器 alice     # 管理员：建用户，得到一条一次性邀请码
remote-cc join <邀请码>                  # 用户：一条命令完成密钥、ssh 配置、主机指纹
remote-cc device invite 服务器-cc        # 用户：在已经能用的电脑上，给自己的另一台电脑发邀请码
```

> 从 0.4 及更早版本升级：0.5 不再支持单容器部署，先执行一次 `remote-cc server migrate --user owner root@服务器`，详见 [deploy/README.md](deploy/README.md#更新移除迁移)。

## 快速上手

```sh
remote-cc setup-server root@服务器.example.com   # 1. 部署服务端，并配置好本机（别名：服务器-cc）
remote-cc login 服务器-cc                        # 2. 在容器里登录 Claude（一次）
cd 你的项目 && remote-cc 服务器-cc               # 3. 开始使用
remote-cc user add root@服务器.example.com alice # 加人：输出一条一次性邀请码，对方执行 remote-cc join <邀请码>
remote-cc device invite 服务器-cc                # 自己的另一台电脑：输出邀请码，在那台电脑上 remote-cc join <邀请码>
```

## 用法

```sh
remote-cc cc                         # 在当前目录启动 Claude Code
remote-cc cc ~/proj                  # 指定项目目录
remote-cc cc -- --continue           # 继续这个目录上次的对话（--resume 选择历史对话）
remote-cc cc -- remote-control       # -- 后面的参数原样传给 claude
remote-cc cc -- --dangerously-skip-permissions   # 容器里是 root 时也能用（remote-cc 会设置 IS_SANDBOX=1，--no-is-sandbox 可关闭）
remote-cc -p 2222 -i ~/.ssh/key root@服务器 .     # 也可以直接写 ssh 参数，不用 ~/.ssh/config
remote-cc --profile work cc          # 指定历史 profile（默认每台本机各自一份；none = 用 B 原本的历史）
remote-cc -v cc                      # 显示过程；-vv / -vvv / -vvvv 同时让 ssh 输出调试信息（相当于 ssh -v / -vv / -vvv）
```

对话历史保存在 B 上，按本机隔离：两台机器即使项目路径相同，也不会看到对方的对话。

A 上执行的命令会记录到日志：Linux 是 `~/.cache/rcc/last.log`，Windows 是 `%LOCALAPPDATA%\rcc\last.log`，macOS 是 `~/Library/Caches/rcc/last.log`。

### 账号

**默认情况下，每台本机使用自己专属的 Claude 账号**，账号名和本机的历史 profile 名相同。所以不同的机器不会共用 Claude 凭据，新机器执行一次 `remote-cc login cc` 登录即可。

```sh
remote-cc login cc                     # 登录本机专属账号：在本机浏览器打开链接，把授权码粘贴回来
remote-cc login --account work cc      # 登录一个命名账号，几台机器可以共用
remote-cc --account work cc            # 使用命名账号（也可以设置 RCC_ACCOUNT=work）
remote-cc status cc
remote-cc accounts cc                  # 列出所有账号的登录状态
remote-cc setup-token --account ci cc  # 生成长期令牌，然后：
remote-cc token --account ci cc        # 把令牌存进 ci 账号（隐藏输入）；--clear 删除
remote-cc logout cc
```

- `--account default` 使用 B 上共用的 `~/.claude`。
- 每个账号在 B 上有独立的配置目录（凭据、组织策略、对话历史），settings、CLAUDE.md、skills、plugins 等个人配置是共用的。
- 账号分开只是为了不混用，**不是安全边界**：能 ssh 进容器的人都是容器里的 root。不信任的人，请给对方单独建一个用户（`user add`），每个用户是独立的容器。

### 更新和管理服务端

```sh
remote-cc update                        # 把本机的 remote-cc 换成最新 release（会校验 SHA256SUMS；Windows 上也可以替换正在运行的 exe）
remote-cc update-server root@服务器     # 拉取最新服务端镜像（包含新版 Claude Code），有变化才重建容器；
                                        # 有会话在进行时会拒绝执行，除非加 --force
remote-cc remove-server root@服务器     # 删除容器；加 --purge 连数据一起删除
```

v0.3.0 之前的版本里没有 `update` 命令，需要用上面的安装命令重新安装。

## 构建

```sh
./scripts/build.sh        # 输出 dist/rcc-{linux,darwin,windows}-{amd64,arm64}，内嵌 B 端 shim
go test ./...             # 包括真实 sshfs 挂载测试（需要 root、sshfs 和 /dev/fuse，否则跳过）
```

代码结构：

| 位置 | 内容 |
| --- | --- |
| `cmd/rcc`、`internal/cli` | 程序入口和命令行解析 |
| `internal/client` | 本机这一侧：会话、账号命令、设备命令 |
| `internal/server` | 管理命令：setup-server、user、update-server、migrate 等 |
| `internal/scripts` | 在远端执行的 shell 脚本（`.sh` 文件，编译时嵌入，CI 里跑 shellcheck） |
| `internal/remote`、`internal/sshconf` | ssh 连接与提权；本机 ssh 配置、密钥、邀请码 |
| `internal/agent`、`internal/shim`、`internal/fsserver` | 命令通道（A 上的 agent、B 上的 shim）和文件通道（SFTP） |
| `deploy/` | 服务端镜像和 compose 文件 |
| `test/integration.sh` | 端到端测试，CI 在 runner 上部署一台真实服务器跑完整流程 |

CI（`.github/workflows/ci.yml`）每次推送都跑 gofmt、三个平台的 vet、单元测试、shellcheck 和端到端测试。推送 `v*` 标签后，CI 构建各平台的二进制和服务端镜像（amd64、arm64）并发布 release。

## 许可证

[MIT](LICENSE)。发布的二进制包含 Go 运行时和若干 BSD 许可证的模块，它们的声明见 release 中的 `THIRD_PARTY_LICENSES.txt`。
