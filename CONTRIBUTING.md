# 参与开发

> 读者：改代码的开发者和发布版本的维护者

设计见 [docs/architecture.md](docs/architecture.md)，文档规则见 [docs/documentation.md](docs/documentation.md)。

## 构建

需要 `go.mod` 里写的 Go 版本。

```sh
./scripts/build.sh                      # 输出 dist/rcc-{linux,darwin,windows}-{amd64,arm64}，内嵌容器里用的 rcc-shim
TARGETS=linux/amd64 ./scripts/build.sh  # 只构建一个平台
go build -o remote-cc ./cmd/rcc         # 不带内嵌 shim，见下文
```

- `scripts/build.sh` 先编译 linux/amd64 和 linux/arm64 的 `rcc-shim`，放进 `internal/embedded/shims/`，再编译各平台的程序，并生成 `SHA256SUMS` 和第三方许可证声明。版本号取自 `VERSION`，默认是 `git describe` 的结果。
- 直接 `go build` 出来的程序不带内嵌 shim，只有电脑和服务器同为 Linux、架构相同时才能用（会把程序自身上传为 shim）。

## 代码结构

| 位置 | 内容 |
| --- | --- |
| `cmd/rcc`、`internal/cli` | 程序入口和命令表（命令、参数和帮助文字都在 `internal/cli/cli.go` 的命令表里） |
| `internal/client` | 电脑一侧：会话、Claude 账号、设备、RCB 一键登录 |
| `internal/server` | 管理命令：`server …` 和 `server user …` |
| `internal/scripts` | 在容器和宿主机上执行的 shell 脚本（`.sh` 文件，编译时嵌入） |
| `internal/remote`、`internal/sshconf` | ssh 连接与提权；电脑上的 ssh 配置、密钥、邀请码 |
| `internal/agent`、`internal/shim`、`internal/fsserver`、`internal/procjob` | 命令通道（电脑上的命令服务、容器里的 shim）、文件通道（SFTP）、进程清理 |
| `internal/rcb`、`internal/egress`、`internal/netexec`、`cmd/rcc-egress` | RCB 客户端、出口主机、进入节点命名空间 |
| `internal/docs` | 文档检查 |
| `deploy/` | 服务端镜像、出口主机镜像和 compose 文件 |
| `test/integration.sh` | 端到端测试 |

## 测试

```sh
go test ./...            # 单元测试；真实 sshfs 挂载的测试需要 root、sshfs 和 /dev/fuse，否则跳过
go test ./internal/docs  # 文档检查，见下文
go vet ./...
gofmt -l .
```

端到端测试 `test/integration.sh` 在一台机器上部署真实的服务器：这台机器既是 Docker 宿主机（经 sudo 账号 ssh 到 localhost），也是几台使用者的电脑（几个 Linux 用户）。它覆盖部署、邀请和加入、一次性邀请码不能复用、跨用户访问被拒绝、网关账号不能执行其他命令、自助加设备和移除设备、一次真实会话（用 `test/mockapi.py` 这个假的 Messages API 驱动真实的 Claude Code）、从单容器部署迁移、更新和移除。它会改动机器上的 Docker 和用户，只在一次性的机器上运行，用法见 CI 里的 `integration` 任务：

```sh
TARGETS=linux/amd64 ./scripts/build.sh
RCC=dist/rcc-linux-amd64 IMAGE=rcc-test:ci sh test/integration.sh
```

## 文档检查

`go test ./internal/docs` 检查：相对链接和锚点（含 `<a id>`）能解析；每个 Markdown 文件都在 README 的「文档」表里（例外见 `docs/documentation.md`）；每个文件开头有「读者」行；术语表「不要写」列里的词；原则 3、4、6 的 grep；文档里写到的每个 `remote-cc <命令>` 都在命令表里；文档里写到的每个 `RCC_*` 环境变量都在代码里，面向使用者和部署者的变量都写进了文档。

改了行为的 PR，同一个 PR 里改它的归属文件（见 `docs/documentation.md` 的「归属表」），并在 PR 模板的「影响的文档」一栏写明。

## CI

`.github/workflows/ci.yml` 在每次推送和 PR 时运行：

- `test`：gofmt、三个平台（linux、darwin、windows）的 `go vet`、`go test ./...`（包括文档检查）。
- `local-side`：在真实的 Windows 和 macOS 上测试电脑一侧（Git Bash、Job Object、Claude 式的命令、转发的 git 调用）。
- `shellcheck`：所有 shell 脚本。
- `integration`：在 runner 上构建镜像，运行端到端测试。

## 镜像

`.github/workflows/image.yml` 在推送 `v*` 标签时构建服务端镜像 `ghcr.io/notdesigned/remote-cc-server` 和出口主机镜像 `ghcr.io/notdesigned/remote-cc-egress`（amd64 和 arm64），标签是版本号和 `latest`。也可以在 Actions 页面手动运行，用 `claude_version` 指定打包的 Claude Code 版本（默认 `stable`）。

本地构建：`docker build deploy`。会临时占用约 2GB 的构建缓存，磁盘紧张的机器不要在本地构建，也不要在正在提供服务的服务器上构建或测试。

## 发布

推送 `v*` 标签后，`.github/workflows/release.yml` 跑测试、构建各平台的程序并上传到 GitHub Release（含 `SHA256SUMS` 和第三方许可证声明），`image.yml` 构建镜像。发布 PR 要逐项勾选：

- [ ] `CHANGELOG.md` 里「未发布」的标题改为版本号和日期，和要推送的 git 标签一致（程序的版本号由 `scripts/build.sh` 取自标签，`remote-cc version` 显示它）。
- [ ] 每个版本的「新增 / 变更 / 移除 / 升级步骤」四节齐全，没有内容的写「无」；需要更新服务器的，「升级步骤」写明 `remote-cc server update`。
- [ ] 和 RCB 的接口有变化时，CHANGELOG 写明「需要 RCB ≥ x」，并和 RCB 一侧核对。
- [ ] `docs/documentation.md`「对外锚点」里的锚点都还在；改名或删除的写进 CHANGELOG。
- [ ] 文档里指向 RCB 的链接仍然有效。
- [ ] README 的「上手」照原文重跑一遍（新服务器上 `server setup`、`account login`、启动会话；另一台电脑 `device join`）。
- [ ] CI 全部通过，包括 `go test ./internal/docs`。
- [ ] 标签推送后，release 里有全部平台的程序，两个镜像都已发布；`remote-cc update` 能换到新版本。
