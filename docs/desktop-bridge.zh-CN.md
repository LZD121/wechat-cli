# 桌面微信只读 TUI 桥接方案

目标：不伪装微信协议，让**官方微信桌面版**保持登录，由一个本地桥接进程读取会话和消息，TUI 通过受控 API 展示。第一阶段只读，确认稳定后再加人工节奏的发送。

## 1. 架构

```text
Windows 主机（官方微信已登录，交互桌面保持解锁）
┌──────────────────────────────────────────────┐
│  Weixin.exe                                  │
│    ├─ 本地 SQLCipher 数据库（消息/会话/联系人）│
│    └─ 自绘 UI（发送时由 UIA/OCR/剪切板驱动）  │
│                                              │
│  bridge（127.0.0.1:8765，Bearer token）       │
│    ├─ 读取：wechatauto-replica / 本地 DB      │
│    └─ 写入：UIA + OCR + SendInput（第二阶段） │
└──────────────────────┬───────────────────────┘
                       │ HTTP/JSON，SSH 或 Tailscale 隧道
┌──────────────────────▼───────────────────────┐
│ TUI（Go/Bubble Tea 或独立前端）               │
│  会话列表 / 消息流 / 搜索 / 草稿              │
│  第一阶段禁止发送；第二阶段人工发送           │
└──────────────────────────────────────────────┘
```

桥接进程不实现 MMTLS、不登录、不保存微信密码，也不替代官方客户端。它只是官方客户端旁边的一个本地读取器/操作器。

## 2. 为什么不是纯 UIA

- 微信 4.x 改成自绘 UI，传统 UIA 树基本消失；纯 `wxauto` 路线主要适用于 3.9.x。
- 4.x 的可行读取路径是本地 SQLCipher 数据库解密；发送路径是 UIA + OCR + 剪切板，部分实现还需要在 `Weixin.dll` 中翻一个 Qt 无障碍开关。
- 因此设计成**可替换的读取适配器**：
  - 第一阶段：`wechatauto-replica` 的 DB 读取。
  - 第二阶段：UIA/OCR 发送。
  - 若库失效，可在适配器后面替换，不影响 TUI 协议。

## 3. API v1

所有接口只监听 `127.0.0.1`，要求：

```http
Authorization: Bearer <token>
```

默认 `read_only=true`；发送接口在配置开启前返回 `403`。

### `GET /v1/health`

```json
{
  "ok": true,
  "bridge_version": "0.1.0",
  "wechat_version": "4.2.0.52",
  "account": {"id": "wxid_xxx", "name": "昵称"},
  "read_only": true,
  "db": {"status": "ok", "decrypted_at": 1770000000},
  "ui": {"status": "ok", "locked": false},
  "warnings": []
}
```

### `GET /v1/chats?limit=100`

```json
{
  "chats": [
    {
      "id": "wxid_alice",
      "name": "Alice",
      "kind": "dm",
      "unread": 2,
      "pinned": false,
      "last": {
        "id": "123456789",
        "timestamp": 1770000000,
        "sender": "Alice",
        "preview": "晚上一起吃饭吗",
        "type": "text"
      }
    }
  ],
  "next_cursor": null
}
```

`kind`：`dm` / `group`（`@chatroom`）/ `official`（`gh_`）。公众号默认不出现在 TUI 列表，与当前过滤规则一致。

### `GET /v1/messages?chat_id=...&limit=50&before=...`

```json
{
  "chat_id": "wxid_alice",
  "messages": [
    {
      "id": "123456789",
      "chat_id": "wxid_alice",
      "sender_id": "wxid_alice",
      "sender_name": "Alice",
      "direction": "in",
      "type": "text",
      "text": "晚上一起吃饭吗",
      "timestamp": 1770000000,
      "sequence": 0
    }
  ],
  "next_before": "123456780"
}
```

`type`：`text` / `image` / `file` / `voice` / `video` / `sticker` / `system` / `unknown`。附件第一阶段只返回预览文本，不下载媒体。

### `GET /v1/sync?since=...`（可选，第二阶段）

用于增量事件；第一阶段 TUI 可以每 2 秒轮询 `/v1/chats` 和当前会话的 `/v1/messages`。

### `POST /v1/send`（默认关闭，需 `--allow-send`）

```json
{
  "chat_id": "wxid_alice",
  "text": "好，七点见",
  "client_id": "uuid-v4"
}
```

```json
{
  "accepted": true,
  "client_id": "uuid-v4",
  "message_id": "123456800",
  "verified": true,
  "timestamp": 1770000100
}
```

规则：

- `client_id` 幂等：同一 ID 重复请求返回同一结果，不重复发送。
- 提交后不自动重试；`verified` 通过数据库回读确认。
- 失败返回 `accepted:false`，错误码见下。
- 发送前必须通过 `POST /v1/read` 或 TUI 显式确认当前会话。

### `POST /v1/read`（第二阶段）

```json
{"chat_id": "wxid_alice", "through_message_id": "123456789"}
```

### 错误格式

```json
{"error": {"code": "wechat_locked", "message": "桌面会话已锁定", "retryable": false}}
```

错误码：`unauthorized`、`read_only`、`wechat_not_running`、`wechat_locked`、`db_key_missing`、`version_unsupported`、`rate_limited`、`send_unverified`。

## 4. 与现有 TUI 的接口

现有 Go TUI 的 `chatBackend` 只有：

```go
sync(context.Context) chatSyncResult
send(context.Context, string, string) chatSendResult
```

新增 `desktopBackend`：

- `sync`：`GET /v1/chats` → 映射为 `Contacts`（Username/Nickname/Flags）；按选中会话拉 `/v1/messages`，映射为 `native.Message`。
- `send`：`POST /v1/send`；桥接默认 `read_only`，加 `--allow-send` 后启用；TUI 按 `health.read_only` 决定是否允许输入发送。
- 复用现有会话列表、消息流、草稿、搜索和公众号过滤；不改 UI 主循环。
- 启动时不走 `nativeTUI` 的微信账号状态检查；用 `--backend desktop` 或独立 `wechat-desktop-tui` 子命令，桥接地址和 token 从配置读取。

## 5. 安全边界

桥接进程拥有账号的完整消息读取能力，按“高权限本地服务”设计：

- 默认只绑定 `127.0.0.1`；远程使用 SSH 端口转发或 Tailscale，不直接暴露公网。
- token 首次生成，保存到用户配置目录；不写日志、不进命令行历史。
- 默认 `read_only=true`；发送需要显式配置和单独 token/开关。
- 不记录消息正文到日志；只记录错误码、耗时、消息 ID 哈希。
- 解密缓存放在用户目录，退出时清理；不进入云盘同步。
- 不在服务会话/锁屏状态运行 UI 自动化；健康检查返回 `wechat_locked`，TUI 只读 DB 时降级。

## 6. 部署前置

当前这台机器检查结果：

- 未检测到官方 `Weixin.exe` / `WeChat.exe`，也没有 `xwechat_files` 数据目录；需要先安装官方微信 4.2.x 并登录。
- 当前 Python 是 3.14.9；`wechatauto-replica` 验证在 Python 3.9–3.12，建议单独安装 Python 3.12 并用 venv，不影响系统 Python。
- 目标微信版本建议固定。`wechatauto-replica` 当前验证到 4.2.0.13；winget 提供 4.2.0.52，DB 读取大概率兼容，但 UIA 发送路径必须重新验证。

推荐部署：

1. 官方微信 4.2.x 登录，保持桌面解锁。
2. Python 3.12 venv 安装 `wechatauto-replica`。
3. 运行 `wechatauto doctor`，确认账号、DB key、图片 key、UIA 状态。
4. 跑只读桥接，先用“文件传输助手”验证会话和消息。
5. 再接 TUI；发送最后开启。

## 7. 分阶段验收

| 阶段 | 验收 |
|---|---|
| P0 环境 | ✅ `wechatauto doctor` 识别账号，19/19 数据库可解密，图片密钥和 UIA 树可用 |
| P1 只读桥 | ✅ `/v1/health`、`/v1/chats`、`/v1/messages` 已用真实账号验证；公众号和服务号按 `verify_flag` 分类 |
| P2 只读 TUI | ✅ TUI 显示会话/消息，公众号默认隐藏，`readOnly` 拦截发送 |
| P3 人工发送 | ✅ 文本发送经 DB 回读确认；重复 `client_id` 返回同一结果且不重复发送；失败不自动重试 |
| P4 可选 | 附件下载、图片预览、已读同步；每项单独评估风险 |

验证日期：2026-11-12。只读链路实测 39 个会话、15 个普通会话展示；发送实测到文件传输助手，DB 回读命中，重放同一 `client_id` 未产生重复消息。

## 8. 风险与不承诺

- 官方客户端路线去掉了“伪造设备/协议指纹”的风险，但不是无风险：自动化行为、读取进程内存中的 DB key、UIA 开关写入、频繁操作仍可能触发风控或违反用户协议。
- 微信版本更新会破坏 OCR 坐标、UIA 类名或 DB 布局；必须锁定版本并保留回退。
- 不承诺“零封号”“稳定长期可用”；这是个人辅助工具，不是官方 API。
- 不实现群发、自动回复、批量加好友、公网暴露、多账号托管。

## 9. 依赖与替代

- 读取：`wechatauto-replica`（Python，MIT）——DB 解密 + 媒体 + UIA 发送；当前维护者时间有限，需锁定版本。
- 纯 UIA：`wxauto`/`wxautox4`——只适合部分 3.9.x/4.0.5，4.x 自绘 UI 下不可靠。
- 备选：自己实现只读 DB 解密适配器；工作量更大，但可去掉第三方依赖。
- 现有 `wechat-cli` 原生协议后端保留，作为低依赖备选；两条路线不要混用同一账号登录状态。
