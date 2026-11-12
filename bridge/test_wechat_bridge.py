import http.client
import json
import os
import tempfile
import threading
import time
import unittest
from http.server import ThreadingHTTPServer

import wechat_bridge as wb


class FakeDB:
    wxid = "wxid_me"

    def __init__(self):
        self.fail = False
        self.sent = []

    def get_self_info(self):
        if self.fail:
            raise RuntimeError("database unavailable")
        return {"username": "wxid_me", "nick_name": "Me", "remark": ""}

    def get_nickname(self, user):
        return {
            "wxid_me": "Me",
            "wxid_alice": "Alice",
            "gh_news": "Daily News",
            "room@chatroom": "Team",
        }.get(user, user)

    def search_contact(self, keyword):
        return []

    def get_sessions(self, limit=100):
        if self.fail:
            raise RuntimeError("database unavailable")
        return [
            {
                "username": "wxid_alice",
                "unread": 2,
                "summary": "hello there",
                "last_time": 1700000000000,
                "last_sender": "Alice",
            },
            {
                "username": "gh_news",
                "unread": 0,
                "summary": "headline",
                "last_time": 1699990000,
                "last_sender": "Daily News",
            },
            {
                "username": "room@chatroom",
                "unread": 1,
                "summary": "group message",
                "last_time": 1699980000,
                "last_sender": "Bob",
            },
        ]

    def get_messages(self, user, limit=20, offset=0):
        if self.fail:
            raise RuntimeError("database unavailable")
        rows = [
            {
                "local_id": 2,
                "type": "文本",
                "sender_username": "wxid_me",
                "create_time": 1700000002,
                "content": "my reply",
                "sort_seq": 22,
                "shard": "message_0.db",
            },
            {
                "local_id": 1,
                "type": "文本",
                "sender_username": "wxid_alice",
                "create_time": 1700000001,
                "content": "hello there",
                "sort_seq": 21,
                "shard": "message_0.db",
            },
        ]
        return (self.sent + rows)[offset:offset + limit]


class BridgeTests(unittest.TestCase):
    def setUp(self):
        self.db = FakeDB()
        self.bridge = wb.Bridge(db_factory=lambda: self.db, read_only=True)

    def test_health_and_account(self):
        health = self.bridge.health()
        self.assertTrue(health["ok"])
        self.assertEqual(health["account"]["id"], "wxid_me")
        self.assertEqual(health["account"]["name"], "Me")
        self.assertTrue(health["read_only"])

    def test_health_reports_db_failure(self):
        self.db.fail = True
        health = self.bridge.health()
        self.assertFalse(health["ok"])
        self.assertEqual(health["error"], "db_unavailable")

    def test_chats_map_official_and_group(self):
        result = self.bridge.chats(100)
        chats = {chat["id"]: chat for chat in result["chats"]}
        self.assertEqual(chats["gh_news"]["kind"], "official")
        self.assertEqual(chats["room@chatroom"]["kind"], "group")
        self.assertEqual(chats["wxid_alice"]["kind"], "dm")
        self.assertEqual(chats["wxid_alice"]["last"]["timestamp"], 1700000000)
        self.assertEqual(chats["wxid_alice"]["last"]["preview"], "hello there")

    def test_messages_are_oldest_first_and_direction_is_mapped(self):
        result = self.bridge.messages("wxid_alice", 20, 0)
        messages = result["messages"]
        self.assertEqual([m["id"] for m in messages], ["message_0.db:1", "message_0.db:2"])
        self.assertEqual([m["direction"] for m in messages], ["in", "out"])
        self.assertEqual(messages[0]["timestamp"], 1700000001)
        self.assertEqual(messages[0]["type"], "text")
        self.assertEqual(messages[0]["sender_name"], "Alice")

    def test_read_only_send_is_rejected(self):
        with self.assertRaises(wb.BridgeError) as raised:
            self.bridge.send("wxid_alice", "hello", "client-1")
        self.assertEqual(raised.exception.code, "read_only")
        self.assertEqual(raised.exception.status, 403)

    def test_token_is_created_private_and_reused(self):
        with tempfile.TemporaryDirectory() as directory:
            first = wb.ensure_token(directory)
            second = wb.ensure_token(directory)
            self.assertEqual(first, second)
            self.assertGreaterEqual(len(first), 32)
            path = os.path.join(directory, "token")
            self.assertTrue(os.path.exists(path))
            if os.name != "nt":
                self.assertEqual(os.stat(path).st_mode & 0o777, 0o600)


    def test_send_is_verified_and_idempotent(self):
        calls = []

        def fake_send(text, who, verify):
            calls.append((text, who, verify))
            self.db.sent.insert(0, {
                "local_id": 99,
                "type": "文本",
                "sender_username": "wxid_me",
                "create_time": int(time.time()),
                "content": text,
                "sort_seq": 1,
                "shard": "message_0.db",
            })
            return {"status": "成功", "message": "sent"}

        with tempfile.TemporaryDirectory() as directory:
            bridge = wb.Bridge(
                db_factory=lambda: self.db,
                read_only=False,
                send_fn=fake_send,
                state_dir=directory,
                min_send_interval=0,
            )
            first = bridge.send("wxid_alice", "hello", "client-1")
            second = bridge.send("wxid_alice", "hello", "client-1")
            self.assertTrue(first["accepted"])
            self.assertTrue(first["verified"])
            self.assertEqual(first, second)
            self.assertEqual(len(calls), 1)
            self.assertEqual(calls[0][1], "Alice")
            self.assertFalse(calls[0][2])

            third = bridge.send("wxid_alice", "hello", "client-2")
            self.assertTrue(third["accepted"])
            self.assertEqual(len(calls), 2)

    def test_send_without_database_confirmation_is_not_retried(self):
        calls = []

        def fake_send(text, who, verify):
            calls.append(text)
            return {"status": "失败", "message": "submission uncertain"}

        with tempfile.TemporaryDirectory() as directory:
            bridge = wb.Bridge(
                db_factory=lambda: self.db,
                read_only=False,
                send_fn=fake_send,
                state_dir=directory,
                min_send_interval=0,
            )
            bridge._verify_sent = lambda chat_id, text, since: None
            with self.assertRaises(wb.BridgeError) as raised:
                bridge.send("wxid_alice", "hello", "client-3")
            self.assertEqual(raised.exception.code, "send_unverified")
            with self.assertRaises(wb.BridgeError):
                bridge.send("wxid_alice", "hello", "client-3")
            self.assertEqual(len(calls), 1)

class HTTPTests(unittest.TestCase):
    def setUp(self):
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), wb.Handler)
        self.server.bridge = wb.Bridge(db_factory=lambda: FakeDB(), read_only=True)
        self.server.token = "test-token"
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()

    def tearDown(self):
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=2)

    def request(self, method, path, token=None, body=None):
        conn = http.client.HTTPConnection("127.0.0.1", self.server.server_port, timeout=3)
        headers = {}
        if token:
            headers["Authorization"] = f"Bearer {token}"
        if body is not None:
            data = json.dumps(body).encode()
            headers["Content-Type"] = "application/json"
        else:
            data = None
        conn.request(method, path, body=data, headers=headers)
        response = conn.getresponse()
        payload = json.loads(response.read())
        conn.close()
        return response.status, payload

    def test_requires_token(self):
        status, payload = self.request("GET", "/v1/health")
        self.assertEqual(status, 401)
        self.assertEqual(payload["error"]["code"], "unauthorized")

    def test_authorized_read(self):
        status, payload = self.request("GET", "/v1/health", "test-token")
        self.assertEqual(status, 200)
        self.assertTrue(payload["ok"])
        status, payload = self.request("GET", "/v1/chats?limit=10", "test-token")
        self.assertEqual(status, 200)
        self.assertEqual(len(payload["chats"]), 3)

    def test_send_is_forbidden(self):
        status, payload = self.request(
            "POST", "/v1/send", "test-token", {"chat_id": "wxid_alice", "text": "hello"}
        )
        self.assertEqual(status, 403)
        self.assertEqual(payload["error"]["code"], "read_only")

    def test_bad_json_and_missing_chat_id(self):
        conn = http.client.HTTPConnection("127.0.0.1", self.server.server_port, timeout=3)
        conn.request(
            "POST",
            "/v1/send",
            body=b"{",
            headers={"Authorization": "Bearer test-token", "Content-Type": "application/json"},
        )
        response = conn.getresponse()
        self.assertEqual(response.status, 400)
        response.read()
        conn.close()

        status, payload = self.request("GET", "/v1/messages", "test-token")
        self.assertEqual(status, 400)
        self.assertEqual(payload["error"]["code"], "bad_request")


class ClassificationTests(unittest.TestCase):
    def test_service_accounts_and_groups(self):
        self.assertEqual(wb.chat_kind("gh_news"), "official")
        self.assertEqual(wb.chat_kind("brandsessionholder"), "official")
        self.assertEqual(wb.chat_kind("brandservicesessionholder"), "official")
        self.assertEqual(wb.chat_kind("cmb4008205555", 29), "official")
        self.assertEqual(wb.chat_kind("room@chatroom", 0), "group")
        self.assertEqual(wb.chat_kind("wxid_alice", 0), "dm")
        self.assertEqual(wb.chat_kind("filehelper", 0), "dm")


if __name__ == "__main__":
    unittest.main()
