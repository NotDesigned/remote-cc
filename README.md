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

设计阶段，尚未实现。

## 预期用法

```sh
rcc user@B            # 在当前目录启动
rcc user@B ~/proj     # 指定项目目录
```

## 依赖

- A：`ssh` 客户端、`bash`
- B：`claude`、`sshfs`、FUSE
