# vps-tool 主控 MVP

这是 vps-tool 的 Python FastAPI 主控 MVP。它提供管理员 Session/Cookie/CSRF 认证、SQLite 持久化、节点注册与 Agent 凭据生命周期、固定 Action 请求、Agent WebSocket gateway，以及带数据库租约的单实例调度骨架。

当前实现只向 Agent 下发固定 Action 名称和结构化参数，不提供任意 Shell、脚本路径、命令字符串或文件写入接口。真正的 WARP/3x-ui 执行器属于 Agent 侧；主控只负责鉴权、校验、排队、投递和记录结果。

## 启动

在 `server` 目录执行：

```powershell
$env:VPS_TOOL_ADMIN_USER = "admin"
$env:VPS_TOOL_ADMIN_PASSWORD = "use-a-password-at-least-12-bytes"
$env:VPS_TOOL_COOKIE_SECURE = "false" # 仅本地 HTTP 开发；生产环境保持 true
$env:VPS_TOOL_DB_PATH = ".\data\vps-tool.sqlite3"
python -m uvicorn app.main:app --host 127.0.0.1 --port 8000 --workers 1
```

生产环境应由 Nginx/Caddy 提供 HTTPS/WSS，并使用单个 Uvicorn worker。若未配置 `VPS_TOOL_ADMIN_USER` 或 `VPS_TOOL_ADMIN_PASSWORD`，应用会在导入/启动时明确失败；密码长度必须为 12 至 72 个 UTF-8 字节，密码不会写入日志。

首次启动会创建 SQLite 数据库并执行版本化迁移。每个连接自动启用：

- `PRAGMA journal_mode = WAL`
- `PRAGMA foreign_keys = ON`
- `PRAGMA busy_timeout = 5000`

## 环境变量

| 变量 | 默认值 | 用途 |
| --- | --- | --- |
| `VPS_TOOL_ADMIN_USER` | 无 | 首次启动创建的唯一管理员用户名 |
| `VPS_TOOL_ADMIN_PASSWORD` | 无 | 首次启动创建的管理员密码；12 至 72 UTF-8 字节 |
| `VPS_TOOL_DB_PATH` | `./data/vps-tool.sqlite3` | SQLite 文件路径 |
| `VPS_TOOL_COOKIE_SECURE` | `true` | Session Cookie 的 `Secure` 属性；本地明文开发可设为 `false` |
| `VPS_TOOL_SESSION_COOKIE` | `vps_tool_session` | Session Cookie 名称 |
| `VPS_TOOL_SESSION_TTL_SECONDS` | `28800` | Session 有效期 |
| `VPS_TOOL_ENROLLMENT_TTL` | `600` | 一次性注册 Token 有效期 |
| `VPS_TOOL_ACTION_TTL` | `180` | Action 请求截止时间 |
| `VPS_TOOL_HEARTBEAT_TIMEOUT` | `90` | Agent 在线判定超时 |
| `VPS_TOOL_SCHEDULER_INTERVAL` | `5` | 调度轮询间隔 |

## 管理员 API

除登录、健康检查外，接口需要 Session Cookie。所有改变状态的接口还需要登录响应中的 `csrf_token`，放在 `X-CSRF-Token` 请求头中。

### 认证

- `POST /api/auth/login`：JSON `{ "username": "...", "password": "..." }`；返回 Session Cookie 和 CSRF Token。
- `GET /api/auth/me`：查看当前管理员。
- `POST /api/auth/logout`：需要 CSRF，销毁服务端 Session。

### 节点和凭据

- `GET /api/nodes`、`GET /api/nodes/{id}`：节点和在线状态。
- `POST /api/nodes`：创建节点，返回一次性 `registration_token`；Token 仅返回当前响应，不写入日志，默认 10 分钟有效。
- `PATCH /api/nodes/{id}`、`DELETE /api/nodes/{id}`：更新或软删除节点；删除会吊销凭据并停用相关任务。
- `POST /api/nodes/{id}/enrollment-token`：吊销旧的未使用 Token 并签发新的 Token。
- `POST /api/nodes/{id}/credentials/rotate`：吊销旧长期凭据并返回一次新的明文凭据；服务端只保存 SHA-256 哈希。
- `POST /api/nodes/{id}/credentials/revoke`：吊销长期凭据并断开当前连接。

Agent 通过 `ws://host/agent`（生产环境必须使用 `wss://`）发送第一条 `agent_hello`。一次性注册时 payload 使用 `node_id` 和 `registration_token`；已注册 Agent 可在 `Authorization: Bearer ...` 握手头或 hello payload 中使用长期 `credential`。长期凭据不放入 URL 查询参数。

### 固定 Action

`GET /api/actions` 返回固定 Action 目录和最近请求；`GET /api/action-requests/{request_id}` 查看单个请求；`GET /api/action-requests` 查看请求列表。

`POST /api/nodes/{id}/actions` 的请求格式：

```json
{
  "action": "change_ip",
  "parameters": {
    "max_attempts": 3,
    "timeout_seconds": 180
  },
  "request_id": "optional-client-id",
  "queue_if_offline": false
}
```

允许的 Action 只有：`get_status`、`get_ip`、`warp_on`、`warp_off`、`change_ip`、`restart_xui`。除 `change_ip` 外参数必须为空对象；`change_ip` 只接受 `max_attempts`（1 至 3）和 `timeout_seconds`（30 至 180）。未知字段、命令字符串和未知 Action 会被拒绝。

节点离线时，默认手动请求返回 `skipped_offline` 和 `node_offline`；明确设置 `queue_if_offline=true` 才会保留为 `queued`，Agent 重连后投递。同一节点同一时刻只能有一个状态变更 Action。

### Agent WebSocket 消息

所有消息使用以下信封：

```json
{
  "protocol_version": 1,
  "message_type": "heartbeat",
  "message_id": "unique-message-id",
  "sent_at": "2026-08-23T00:00:00Z",
  "payload": {}
}
```

主控接受 `agent_hello`、`heartbeat`、`status_report`、`command_ack`、`command_progress`、`command_result`。握手后主控会投递排队请求；断线时 `dispatched`、`accepted`、`running` 请求标记为 `unknown`，Agent 重连时可在 `agent_hello.payload.reconcile` 中提交最终 `command_result` 对账。

### 任务

- `GET /api/tasks`、`GET /api/tasks/{id}`：任务和 task run。
- `POST /api/tasks`：支持 `daily`、`weekly`、`cron`，每个任务包含 IANA 时区、节点列表、固定状态变更 Action、重试配置。
- `PATCH /api/tasks/{id}`：修改任务或启停。
- `DELETE /api/tasks/{id}`：软删除并停用任务。

调度器在 FastAPI lifespan 中启动，使用 SQLite `scheduler_leases` 租约，避免同一数据库由多个进程重复领取到期任务。部署仍应使用单个 Uvicorn worker；租约是防重复的 MVP 保护，不是多主控高可用选主实现。

## 验证

在项目根目录执行：

```powershell
python -m compileall server
```

依赖见 `requirements.txt`。`croniter` 用于完整 Cron 解析；未安装它时，MVP 会退化到受限的五字段 Cron 解析，以便核心服务仍能启动。

## 集成目录

- `../web`：静态管理工作台。主控启动后通过 `/` 提供，无需单独构建。
- `../agent`：低内存 VPS 上运行的 Go Agent。Agent 只执行固定 Action，并通过 WSS 与主控通信。

Agent 的环境变量和一次性运行方式见 `../agent/README.md`。生产部署时请把主控放在 HTTPS 反向代理后，并保持单个 Uvicorn worker。
