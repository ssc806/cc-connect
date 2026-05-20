# 通过友空间和 yms-rca 对话

本文说明如何安装并配置 cc-connect，让友空间（YouZone）消息转发到本机的 `yms-rca`，从而在友空间里和 yms-rca 对话。

## 工作方式

```text
友空间用户
  ↓
友空间机器人
  ↓  WebSocket / xmpp，不需要公网 webhook
cc-connect youzone platform
  ↓
cc-connect engine
  ↓  yms-rca rpc --no-color
yms-rca
```

cc-connect 的 youzone 平台负责收发友空间消息；`yms-rca` agent 负责启动本机 `yms-rca rpc --no-color` 子进程。用户在友空间发送普通文本时，会进入对应的 yms-rca 会话；发送 `/connect <profile>` 时，会切换到 yms-rca 的连接配置。

## 前置条件

- 一台能长期运行 cc-connect 的机器，macOS、Linux、Windows 均可。
- 已获得友空间机器人配置：`access_token（yht_access_token）`、`tenant_id`，以及 `robot_id`。
- 已安装并配置好 `yms-rca` CLI，且 `yms-rca` 在 `PATH` 中可执行，或知道它的绝对路径。
- 已准备 yms-rca 连接配置，例如 `~/.yms-rca/connections/yms-dev.yaml`。

## 1. 安装 cc-connect

```bash
git clone https://github.com/chenhg5/cc-connect.git
cd cc-connect
make build AGENTS=yms-rca PLATFORMS_INCLUDE=youzone
./cc-connect --version
```

如果你使用源码构建并希望包含所有 agent 与 platform，也可以直接执行：

```bash
make build
```

## 2. 检查 yms-rca

先确认 cc-connect 运行用户能直接调用 `yms-rca`：

```bash
which yms-rca
yms-rca --version
```

yms-rca 的连接配置默认位于：

```text
~/.yms-rca/connections/<profile>.yaml
```

一个常见的连接配置类似：

```yaml
mcp:
  endpoint: http://iuapyys.yyuap.com/mcp
  token_env: IUAPYYS_MCP_TOKEN
```

`token_env` 写的是环境变量名，不是 token 值。真正的 token 必须存在于 cc-connect 进程环境中：

```bash
export IUAPYYS_MCP_TOKEN='<real token>'
```

## 3. 创建 config.toml

推荐放在全局配置目录：

```bash
mkdir -p ~/.cc-connect
cp config.example.toml ~/.cc-connect/config.toml
```

也可以手工创建一个最小配置：

```toml
language = "zh"

[log]
level = "info"

[[projects]]
name = "youzone-yms"

# 可选：允许哪些友空间用户执行 /shell、/restart、/upgrade 等高权限命令。
# 生产环境不要直接设为 "*"，建议填写管理员 sender ID。
# admin_from = "user-1.esn.upesn"

[projects.agent]
type = "yms-rca"

[projects.agent.options]
work_dir = "/absolute/path/to/your/workspace"
cmd = "yms-rca"

# provider 与 model 必须同时设置；如果模型完全由 yms-rca profile 管理，可先不填。
# provider = "yonyou"
# model = "yonyou/MiniMax-M2.7-highspeed"

# 权限模式：
# default           高危操作询问用户
# dontAsk           高危操作自动拒绝
# bypassPermissions 高危操作自动批准
# yolo              自动批准所有工具调用
mode = "default"

# 推理强度：off | minimal | low | medium | high | xhigh
# thinking = "medium"

# 可选：覆盖 yms-rca 连接配置目录；默认 ~/.yms-rca/connections
# connections_dir = "/absolute/path/to/connections"

# 可选：高危确认超时，单位秒；默认 300
# confirm_timeout_secs = 300

[[projects.platforms]]
type = "youzone"

[projects.platforms.options]
base_url = "https://c2.yonyoucloud.com"
api_prefix = "/yonbip-ec-link"

access_token = "${YOUZONE_ACCESS_TOKEN}"
tenant_id = "${YOUZONE_TENANT_ID}"
robot_id = "your-youzone-robot-id"

# 允许哪些友空间 sender ID 使用该机器人。
# 首次联调可临时设为 "*"；上线后建议改为明确的 sender ID 列表。
allow_from = "*"

websocket_protocols = "xmpp"
heartbeat_mode = "xmpp-whitespace"
ping_interval = "25s"
reconnect_delays = "1s,3s,10s,30s"
```

## 4. 设置环境变量

配置里使用了 `${YOUZONE_ACCESS_TOKEN}`、`${YOUZONE_TENANT_ID}`。cc-connect 加载配置时会从当前进程环境展开它们：

```bash
export YOUZONE_ACCESS_TOKEN='<your yht_access_token>'
export YOUZONE_TENANT_ID='<your tenant id>'
export IUAPYYS_MCP_TOKEN='<real yms mcp token>'
```

如果运行环境配置了全局代理，而代理无法访问友空间域名，给友空间域名加 `NO_PROXY`：

```bash
export NO_PROXY=yonyoucloud.com,yyuap.com,localhost,127.0.0.1,::1
export no_proxy="$NO_PROXY"
```

## 5. 启动 cc-connect

前台启动：

```bash
cc-connect -config ~/.cc-connect/config.toml
```

看到类似日志表示配置已加载并开始连接：

```text
level=INFO msg="platform started" project=youzone-yms platform=youzone
level=INFO msg="cc-connect is running" projects=1
```

第一次联调时，建议保持前台运行并观察日志。收到友空间消息后，youzone 平台会打印入站摘要日志，其中包含 `sender`、`conversation`、`session` 等字段。确认 sender ID 后，把 `allow_from = "*"` 改成明确列表，例如：

```toml
allow_from = "user-1.esn.upesn,user-2.esn.upesn"
```

## 6. 安装为系统服务

确认前台启动可用后，再安装守护进程：

```bash
export YOUZONE_ACCESS_TOKEN='<your yht_access_token>'
export YOUZONE_TENANT_ID='<your tenant id>'
export IUAPYYS_MCP_TOKEN='<real yms mcp token>'

cc-connect daemon install --config ~/.cc-connect/config.toml --force
cc-connect daemon status
cc-connect daemon logs -f
```

`daemon install` 默认只捕获代理相关环境变量，不再扫描配置中的 `${ENV}` 占位符，也不再扫描 yms-rca profile 里 `mcp.token_env` 声明的环境变量。Token 请通过部署脚本或系统服务管理器注入到 cc-connect 进程环境。

安装命令保持为：

```bash
cc-connect daemon install --config ~/.cc-connect/config.toml --force
```

新增 yms-rca profile 或新增 `mcp.token_env` 之后，不需要为了捕获 secret 重新执行 `daemon install`；但部署脚本或服务管理器必须在用户选择该 profile 前注入对应环境变量。

只执行 `cc-connect daemon restart` 不会重新生成服务文件，也不会捕获新增环境变量。

## 7. 在友空间中对话

在友空间里向机器人发送：

```text
/connect yms-dev
```

其中 `yms-dev` 对应 `~/.yms-rca/connections/yms-dev.yaml`。连接成功后继续发送普通消息：

```text
帮我看一下当前项目有哪些待处理问题
```

cc-connect 会为不同的友空间会话生成独立 session key，格式类似：

```text
youzone:<conversation_id>:<sender_id>
```

同一个用户在同一个会话里会复用上下文；不同会话或不同用户会进入不同的 yms-rca 会话。

## 常见问题

### 启动时报 `youzone: access_token is required`

`access_token` 为空。检查环境变量是否已设置，或者临时把真实值直接写入 `config.toml` 验证：

```bash
env | grep YOUZONE
cc-connect -config ~/.cc-connect/config.toml
```

### 发送 `/connect yms-dev` 后提示缺少环境变量

yms-rca profile 的 `mcp.token_env` 指向的变量没有出现在 cc-connect 进程环境中。先导出变量，再重装服务：

```bash
export IUAPYYS_MCP_TOKEN='<real token>'
cc-connect daemon install --config ~/.cc-connect/config.toml --force
cc-connect daemon restart
```

### 收不到友空间消息

按顺序检查：

1. `cc-connect daemon logs -f` 是否有 `youzone: websocket connected`。
2. `access_token`、`tenant_id`、`robot_id` 是否属于同一租户和机器人。
3. `allow_from` 是否限制了当前 sender ID。
4. 临时启用前台运行，观察收到消息时日志里的 `sender` 与 `session` 字段。

### yms-rca 找不到

服务环境里的 `PATH` 和交互式 shell 可能不同。优先在配置里写绝对路径：

```toml
[projects.agent.options]
cmd = "/absolute/path/to/yms-rca"
```

然后重新安装服务：

```bash
cc-connect daemon install --config ~/.cc-connect/config.toml --force
```
