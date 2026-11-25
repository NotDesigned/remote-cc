# remote-cc

> 读者：第一次来的人

电脑上不用安装 Claude Code，也能用 Claude Code 写这台电脑上的代码。Claude Code 运行在服务器的 Docker 容器里，界面显示在电脑的终端里；它执行的 Bash 命令在电脑上运行，读写的也是电脑上的项目文件。

```
电脑（运行 remote-cc） ──ssh──► 服务器（网关，以及每个服务器用户一个容器，容器里运行 Claude Code）
```

一台服务器可以给多个人用：每人一个容器，整台服务器只开一个端口，用一次性邀请码加人。还可以连接 [Remote Claude Browser（RCB）](https://github.com/NotDesigned/remote-claude-browser)，用浏览器配置的 Claude 账号和出口运行 Claude Code。原理见 [docs/architecture.md](docs/architecture.md)。

## 安装

### 电脑

Windows（PowerShell）：

```powershell
irm https://raw.githubusercontent.com/NotDesigned/remote-cc/main/install.ps1 | iex
```

macOS、Linux：

```sh
curl -fsSL https://raw.githubusercontent.com/NotDesigned/remote-cc/main/install.sh | sh
```

安装脚本下载本平台的 release，用 `SHA256SUMS` 校验后安装为 `remote-cc`：Windows 上装到 `%USERPROFILE%\.local\bin` 并加入用户 PATH；macOS 和 Linux 上装到 `~/.local/bin`，这个目录不在 PATH 里时会提示怎么添加。

可以用环境变量调整（来自 `install.sh` 和 `install.ps1`）：

| 变量 | 默认值 | 作用 |
| --- | --- | --- |
| `RCC_VERSION` | 最新的 release | 要安装的版本，写 release 的标签，如 `vX.Y.Z` |
| `RCC_INSTALL_DIR` | 见上文 | 安装目录 |
| `RCC_NAME` | `remote-cc` | 命令名 |
| `RCC_REPO` | `NotDesigned/remote-cc` | 从哪个 GitHub 仓库下载 |

```powershell
$env:RCC_VERSION = 'vX.Y.Z'; irm https://raw.githubusercontent.com/NotDesigned/remote-cc/main/install.ps1 | iex
```

```sh
curl -fsSL https://raw.githubusercontent.com/NotDesigned/remote-cc/main/install.sh | RCC_INSTALL_DIR=/usr/local/bin sh
```

电脑上还需要 `ssh` 客户端和 `bash`（Windows 上是 Git for Windows 带的 Git Bash，Claude 的命令在它里面执行）。缺少时安装脚本会提示。之后更新用 `remote-cc update`。

默认命令名是 `remote-cc` 而不是 `rcc`，因为 `rcc` 容易和别的程序重名，例如 Anaconda、Qt 带的 `rcc.exe`。remote-cc 打印的提示都使用你实际调用的命令名。

### 服务器

服务器上不用手动安装任何东西：部署者在自己的电脑上执行 `remote-cc server setup`，它经 ssh 在宿主机上装好一切。宿主机需要 Docker（可以让 remote-cc 自动安装）和 `/dev/fuse`，详见 [deploy/README.md](deploy/README.md)。

## 上手

### 自己部署服务器

```sh
remote-cc server setup root@<服务器>  # 部署服务器，你是第一个服务器用户；在电脑上写好容器的别名 <服务器>-cc
remote-cc account login <服务器>-cc   # 在容器里登录 Claude（每台电脑一次）
cd ~/proj && remote-cc <服务器>-cc    # 开始使用
```

别名是服务器主机名的第一段加 `-cc`。给别人加用户见 [deploy/README.md](deploy/README.md#用户)。

### 加入别人的服务器

部署者给你一条一次性邀请码，然后：

```sh
remote-cc device join <邀请码>       # 生成密钥、登记这台电脑、写好 ssh 配置和主机指纹
remote-cc account login <服务器>-cc  # 在容器里登录 Claude（每台电脑一次）
cd ~/proj && remote-cc <服务器>-cc   # 开始使用
```

`remote-cc help` 列出所有命令。日常用法、Claude 账号、给自己的另一台电脑发邀请码、连接 RCB，见 [docs/usage.md](docs/usage.md)。

## 文档

| 读者 | 文档 | 内容 |
| --- | --- | --- |
| 使用者 | [docs/usage.md](docs/usage.md) | 会话、Claude 账号、历史档案、设备、更新、连接 RCB、排查 |
| 部署者 | [deploy/README.md](deploy/README.md) | 部署服务器、用户、更新、迁移、目录、备份、出口主机、排查 |
| 升级的人 | [CHANGELOG.md](CHANGELOG.md) | 各版本的变化和升级步骤 |
| 开发者 | [docs/architecture.md](docs/architecture.md) | 当前设计、出网、安全模型和决策 |
| 开发者 | [CONTRIBUTING.md](CONTRIBUTING.md) | 构建、测试、CI、镜像、发布 |
| 写文档的人 | [docs/documentation.md](docs/documentation.md) | 文档规则和术语 |
| 维护者 | [docs/records/2026-11-21-初始设计和实测.md](docs/records/2026-11-21-初始设计和实测.md) | 初始设计、早期计划和实测（存档） |
| 维护者 | [docs/records/2026-11-21-与-cc-desk-tunnel-的对比.md](docs/records/2026-11-21-与-cc-desk-tunnel-的对比.md) | 与 cc-desk-tunnel 的对比（存档） |
| 维护者 | [docs/records/2026-11-24-compose-项目名冲突事故.md](docs/records/2026-11-24-compose-项目名冲突事故.md) | compose 项目名冲突事故（存档） |
| 维护者 | [docs/records/2026-11-24-服务端和多用户的实测.md](docs/records/2026-11-24-服务端和多用户的实测.md) | 服务端和多用户的实测（存档） |

## 许可证

[MIT](LICENSE)。发布的程序包含 Go 运行时和若干 BSD 许可证的模块，它们的声明见 release 里的 `THIRD_PARTY_LICENSES.txt`。
