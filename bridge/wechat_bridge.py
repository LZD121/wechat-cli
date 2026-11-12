#!/usr/bin/env python3
"""Local bridge between the official WeChat 4.x client and a TUI.

The bridge reads the logged-in client's local SQLCipher databases through
wechatauto-replica. It never logs in. It is read-only by default; --allow-send
enables human-paced text sends with database read-back verification.
"""

from __future__ import annotations

import argparse
import hmac
import json
import os
import secrets
import sys
import time
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any, Callable, Dict, Optional
from urllib.parse import parse_qs, urlparse

try:
    from wechatauto import WeChatDB
except Exception:  # pragma: no cover - reported by /v1/health
    WeChatDB = None

DEFAULT_PORT = 8765
CONFIG_DIR_NAME = "wechat-bridge"
TOKEN_ENV = "WECHAT_BRIDGE_TOKEN"

TYPE_MAP = {
    "文本": "text",
    "图片": "image",
    "语音": "voice",
    "视频": "video",
    "动画表情": "sticker",
    "位置": "location",
    "文件/链接/卡片": "file",
    "音视频通话": "call",
    "系统消息": "system",
    "红包": "redpacket",
}


class BridgeError(Exception):
    def __init__(self, code: str, message: str, status: int = 400, retryable: bool = False):
        super().__init__(message)
        self.code = code
        self.message = message
        self.status = status
        self.retryable = retryable


def default_config_dir() -> str:
    base = os.environ.get("LOCALAPPDATA") or os.path.expanduser("~/.config")
    return os.path.join(base, CONFIG_DIR_NAME)


def ensure_token(config_dir: str) -> str:
    os.makedirs(config_dir, mode=0o700, exist_ok=True)
    path = os.path.join(config_dir, "token")
    try:
        with open(path, "r", encoding="utf-8") as f:
            token = f.read().strip()
        if token:
            return token
    except FileNotFoundError:
        pass
    token = secrets.token_urlsafe(32)
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    try:
        os.write(fd, token.encode("utf-8"))
    finally:
        os.close(fd)
    return token


def normalize_timestamp(value: Any) -> int:
    try:
        ts = int(value)
    except (TypeError, ValueError):
        return 0
    if ts > 10_000_000_000:  # milliseconds in some client builds
        ts //= 1000
    return max(0, ts)


def one_line(value: Any, limit: int = 300) -> str:
    if not isinstance(value, str):
        value = "" if value is None else str(value)
    text = " ".join(value.split())
    return text[:limit]


OFFICIAL_HOLDERS = {"brandsessionholder", "brandservicesessionholder"}

def chat_kind(username: str, verify_flag: int = 0) -> str:
    lowered = username.lower()
    if lowered.endswith("@chatroom"):
        return "group"
    if (
        lowered.startswith("gh_")
        or lowered in OFFICIAL_HOLDERS
        or int(verify_flag or 0) != 0
    ):
        return "official"
    return "dm"


def find_weixin_hwnd() -> int:
    """Return the largest top-level window owned by Weixin.exe, even if hidden."""
    try:
        import psutil
        import win32gui
        import win32process
    except Exception:
        return 0
    best = 0
    best_area = 0

    def visit(hwnd, _):
        nonlocal best, best_area
        if not win32gui.GetWindowText(hwnd):
            return
        if "QWindowIcon" not in win32gui.GetClassName(hwnd):
            return
        try:
            _, pid = win32process.GetWindowThreadProcessId(hwnd)
            name = psutil.Process(pid).name().lower()
        except Exception:
            return
        if name not in {"weixin.exe", "wechat.exe"}:
            return
        left, top, right, bottom = win32gui.GetWindowRect(hwnd)
        area = max(0, right - left) * max(0, bottom - top)
        if area > best_area:
            best, best_area = hwnd, area

    win32gui.EnumWindows(visit, None)
    return best

def map_type(value: Any) -> str:
    if isinstance(value, str):
        return TYPE_MAP.get(value, "unknown")
    return "unknown"


def message_id(row: Dict[str, Any]) -> str:
    shard = str(row.get("shard") or "").replace("\\", "/")
    local_id = row.get("local_id")
    return f"{shard}:{local_id}" if shard else str(local_id)


class Bridge:
    def __init__(
        self,
        db_factory: Optional[Callable[[], Any]] = None,
        read_only: bool = True,
        send_fn: Optional[Callable[[str, str, bool], Any]] = None,
        state_dir: Optional[str] = None,
        min_send_interval: float = 2.0,
    ):
        self._db_factory = db_factory or WeChatDB
        self._db: Optional[Any] = None
        self._self: Optional[Dict[str, Any]] = None
        self._nicknames: Dict[str, str] = {}
        self._lock = threading.Lock()
        self._send_lock = threading.Lock()
        self._flags_cache: Optional[Dict[str, int]] = None
        self._flags_at = 0.0
        self.read_only = read_only
        self._send_fn = send_fn
        self._state_dir = state_dir
        self._min_send_interval = max(0.0, min_send_interval)
        self._last_send_at = 0.0
        self._journal_path = os.path.join(state_dir, "sent.json") if state_dir else None
        self._journal = self._load_journal()

    def _db_instance(self) -> Any:
        if self._db is not None:
            return self._db
        if self._db_factory is None:
            raise BridgeError("db_unavailable", "wechatauto-replica is not installed", 503)
        try:
            self._db = self._db_factory()
        except Exception as exc:  # noqa: BLE001 - surfaced as a stable API error
            raise BridgeError(
                "wechat_not_logged_in",
                f"cannot open the official WeChat database: {exc}",
                503,
                retryable=True,
            ) from exc
        return self._db

    def _self_info(self) -> Dict[str, Any]:
        if self._self is None:
            try:
                self._self = self._db_instance().get_self_info() or {}
            except Exception as exc:  # noqa: BLE001
                raise BridgeError("db_unavailable", str(exc), 503, retryable=True) from exc
        return self._self

    def _nickname(self, username: str) -> str:
        if not username:
            return ""
        if username in self._nicknames:
            return self._nicknames[username]
        try:
            name = self._db_instance().get_nickname(username)
        except Exception:
            name = ""
        name = one_line(name) or username
        self._nicknames[username] = name
        return name

    def _load_journal(self) -> Dict[str, Any]:
        if not self._journal_path:
            return {}
        try:
            with open(self._journal_path, "r", encoding="utf-8") as f:
                data = json.load(f)
            return data if isinstance(data, dict) else {}
        except (FileNotFoundError, json.JSONDecodeError, OSError):
            return {}

    def _save_journal(self) -> None:
        if not self._journal_path:
            return
        if len(self._journal) > 1000:
            for key in list(self._journal)[: len(self._journal) - 1000]:
                del self._journal[key]
        os.makedirs(os.path.dirname(self._journal_path), exist_ok=True)
        tmp = self._journal_path + ".tmp"
        with open(tmp, "w", encoding="utf-8") as f:
            json.dump(self._journal, f, ensure_ascii=False, separators=(",", ":"))
        os.replace(tmp, self._journal_path)

    @staticmethod
    def _strip_sender_prefix(chat_id: str, content: str, sender: str) -> str:
        if chat_id.lower().endswith("@chatroom") and sender and content.startswith(sender + ":"):
            return content[len(sender) + 1:].lstrip("\r\n ")
        return content

    def _target_name(self, chat_id: str, chat_name: str = "") -> str:
        if chat_id == "filehelper":
            return "文件传输助手"
        name = one_line(chat_name) or self._nickname(chat_id)
        if (
            not name
            or name == chat_id
            or name.startswith("wxid_")
            or name.endswith("@chatroom")
            or name.endswith("@openim")
        ):
            raise BridgeError("target_unresolved", "cannot resolve a safe display name for this chat", 409)
        try:
            matches = self._db_instance().search_contact(name) or []
            usernames = {str(m.get("username") or "") for m in matches}
            if len(usernames) > 1 and chat_id not in usernames:
                raise BridgeError("target_ambiguous", "multiple contacts match the display name", 409)
        except BridgeError:
            raise
        except Exception:
            pass
        return name

    def _verify_sent(self, chat_id: str, text: str, since: int) -> Optional[Dict[str, Any]]:
        deadline = time.time() + 8.0
        wanted = text.strip()
        while time.time() < deadline:
            try:
                rows = self._db_instance().get_messages(chat_id, limit=10) or []
            except Exception:
                rows = []
            for row in rows:
                sender = str(row.get("sender_username") or "")
                content = row.get("content")
                content = "" if content is None else str(content)
                content = self._strip_sender_prefix(chat_id, content, sender)
                if content.strip() != wanted:
                    continue
                if normalize_timestamp(row.get("create_time")) < since - 2:
                    continue
                return row
            time.sleep(0.5)
        return None

    @staticmethod
    def _default_send(text: str, who: str, verify: bool) -> Any:
        from wechatauto.guia import WeChatGUI
        hwnd = find_weixin_hwnd()
        if not hwnd:
            raise BridgeError("wechat_not_running", "official Weixin window not found", 503, True)
        wx = WeChatGUI(hwnd=hwnd)
        return wx.send_msg(text, who, verify=verify)

    def _contact_verify_flags(self) -> Dict[str, int]:
        now = time.monotonic()
        if self._flags_cache is not None and now - self._flags_at < 60:
            return self._flags_cache
        flags: Dict[str, int] = {}
        try:
            db = self._db_instance()
            for rel, path, _ in getattr(db, "_db_files", []):
                if os.path.basename(path) != "contact.db":
                    continue
                conn = db._open(rel)
                try:
                    for row in conn.execute("SELECT username, verify_flag FROM contact"):
                        flags[str(row[0])] = int(row[1] or 0)
                finally:
                    conn.close()
                break
        except Exception:
            flags = {}
        self._flags_cache = flags
        self._flags_at = now
        return flags

    def health(self) -> Dict[str, Any]:
        with self._lock:
            result: Dict[str, Any] = {
                "ok": False,
                "bridge_version": "0.1.0",
                "read_only": self.read_only,
                "db": {"status": "unknown"},
                "warnings": [],
            }
            try:
                db = self._db_instance()
                info = self._self_info()
                result["ok"] = True
                result["account"] = {
                    "id": info.get("username") or getattr(db, "wxid", ""),
                    "name": one_line(info.get("remark") or info.get("nick_name") or ""),
                }
                result["db"] = {"status": "ok"}
            except BridgeError as exc:
                result["error"] = exc.code
                result["message"] = exc.message
                result["db"] = {"status": "error"}
                return result
            return result

    def chats(self, limit: int) -> Dict[str, Any]:
        with self._lock:
            db = self._db_instance()
            flags = self._contact_verify_flags()
            try:
                sessions = db.get_sessions(limit=limit) or []
            except Exception as exc:  # noqa: BLE001
                raise BridgeError("db_unavailable", str(exc), 503, retryable=True) from exc
            chats = []
            for session in sessions:
                username = str(session.get("username") or "")
                if not username:
                    continue
                last_time = normalize_timestamp(session.get("last_time"))
                last = None
                if last_time:
                    last = {
                        "id": f"preview:{username}:{last_time}",
                        "timestamp": last_time,
                        "sender": one_line(session.get("last_sender")),
                        "preview": one_line(session.get("summary")),
                        "type": "text",
                    }
                chats.append(
                    {
                        "id": username,
                        "name": self._nickname(username),
                        "kind": chat_kind(username, flags.get(username, 0)),
                        "unread": int(session.get("unread") or 0),
                        "pinned": False,
                        "last": last,
                    }
                )
            return {"chats": chats, "next_cursor": None}

    def messages(self, chat_id: str, limit: int, offset: int) -> Dict[str, Any]:
        with self._lock:
            db = self._db_instance()
            self_info = self._self_info()
            self_id = str(self_info.get("username") or getattr(db, "wxid", ""))
            try:
                rows = db.get_messages(chat_id, limit=limit, offset=offset) or []
            except Exception as exc:  # noqa: BLE001
                raise BridgeError("db_unavailable", str(exc), 503, retryable=True) from exc
            messages = []
            for row in reversed(rows):  # DB returns newest first; TUI wants oldest first
                sender = str(row.get("sender_username") or "")
                direction = "unknown"
                if sender and self_id:
                    direction = "out" if sender == self_id else "in"
                content = row.get("content")
                if not isinstance(content, str):
                    content = "" if content is None else str(content)
                content = self._strip_sender_prefix(chat_id, content, sender)
                messages.append(
                    {
                        "id": message_id(row),
                        "chat_id": chat_id,
                        "sender_id": sender,
                        "sender_name": self._nickname(sender) if sender else "",
                        "direction": direction,
                        "type": map_type(row.get("type")),
                        "text": content,
                        "timestamp": normalize_timestamp(row.get("create_time")),
                        # sort_seq is 64-bit in WeChat 4.x; the TUI sequence field is uint32.
                        "sequence": 0,
                    }
                )
            return {
                "chat_id": chat_id,
                "messages": messages,
                "next_offset": offset + len(rows) if len(rows) == limit else None,
            }

    def send(self, chat_id: str, text: str, client_id: str, chat_name: str = "") -> Dict[str, Any]:
        if self.read_only:
            raise BridgeError("read_only", "this bridge is read-only", 403)
        chat_id = chat_id.strip()
        text = text.strip()
        client_id = client_id.strip()
        if not chat_id or not text or not client_id:
            raise BridgeError("bad_request", "chat_id, text and client_id are required", 400)
        if len(text) > 60 * 1024:
            raise BridgeError("bad_request", "message exceeds 60 KiB", 400)
        with self._send_lock:
            with self._lock:
                cached = self._journal.get(client_id)
                if cached:
                    if cached.get("accepted"):
                        return cached["result"]
                    raise BridgeError(
                        str(cached.get("code") or "send_failed"),
                        str(cached.get("message") or "previous send was not confirmed"),
                        int(cached.get("status") or 502),
                        False,
                    )
                now = time.monotonic()
                if now - self._last_send_at < self._min_send_interval:
                    raise BridgeError("rate_limited", "wait before sending another message", 429, True)
                name = self._target_name(chat_id, chat_name)
                self._last_send_at = now
                since = int(time.time())
            # Do not hold the read lock while driving the official UI. The TUI
            # syncs chats/messages through the same bridge and must stay live.
            try:
                send = self._send_fn or self._default_send
                response = send(text, name, False)
                message = str(response.get("message") or "") if isinstance(response, dict) else ""
            except Exception as exc:  # noqa: BLE001
                response = None
                message = f"send failed: {exc}"
            with self._lock:
                row = self._verify_sent(chat_id, text, since)
                if row is not None:
                    result = {
                        "accepted": True,
                        "verified": True,
                        "client_id": client_id,
                        "message_id": message_id(row),
                        "timestamp": normalize_timestamp(row.get("create_time")),
                    }
                    self._journal[client_id] = {"accepted": True, "result": result}
                    self._save_journal()
                    return result
                failure = {
                    "accepted": False,
                    "code": "send_unverified",
                    "message": message or "message was submitted but could not be confirmed in the database",
                    "status": 502,
                }
                self._journal[client_id] = failure
                self._save_journal()
                raise BridgeError("send_unverified", failure["message"], 502, False)


class Handler(BaseHTTPRequestHandler):
    server_version = "wechat-bridge/0.1"
    protocol_version = "HTTP/1.1"

    def handle_one_request(self) -> None:
        try:
            super().handle_one_request()
        except (ConnectionResetError, BrokenPipeError):
            pass

    def _authorized(self) -> bool:
        expected = getattr(self.server, "token", "")
        if not expected:
            return True
        supplied = self.headers.get("Authorization", "")
        return hmac.compare_digest(supplied, f"Bearer {expected}")

    def _json(self, status: int, payload: Dict[str, Any]) -> None:
        body = json.dumps(payload, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(body)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(body)

    def _error(self, exc: BridgeError) -> None:
        self._json(
            exc.status,
            {"error": {"code": exc.code, "message": exc.message, "retryable": exc.retryable}},
        )

    def do_GET(self) -> None:  # noqa: N802 - http.server API
        if not self._authorized():
            self._json(401, {"error": {"code": "unauthorized", "message": "invalid token", "retryable": False}})
            return
        parsed = urlparse(self.path)
        query = parse_qs(parsed.query)
        bridge: Bridge = self.server.bridge  # type: ignore[attr-defined]
        try:
            if parsed.path == "/v1/health":
                self._json(200, bridge.health())
            elif parsed.path == "/v1/chats":
                limit = min(200, max(1, int(query.get("limit", ["100"])[0])))
                self._json(200, bridge.chats(limit))
            elif parsed.path == "/v1/messages":
                chat_id = (query.get("chat_id") or [""])[0]
                if not chat_id:
                    raise BridgeError("bad_request", "chat_id is required", 400)
                limit = min(200, max(1, int(query.get("limit", ["50"])[0])))
                offset = max(0, int(query.get("offset", ["0"])[0]))
                self._json(200, bridge.messages(chat_id, limit, offset))
            else:
                raise BridgeError("not_found", "unknown endpoint", 404)
        except ValueError as exc:
            self._error(BridgeError("bad_request", str(exc), 400))
        except BridgeError as exc:
            self._error(exc)
        except Exception as exc:  # noqa: BLE001 - never leak tracebacks over HTTP
            print(f"wechat-bridge: {type(exc).__name__}: {exc}", file=sys.stderr)
            self._error(BridgeError("internal_error", "bridge request failed", 500))

    def do_POST(self) -> None:  # noqa: N802 - http.server API
        if not self._authorized():
            self._json(401, {"error": {"code": "unauthorized", "message": "invalid token", "retryable": False}})
            return
        parsed = urlparse(self.path)
        length = min(1_000_000, int(self.headers.get("Content-Length", "0") or 0))
        body = self.rfile.read(length) if length else b"{}"
        try:
            payload = json.loads(body.decode("utf-8"))
        except (UnicodeDecodeError, json.JSONDecodeError):
            self._error(BridgeError("bad_request", "body must be JSON", 400))
            return
        bridge: Bridge = self.server.bridge  # type: ignore[attr-defined]
        try:
            if parsed.path == "/v1/send":
                result = bridge.send(
                    str(payload.get("chat_id") or ""),
                    str(payload.get("text") or ""),
                    str(payload.get("client_id") or ""),
                    str(payload.get("chat_name") or ""),
                )
                self._json(200, result)
            else:
                raise BridgeError("not_found", "unknown endpoint", 404)
        except BridgeError as exc:
            self._error(exc)
        except Exception as exc:  # noqa: BLE001
            print(f"wechat-bridge: {type(exc).__name__}: {exc}", file=sys.stderr)
            self._error(BridgeError("internal_error", "bridge request failed", 500))

    def log_message(self, _format: str, *args: Any) -> None:
        del args


def parse_args(argv: Optional[list[str]] = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description="local bridge for official WeChat 4.x")
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=DEFAULT_PORT)
    parser.add_argument("--config-dir", default=default_config_dir())
    parser.add_argument("--token", default=os.environ.get(TOKEN_ENV))
    parser.add_argument("--allow-send", action="store_true", help="enable human-paced text sending")
    return parser.parse_args(argv)


def main(argv: Optional[list[str]] = None) -> int:
    args = parse_args(argv)
    token = args.token
    token_path = os.path.join(args.config_dir, "token")
    if not token:
        token = ensure_token(args.config_dir)
    if args.host not in {"127.0.0.1", "::1", "localhost"}:
        print("wechat-bridge: warning: listening on a non-loopback address; use SSH/Tailscale", file=sys.stderr)
    bridge = Bridge(read_only=not args.allow_send, state_dir=args.config_dir)
    server = ThreadingHTTPServer((args.host, args.port), Handler)
    server.bridge = bridge  # type: ignore[attr-defined]
    server.token = token  # type: ignore[attr-defined]
    print(
        f"wechat-bridge listening on http://{args.host}:{args.port} "
        f"({'send enabled' if not bridge.read_only else 'read-only'}; token file: {token_path})",
        file=sys.stderr,
    )
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        pass
    finally:
        server.server_close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
