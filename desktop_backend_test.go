package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kevmo314/wechat-cli/internal/native"
)

func TestTUIDesktopPreviewDoesNotBecomeTranscriptMessage(t *testing.T) {
	m := testChatModel()
	m.previews["carol"] = native.Message{
		ID:        "preview:carol:10",
		Peer:      "carol",
		Type:      1,
		Text:      "latest preview",
		Timestamp: 10,
	}
	m.rebuildRows()
	found := false
	for _, row := range m.rows {
		if row.Peer == "carol" {
			found = true
			if row.Last.Text != "latest preview" {
				t.Fatalf("preview not attached to row: %#v", row)
			}
		}
	}
	if !found {
		t.Fatal("preview-only conversation missing from sidebar")
	}
	for _, message := range m.messages {
		if strings.HasPrefix(message.ID, "preview:") {
			t.Fatal("preview leaked into transcript messages")
		}
	}
	m.resize(100, 30)
	if view := m.View(); !strings.Contains(view, "latest preview") {
		t.Fatal("preview not rendered in sidebar")
	}
}

func TestDesktopBackendSyncMapsChatsPreviewsAndMessages(t *testing.T) {
	var requestedChats []string
	var sentBody map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, `{"error":{"code":"unauthorized"}}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/chats":
			_, _ = fmt.Fprint(w, `{"chats":[
				{"id":"alice","name":"Alice Nick","kind":"dm","unread":1,"pinned":false,
				 "last":{"id":"preview:alice:10","timestamp":1700000000,"sender":"Alice","preview":"hi","type":"text"}},
				{"id":"gh_news","name":"Daily News","kind":"official","unread":0,"pinned":false,"last":null}
			]}`)
		case "/v1/messages":
			chat := r.URL.Query().Get("chat_id")
			requestedChats = append(requestedChats, chat)
			if chat == "alice" {
				_, _ = fmt.Fprint(w, `{"chat_id":"alice","messages":[
					{"id":"m1","chat_id":"alice","sender_id":"alice","sender_name":"Alice","direction":"in",
					 "type":"text","text":"hello","timestamp":1700000001,"sequence":7}
				]}`)
				return
			}
			_, _ = fmt.Fprint(w, `{"chat_id":"`+chat+`","messages":[]}`)
		case "/v1/send":
			if err := json.NewDecoder(r.Body).Decode(&sentBody); err != nil {
				http.Error(w, `{"error":{"code":"bad_request"}}`, http.StatusBadRequest)
				return
			}
			_, _ = fmt.Fprint(w, `{"accepted":true,"verified":true,"client_id":"client-1","message_id":"m99","timestamp":1700000002}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	backend := &desktopBackend{
		baseURL: server.URL,
		token:   "test-token",
		owner:   "Me",
		client:  &http.Client{},
	}
	result := backend.sync(context.Background())
	if result.Err != nil {
		t.Fatalf("sync: %v", result.Err)
	}
	if len(requestedChats) != 1 || requestedChats[0] != "alice" {
		t.Fatalf("default focus did not pick the first non-official chat: %v", requestedChats)
	}
	if result.Contacts["alice"].Nickname != "Alice Nick" {
		t.Fatalf("contact name not mapped: %#v", result.Contacts["alice"])
	}
	if result.Previews["alice"].Text != "hi" {
		t.Fatalf("preview not mapped: %#v", result.Previews["alice"])
	}
	if len(result.Messages) != 1 {
		t.Fatalf("messages not mapped: %#v", result.Messages)
	}
	message := result.Messages[0]
	if message.Peer != "alice" || message.Type != 1 || message.Timestamp != 1700000001 || message.Sequence != 7 || message.Outgoing {
		t.Fatalf("message fields incorrect: %#v", message)
	}

	backend.focus("gh_news")
	result = backend.sync(context.Background())
	if result.Err != nil {
		t.Fatalf("second sync: %v", result.Err)
	}
	if len(requestedChats) != 2 || requestedChats[1] != "gh_news" {
		t.Fatalf("focus was not forwarded: %v", requestedChats)
	}
	send := backend.send(context.Background(), "alice", "hello")
	if send.Err != nil || !send.Accepted || send.Message.ID != "m99" {
		t.Fatalf("send failed: %#v", send)
	}
	if sentBody["chat_name"] != "Alice Nick" || sentBody["client_id"] == "" || sentBody["text"] != "hello" {
		t.Fatalf("send body not mapped: %#v", sentBody)
	}
}

func TestBridgeTypeCode(t *testing.T) {
	cases := map[string]int32{
		"text": 1, "image": 3, "voice": 34, "video": 43, "sticker": 47,
		"location": 48, "file": 49, "call": 50, "system": 10000, "unknown": 1,
	}
	for kind, want := range cases {
		if got := bridgeTypeCode(kind); got != want {
			t.Errorf("bridgeTypeCode(%q) = %d, want %d", kind, got, want)
		}
	}
}
