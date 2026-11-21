# remote-cc (rcc)

本机不用安装 Claude Code，也能用 Claude Code 写本机的代码。

```
A (本机，运行 rcc)  ──ssh──►  B (远程，装有 claude)
```

`rcc` 从 A ssh 到 B，在 B 上启动 Claude Code，界面显示在 A 的终端里：

- **命令**：通过 `CLAUDE_CODE_SHELL_PREFIX` 加反向 unix socket，Bash 工具的命令在 A 上执行。
- **文件**：通过反向 sshfs（`sshfs -o slave` 加 rcc 内置的 SFTP 服务），把 A 的项目目录挂载到 B 上相同的绝对路径，
  Read/Write/Edit/Glob/Grep 实际读写的是 A 上的文件。

详细设计见 [docs/DESIGN.md](docs/DESIGN.md)。

## 状态

- [x] M1 命令通道（`rcc serve` / `rcc-shim`），已和真实的 Claude Code 端到端验证
- [ ] M2 文件通道（内置 SFTP 加 `sshfs -o slave`）
- [ ] M3 一键启动 `rcc user@B`
- [ ] M4 增强

## 手动试用 M1

```sh
go build -o rcc ./cmd/rcc            # A 和 B 各一份（可以交叉编译）
ln -s rcc rcc-shim                   # B 上

# A：
rcc serve --listen 127.0.0.1:7788 --token SECRET
ssh -R /tmp/rcc.sock:127.0.0.1:7788 user@B

# B（在上面那个 ssh 会话里，项目目录要和 A 上路径相同，M2 之前需要自己同步）：
cd /same/path/as/on/A
RCC_SOCK=/tmp/rcc.sock RCC_TOKEN=SECRET CLAUDE_CODE_SHELL_PREFIX=$PWD/rcc-shim claude
```

## 开发

```sh
go test ./...                                   # 包含 shim↔agent 端到端测试
GOOS=windows go build ./cmd/rcc                 # 交叉编译检查
```

## 预期用法

```sh
rcc user@B            # 在当前目录启动
rcc user@B ~/proj     # 指定项目目录
```

## 依赖

- A：`ssh` 客户端、`bash`
- B：`claude`、`sshfs`、FUSE
