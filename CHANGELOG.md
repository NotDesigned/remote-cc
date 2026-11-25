> 读者：升级 remote-cc 的使用者和管理员

# 更新日志

## 0.7.0（未发布）

### 新增

- `remote-cc help 命令`（或任何命令后加 `-h`）显示这条命令的全部选项；`remote-cc help` 是按用途分组的总览。
- 只输入一组命令的名词（`remote-cc server`、`server user`、`account`、`device`、`rcb`）时，列出这一组的子命令。

### 变更

- 帮助里区分两种主机：`HOST` 是你的容器（`server setup` 或 `join` 写好的 ssh 别名），`SERVER` 是 Docker 宿主机（root 或 sudo 账号）。
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

- 把脚本、别名和文档里的旧命令名按上表换成新名字。服务端不需要操作。
- 和 RCB 一起升级：RCB 配置卡片上的首次登录提示从下一个 RCB 版本起才是 `account login --rcb`，旧版 RCB 提示的 `login --rcb` 在本版本会报错（报错里会给出新名字）。
