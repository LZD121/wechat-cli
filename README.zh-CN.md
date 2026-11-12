# wechat-cli 中文使用说明（个人使用）

本仓库是 [kevmo314/wechat-cli](https://github.com/kevmo314/wechat-cli) 的本地克隆，用于在终端里收发个人微信消息。它直接实现微信原生协议，不需要安装或运行微信桌面版，也不需要额外 bridge 服务。

> **风险提示（先读）**
>
> 这是非腾讯官方客户端。微信个人号没有公开 API，本项目通过逆向协议伪装成官方设备登录，违反微信用户协议，存在账号被强制下线、限制登录或封禁的风险。项目仍属实验性：协议可能随时失效，凭据在本地明文保存。建议先用小号验证，不要用于群发、营销或高频自动化。

## 1. 功能范围

- TUI：会话列表、消息流、未读计数、置顶、搜索、回复、引用、链接、翻译、附件下载。
- CLI：登录、同步、历史、联系人、发文本、等待新消息、下载附件、翻译、标记已读。
- 支持文本、图片/文件下载、引用、链接、已读状态；可显示收到的表情，群消息会尽量解析群成员显示名。
- 默认隐藏以 `gh_` 开头的公众号/服务号；在搜索框输入名称时仍会显示，选中后也会保留在当前列表。
- 尚未实现：语音/视频下载、媒体上传、加/删好友、完整历史迁移、新设备验证挑战。

## 2. 环境与构建

需要 Go 1.25 或更新版本，以及 Git。Windows、macOS、Linux 均可构建。

```powershell
git clone https://github.com/kevmo314/wechat-cli.git
cd wechat-cli
go build -o bin\wechat.exe .
```

Linux/macOS：

```sh
go build -o bin/wechat .
```

## 3. 安装（Windows，可选）

把二进制放到用户目录并加入用户 PATH：

```powershell
$dir = "$env:LOCALAPPDATA\Programs\wechat-cli"
New-Item -ItemType Directory -Force $dir | Out-Null
Copy-Item .\bin\wechat.exe "$dir\wechat.exe" -Force
$userPath = [Environment]::GetEnvironmentVariable("Path", "User")
if ($userPath -notlike "*$dir*") {
    [Environment]::SetEnvironmentVariable("Path", "$userPath;$dir", "User")
}
```

重新打开终端后验证：

```powershell
wechat version
wechat status
```

`status` 未登录时应输出类似：

```json
{"backend":"native","logged_in":false}
```

## 4. 登录

推荐使用 `tablet` profile。作者在测试账号上观察到平板登录后其他客户端仍保持在线，但不保证所有账号都如此。

### cmd.exe

```cmd
wechat --config-dir "%APPDATA%\wechat-tablet" --profile tablet login --qr "%USERPROFILE%\Desktop\wechat-login.png"
```

### PowerShell

```powershell
wechat --config-dir "$env:APPDATA\wechat-tablet" --profile tablet login --qr "$env:USERPROFILE\Desktop\wechat-login.png"
```

> `cmd.exe` 不展开 `$env:...`，会报 `The filename, directory name, or volume label syntax is incorrect.`，必须用 `%APPDATA%`。

登录过程：

1. 终端显示二维码，同时把 PNG 保存到 `--qr` 指定的位置。
2. 用手机微信扫码，在手机上确认登录。
3. 按代码实现，登录请求携带的设备名是 `WeChat CLI`，手机确认页通常会显示这个名字。
4. 命令在认证成功后退出，凭据写入配置目录。

说明：

- 默认登录超时 8 分钟；`--resume` 只接受 15 分钟内创建的未完成尝试。
- 全局参数（`--config-dir`、`--profile`）必须写在子命令前面。
- 不同 profile 使用不同配置目录，避免凭据混用。

| Profile | 行为 | 建议 |
| --- | --- | --- |
| `tablet` | 伪装 Android 平板，测试中与手机/其他客户端共存 | 首选 |
| `desktop` | 伪装 Linux 桌面客户端，可能顶掉另一台电脑的桌面会话 | 谨慎 |
| `phone` | 伪装 Android 手机，可能替换你的手机会话；完整认证未验证 | 不要用主号 |

## 5. TUI

启动：

```cmd
wechat --config-dir "%APPDATA%\wechat-tablet" --profile tablet
```

离线浏览本地历史：

```cmd
wechat --config-dir "%APPDATA%\wechat-tablet" --profile tablet tui --offline
```

| 操作 | 按键 |
| --- | --- |
| 切换会话 | Up / Down |
| 滚动消息 | PgUp / PgDn |
| 发送 | Enter 或 Ctrl+S |
| 换行 | Alt+Enter 或 Ctrl+J |
| 搜索 / 新建会话 | Ctrl+K / Ctrl+N |
| 选中消息 | 鼠标点击 |
| 回复选中消息（默认最近一条收到的消息） | Ctrl+R 或右键 |
| 取消回复 | Ctrl+X |
| 查看引用原文 | 点击引用预览 |
| 打开链接 | 点击链接或分享链接消息 |
| 翻译 / 隐藏翻译 | Ctrl+T |
| 下载附件 / 查看图片 | Ctrl+O，然后选择并按 Enter |
| 切换设备 profile | Ctrl+P |
| 联系人页 | Alt+C，或 Ctrl+G 再按 C |
| 消息详情 | Ctrl+E |
| 刷新 / 重试同步 | Alt+S |
| 恢复发送失败的草稿 | Ctrl+Y |
| 帮助 / 退出 | Ctrl+G / Ctrl+Q |

其他：

- 翻译使用微信服务端接口，不需要单独的翻译 API key；默认英文，可通过 `tui --language zh_CN` 改为中文。
- 图片以彩色终端块显示；图片和文件可下载，语音/视频下载和媒体上传未实现。
- 会话草稿按会话保存；发送失败可恢复，但中断的发送不会自动重试，因为可能已送达。
- 联系人页支持按名称、备注、别名或 ID 搜索；`Ctrl+R` 修改备注，`Ctrl+P` 置顶/取消置顶，`Esc` 返回草稿。

## 6. 常用 CLI 命令

以下命令均需带上相同的配置目录；建议同时显式指定 profile。

```cmd
wechat --config-dir "%APPDATA%\wechat-tablet" --profile tablet status
wechat --config-dir "%APPDATA%\wechat-tablet" --profile tablet read --json
wechat --config-dir "%APPDATA%\wechat-tablet" --profile tablet read --follow
wechat --config-dir "%APPDATA%\wechat-tablet" --profile tablet history --limit 50
wechat --config-dir "%APPDATA%\wechat-tablet" --profile tablet contacts
```

发送与回复（`RECIPIENT_ID` / `MESSAGE_ID` 来自 `contacts`、`read --json` 或 `history`）：

```cmd
wechat --config-dir "%APPDATA%\wechat-tablet" --profile tablet send --to RECIPIENT_ID --text "Hello"
wechat --config-dir "%APPDATA%\wechat-tablet" --profile tablet send --to RECIPIENT_ID --text "Reply" --reply-to MESSAGE_ID
```

等待新消息：

```cmd
wechat --config-dir "%APPDATA%\wechat-tablet" --profile tablet wait --timeout 60s --json
wechat --config-dir "%APPDATA%\wechat-tablet" --profile tablet wait --from RECIPIENT_ID --json
```

附件、翻译、已读：

```cmd
wechat --config-dir "%APPDATA%\wechat-tablet" --profile tablet attachment --id MESSAGE_ID --preview
wechat --config-dir "%APPDATA%\wechat-tablet" --profile tablet attachment --id MESSAGE_ID --output "%USERPROFILE%\Desktop\file.bin"
wechat --config-dir "%APPDATA%\wechat-tablet" --profile tablet translate --text "你好，世界" --lang en
wechat --config-dir "%APPDATA%\wechat-tablet" --profile tablet translate --id MESSAGE_ID --lang en
wechat --config-dir "%APPDATA%\wechat-tablet" --profile tablet mark-read --to RECIPIENT_ID
```

每个命令都支持 `--help`：

```cmd
wechat send --help
```

## 7. 配置、历史与隐私

- Windows 默认配置目录：`%APPDATA%\wechat-cli`；本文示例显式使用 `%APPDATA%\wechat-tablet`。
- 原生账号状态在 `native\state.json`，包含登录凭据和密钥，**明文保存**。同步批次归档在 `native\batches\`。
- 不要把配置目录、同步归档或 `wechat-login.png` 分享给别人，也不要放进云盘同步。
- `history` 只读取本客户端自己同步下来的归档；新设备登录不保证获得完整历史，通常只能同步到近期消息。
- 同一配置目录同时只允许一个 `login` / `read` / `logout` 进程；发送进程可以与读进程并行。
- `logout` 只删除本地凭据和游标，保留归档；它不会远程吊销设备授权，需要在手机微信的“登录设备管理”里手动移除。
- 若登录时顶掉了桌面会话，`logout` 也不会恢复原来的桌面会话。

## 8. 常见问题

**在 cmd 里报 `$env:APPDATA` 路径错误**

cmd 不展开 PowerShell 变量。改用：

```cmd
wechat --config-dir "%APPDATA%\wechat-tablet" --profile tablet login
```

**提示 `already logged in natively`**

该配置目录已有账号。用 `logout` 退出，或换一个 `--config-dir`：

```cmd
wechat --config-dir "%APPDATA%\wechat-tablet" logout
```

**二维码过期 / 登录超时**

重新执行 `login`。`--resume` 只适用于 15 分钟内保存的尝试，过期后必须重新扫码。

**提示 profile 不一致**

登录尝试保存了 profile，续登时必须使用同一个 `--profile`，或换配置目录重新登录。

**重新编译后需要重新登录吗？**

不需要。账号凭据和同步游标保存在 `--config-dir` 指向的配置目录里，和二进制无关；重新 build 并替换 `wechat.exe` 后，继续使用同一个 `--config-dir` 和 `--profile` 即可。只有执行 `logout`、删除配置目录或服务端会话过期时才需要重新扫码。

**登录后被要求新设备验证**

项目会检测手机/LAN 验证挑战，但尚未实现完整处理。按手机提示操作；若无法通过，换回官方客户端或使用其他配置目录重试。

**历史消息不完整**

这是预期行为。客户端只能拿到新设备会话同步到的消息，不保证能下载全部历史，也不支持导入官方桌面版数据库。

**TUI 画面错乱**

使用 Windows Terminal，并确认终端为 UTF-8。TUI 依赖终端尺寸和 ANSI 能力；过小的窗口会压缩布局。

## 9. 风险与边界

- **账号风险**：微信禁止非官方客户端。服务端可看到新设备类型、客户端版本、请求校验字段和行为模式，存在风控处置的可能。没有公开的封号概率，不能因为“没有注入 DLL”就认为安全。
- **协议风险**：项目通过零 notice checksum、固定设备画像等方式兼容服务端；这些是非标准行为，可能随微信更新失效。
- **数据风险**：登录凭据明文落盘；第三方构建可能被篡改，建议自己从源码构建。
- **稳定性**：项目实验性较强，session 续期、重连、验证挑战、媒体上传等仍不完整。已知上游问题：会话过期（`-13`）不会自动续期；每次全新 QR 登录会生成新的设备 ID，手机可能反复提示新设备。
- **合规**：仅用于个人账号的日常收发，不要用于营销、群发、批量加好友或其他自动化滥用。

## 10. 卸载

```cmd
wechat --config-dir "%APPDATA%\wechat-tablet" logout
```

然后删除二进制和配置目录：

```powershell
Remove-Item "$env:LOCALAPPDATA\Programs\wechat-cli" -Recurse -Force
Remove-Item "$env:APPDATA\wechat-tablet" -Recurse -Force
# 如果用过默认配置目录，一并删除
Remove-Item "$env:APPDATA\wechat-cli" -Recurse -Force
```

如果不再需要，从用户 PATH 中移除 `%LOCALAPPDATA%\Programs\wechat-cli`。最后在手机微信的“登录设备管理”中移除 `WeChat CLI`。

## 11. 桌面微信 TUI（实验）

官方微信 4.x + 本地桥接，不伪装协议。默认只读；启动桥时显式加 `--allow-send` 才会开启人工发送。设计见 [`docs/desktop-bridge.zh-CN.md`](docs/desktop-bridge.zh-CN.md)。

前置：

- 官方微信 4.2.x 已登录，桌面保持解锁。
- Python 3.12 venv 已安装 `wechatauto-replica`。

启动桥（默认只读，仅监听 `127.0.0.1:8765`）：

```cmd
.venv-bridge\Scripts\python.exe bridge\wechat_bridge.py --port 8765
```

开启人工发送：

```cmd
.venv-bridge\Scripts\python.exe bridge\wechat_bridge.py --port 8765 --allow-send
```

发送会驱动官方微信窗口；桥接限制最小 2 秒间隔，按 `client_id` 幂等，发送后用本地数据库回读确认。结果不确定时不会自动重试。

token 在 `%LOCALAPPDATA%\wechat-bridge\token`，不要分享。

启动 TUI（另开终端）：

```cmd
wechat desktop --bridge-url http://127.0.0.1:8765
```

默认读取 token 文件。远程使用时用 SSH 端口转发或 Tailscale，不要把桥接到公网。

当前限制：读取依赖本地 SQLCipher 数据库和 `wechatauto-replica`，微信版本升级可能失效；公众号/服务号默认隐藏；附件只显示预览，不下载媒体；发送是 UI 自动化，失败时可能需要人工确认微信窗口状态。

## 12. 上游与许可

- 上游：<https://github.com/kevmo314/wechat-cli>
- 协议调研文档：[`docs/native-protocol.md`](docs/native-protocol.md)、[`docs/tablet-protocol.md`](docs/tablet-protocol.md)
- 许可：仓库当前没有 `LICENSE` 文件，GitHub 也未识别许可证；默认版权保留，个人使用、修改或分发前请自行确认授权。
- 维护状态：上游最后一次代码提交为 2026-10-24；截至 2026-11-12 没有新提交或 release，社区 issue/PR 未见维护者回复。相关项目 [`thisnick/agent-wechat`](https://github.com/thisnick/agent-wechat) 仍在活跃开发。

本项目与腾讯无关，不是官方产品。
