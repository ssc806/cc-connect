# 通过微信个人号和 yms-rca 对话

本文说明如何安装并配置 cc-connect，让微信个人号（Weixin / ilink）消息转发到本机的 `yms-rca`，从而在微信里和 yms-rca 对话。

## 工作方式

```text
微信用户
  ↓
微信个人号 ilink 机器人
  ↓  getUpdates 长轮询 / sendMessage，不需要公网 webhook
cc-connect weixin platform
  ↓
cc-connect engine
  ↓  yms-rca rpc --no-color
yms-rca
```

cc-connect 的 weixin 平台负责通过 ilink 网关收发微信消息；`yms-rca` agent 负责启动本机 `yms-rca rpc --no-color` 子进程。用户在微信里发送普通文本时，会进入对应的 yms-rca 会话；发送 `/connect <profile>` 时，会切换到 yms-rca 的连接配置。

> 注意：本文是微信个人号（`type = "weixin"`）通道，不是企业微信（`type = "wecom"`）。

## 前置条件

- 一台能长期运行 cc-connect 的机器，macOS、Linux、Windows 均可。
- 已准备微信个人号 ilink 机器人登录方式：可用手机微信扫码，或已有 ilink Bearer Token。
- 已安装并配置好 `yms-rca` CLI，且 `yms-rca` 在 `PATH` 中可执行，或知道它的绝对路径。
- 已准备 yms-rca 连接配置，例如 `~/.yms-rca/connections/yms-dev.yaml`。

## 1. 安装 cc-connect

```bash
git clone https://github.com/chenhg5/cc-connect.git
cd cc-connect
make build AGENTS=yms-rca PLATFORMS_INCLUDE=weixin
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
name = "weixin-yms"

# 可选：允许哪些微信用户执行 /shell、/restart、/upgrade 等高权限命令。
# 生产环境不要直接设为 "*"，建议填写管理员 ilink 用户 ID。
# admin_from = "user@im.wechat"

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
type = "weixin"

[projects.platforms.options]
# 推荐先留空或写占位值，然后用 `cc-connect weixin setup --project weixin-yms`
# 扫码写入真实 token、base_url 与 account_id。
token = "your-ilink-bot-bearer-token"

# 可选：ilink 网关地址；默认 https://ilinkai.weixin.qq.com
base_url = "https://ilinkai.weixin.qq.com"

# 可选：微信 CDN 根地址，用于图片、文件、视频、语音等媒体收发。
cdn_base_url = "https://novac2c.cdn.weixin.qq.com/c2c"

# 允许哪些微信 ilink 用户 ID 使用该机器人。
# 首次扫码时可先留空，并配合 --set-allow-from-empty 回填扫码用户；
# 上线后建议改为明确的用户 ID 列表。
allow_from = ""

# 多微信账号或多机器人时用于隔离本地状态目录。
account_id = "default"

# 可选：运营商要求 SKRouteTag 时填写。
# route_tag = ""

# 可选：长轮询超时，单位毫秒。
long_poll_timeout_ms = 35000
```

## 4. 登录或绑定微信个人号

如果你要扫码登录，在配置文件存在且项目名已写好后执行：

```bash
cc-connect weixin setup --config ~/.cc-connect/config.toml --project weixin-yms --set-allow-from-empty
```

终端会打印二维码或可复制 URL。用手机微信确认登录后，命令会把 ilink `token`、`base_url`、`account_id` 等写回 `config.toml`。`--set-allow-from-empty` 只会在 `allow_from` 为空时尝试写入扫码用户；如果当前是 `allow_from = "*"`，它会保留通配设置。联调成功后请确认 `allow_from` 是明确的用户 ID 列表。

如果你已有 ilink Bearer Token，可以绑定：

```bash
cc-connect weixin bind --config ~/.cc-connect/config.toml --project weixin-yms --token '<your ilink bearer token>'
```

如果网关不是默认地址，额外指定：

```bash
cc-connect weixin setup --config ~/.cc-connect/config.toml --project weixin-yms --api-url 'https://your-ilink-gateway.example'
```

## 5. 设置环境变量

yms-rca profile 里 `mcp.token_env` 声明的变量必须出现在 cc-connect 进程环境中：

```bash
export IUAPYYS_MCP_TOKEN='<real yms mcp token>'
```

如果你没有使用 `weixin setup` 把 token 写入配置，而是在 `config.toml` 中使用了 `${WEIXIN_ILINK_TOKEN}` 这类占位符，也需要导出对应变量：

```bash
export WEIXIN_ILINK_TOKEN='<your ilink bearer token>'
```

如果运行环境配置了全局代理，而代理无法访问 ilink 或微信 CDN，给相关域名加 `NO_PROXY`：

```bash
export NO_PROXY=ilinkai.weixin.qq.com,weixin.qq.com,qq.com,localhost,127.0.0.1,::1
export no_proxy="$NO_PROXY"
```

## 6. 启动 cc-connect

前台启动：

```bash
cc-connect -config ~/.cc-connect/config.toml
```

看到类似日志表示配置已加载并开始长轮询：

```text
level=INFO msg="platform started" project=weixin-yms platform=weixin
level=INFO msg="cc-connect is running" projects=1
```

第一次联调时，建议保持前台运行并用允许的微信账号先给机器人发一条消息。weixin 平台需要从入站消息中缓存 `context_token`，后续才能稳定回复该用户。

确认微信用户 ID 后，把 `allow_from` 改成明确列表，例如：

```toml
allow_from = "user1@im.wechat,user2@im.wechat"
```

## 7. 安装为系统服务

确认前台启动可用后，再安装守护进程：

```bash
export IUAPYYS_MCP_TOKEN='<real yms mcp token>'

cc-connect daemon install --config ~/.cc-connect/config.toml --force
cc-connect daemon status
cc-connect daemon logs -f
```

`daemon install` 会捕获配置中的 `${ENV}` 占位符，以及 yms-rca profile 里 `mcp.token_env` 声明的环境变量，并写入 launchd、systemd 或 Windows Task Scheduler 的服务文件。服务文件权限会按 owner-only 创建，但同一系统用户下运行的进程仍可能读取到这些值。

如果你不希望 cc-connect 捕获 secret，请改用系统服务管理器自己的密钥注入方式，并安装时加：

```bash
cc-connect daemon install --config ~/.cc-connect/config.toml --force --no-capture-secrets
```

新增 yms-rca profile 或新增 `mcp.token_env` 之后，需要重新执行：

```bash
cc-connect daemon install --config ~/.cc-connect/config.toml --force
```

只执行 `cc-connect daemon restart` 不会重新生成服务文件，也不会捕获新增环境变量。

## 8. 在微信中对话

在微信里向机器人发送：

```text
/connect yms-dev
```

其中 `yms-dev` 对应 `~/.yms-rca/connections/yms-dev.yaml`。连接成功后继续发送普通消息：

```text
帮我看一下当前项目有哪些待处理问题
```

cc-connect 会为不同的微信用户生成独立 session key，格式类似：

```text
weixin:dm:<user_id>
```

同一个微信用户会复用上下文；不同用户会进入不同的 yms-rca 会话。

## 常见问题

### 启动时报 `weixin: token is required`

`token` 为空。先执行扫码或绑定命令，或检查环境变量是否已设置：

```bash
cc-connect weixin setup --config ~/.cc-connect/config.toml --project weixin-yms
env | grep WEIXIN
cc-connect -config ~/.cc-connect/config.toml
```

### 启动后收不到微信消息

按顺序检查：

1. `cc-connect daemon logs -f` 是否有 `weixin: getUpdates failed` 或会话过期日志。
2. `token`、`base_url`、`route_tag` 是否与 ilink 网关要求一致。
3. `allow_from` 是否限制了当前微信 ilink 用户 ID。
4. 临时前台运行，并从微信端主动给机器人发一条普通文本消息。

### 能收到消息但无法回复

weixin 平台回复依赖 `context_token`。首次启动后必须先由微信用户给机器人发一条消息，平台会把 `context_token` 缓存在：

```text
<data_dir>/weixin/<project>/<account_id>/context_tokens.json
```

如果该文件不存在或没有对应用户记录，重新从微信端发一条消息后再试。

### 发送 `/connect yms-dev` 后提示缺少环境变量

yms-rca profile 的 `mcp.token_env` 指向的变量没有出现在 cc-connect 进程环境中。先导出变量，再重装服务：

```bash
export IUAPYYS_MCP_TOKEN='<real token>'
cc-connect daemon install --config ~/.cc-connect/config.toml --force
cc-connect daemon restart
```

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
