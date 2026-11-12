package main

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/kevmo314/wechat-cli/internal/native"
)

type fakeChatBackend struct {
	calls      int
	peer, text string
}

func (f *fakeChatBackend) sync(context.Context) chatSyncResult { return chatSyncResult{} }
func (f *fakeChatBackend) send(_ context.Context, peer, text string) chatSendResult {
	f.calls++
	f.peer = peer
	f.text = text
	return chatSendResult{Peer: peer, Text: text, Accepted: true, Message: native.Message{ID: "sent", Peer: peer, Text: text, Outgoing: true, Type: 1, Timestamp: 4}}
}
func testChatModel() chatModel {
	data := chatSnapshot{Contacts: map[string]native.Contact{"alice": {Username: "alice", Nickname: "Alice 王"}, "bob": {Username: "bob", Nickname: "Bob"}}, Messages: []native.Message{
		{ID: "1", From: "alice", To: "me", Peer: "alice", Type: 1, Text: "你好 👋 this is a message", Timestamp: 2},
		{ID: "2", From: "bob", To: "me", Peer: "bob", Type: 1, Text: "Hello", Timestamp: 1},
		{ID: "3", From: "me", To: "me", Peer: "me", Type: 51, Text: "INTERNAL_CONTROL", Timestamp: 3},
	}}
	return newChatModel(context.Background(), &fakeChatBackend{}, "me", native.AndroidTabletProfile, data, false)
}
func TestTUILayoutAndControlFiltering(t *testing.T) {
	for _, size := range [][2]int{{64, 18}, {80, 24}, {120, 40}} {
		m := testChatModel()
		m.resize(size[0], size[1])
		view := m.View()
		if strings.Contains(view, "INTERNAL_CONTROL") {
			t.Fatal("system control became a chat")
		}
		if !strings.Contains(view, "Alice") {
			t.Fatal("contact name not shown")
		}
		if h := lipgloss.Height(view); h > size[1] {
			t.Errorf("%v: view height %d", size, h)
		}
		for _, line := range strings.Split(view, "\n") {
			if w := lipgloss.Width(line); w > size[0] {
				t.Errorf("%v: line width %d: %q", size, w, ansi.Strip(line))
			}
		}
	}
}
func TestTUISendCapturesRecipientAndBlocksDoubleSubmit(t *testing.T) {
	m := testChatModel()
	backend := m.backend.(*fakeChatBackend)
	m.setFocus(focusCompose)
	m.composer.SetValue("Hello Alice")
	next, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(chatModel)
	if m.composer.Value() != "" {
		t.Fatal("composer did not clear immediately after submit")
	}
	if cmd == nil || !m.sending {
		t.Fatal("explicit submit did not start")
	}
	next, duplicate := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m = next.(chatModel)
	if duplicate != nil {
		t.Fatal("double submit scheduled")
	}
	m.selectPeer("bob")
	m.composer.SetValue("Draft for Bob")
	result := cmd()
	if backend.calls != 1 || backend.peer != "alice" || backend.text != "Hello Alice" {
		t.Fatalf("recipient changed during send: %#v", backend)
	}
	next, _ = m.Update(result)
	m = next.(chatModel)
	if m.composer.Value() != "Draft for Bob" || m.selected != "bob" {
		t.Fatal("send completion destroyed another conversation's draft")
	}
	m.selectPeer("alice")
	if m.composer.Value() != "" {
		t.Fatal("accepted draft retained for accidental resend")
	}
}
func TestTUIDraftsSurviveIncomingReorderAndPasteDoesNotSend(t *testing.T) {
	m := testChatModel()
	m.setFocus(focusCompose)
	m.composer.SetValue("draft alice")
	next, _ := m.Update(chatSyncResult{Messages: []native.Message{{ID: "4", From: "bob", To: "me", Peer: "bob", Type: 1, Text: "new", Timestamp: 10}}})
	m = next.(chatModel)
	if m.selected != "alice" || m.composer.Value() != "draft alice" {
		t.Fatal("sync switched compose recipient")
	}
	m.selectPeer("bob")
	m.composer.SetValue("draft bob")
	m.selectPeer("alice")
	if m.composer.Value() != "draft alice" {
		t.Fatal("draft lost when switching")
	}
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("pasted\ntext"), Paste: true})
	m = next.(chatModel)
	if m.sending || m.backend.(*fakeChatBackend).calls != 0 {
		t.Fatal("paste sent a message")
	}
}
func TestTUIEscapesRemoteTerminalControl(t *testing.T) {
	m := testChatModel()
	m.contacts["alice"] = native.Contact{Username: "alice", Nickname: "Alice\x1b]52;c;dGVzdA==\a"}
	m.messages[0].Text = "hello\x1b[2J\x1b]0;malicious-title\a"
	m.refreshHistory(true)
	view := m.View()
	if strings.Contains(view, "malicious-title") || strings.Contains(view, "dGVzdA==") || strings.Contains(view, "\x1b[2J") {
		t.Fatal("remote terminal escape rendered")
	}
}
func TestTUIMediaPreviews(t *testing.T) {
	if got := messagePreview(native.Message{Type: 49, Text: "<msg><appmsg><title>A shared link</title></appmsg></msg>"}); got != "[Shared item] A shared link" {
		t.Fatal(got)
	}
	if got := messagePreview(native.Message{Type: 3, Text: "raw image metadata"}); strings.Contains(got, "metadata") {
		t.Fatal(got)
	}
}

func TestCompactMessageLayout(t *testing.T) {
	m := testChatModel()
	m.resize(120, 30)
	if n := m.viewport.TotalLineCount(); n > 3 {
		t.Fatalf("two short messages should not consume three lines each: %d", n)
	}
}

func TestTUIArrowNavigationKeepsComposerFocused(t *testing.T) {
	m := testChatModel()
	m.composer.SetValue("Alice draft")
	next, _ := m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m = next.(chatModel)
	if m.selected != "bob" || m.focus != focusCompose || !m.composer.Focused() {
		t.Fatal("arrow navigation left composer focus")
	}
	m.composer.SetValue("Bob draft")
	next, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m = next.(chatModel)
	if m.composer.Value() != "Alice draft" {
		t.Fatal("switching lost the original draft")
	}
}
func TestPinnedConversationAndUnreadActivity(t *testing.T) {
	m := testChatModel()
	c := m.contacts["alice"]
	c.Flags = 0x803
	m.contacts["alice"] = c
	m.rebuildRows()
	next, _ := m.Update(chatSyncResult{Messages: []native.Message{{ID: "new-bob", From: "bob", To: "me", Peer: "bob", Type: 1, Text: "new arrival with older server clock", Timestamp: 1}}})
	m = next.(chatModel)
	if m.rows[0].Peer != "alice" {
		t.Fatal("incoming activity displaced a pinned conversation")
	}
	if m.totalUnread() != 1 || m.unread["bob"] != 1 {
		t.Fatal("incoming unread event not counted")
	}
	if !strings.Contains(ansi.Strip(m.View()), "PIN ·") {
		t.Fatal("pinned conversation not demarcated")
	}
	next, _ = m.Update(chatSyncResult{Messages: []native.Message{{ID: "new-bob", From: "bob", Peer: "bob", Type: 1, Timestamp: 1}}})
	m = next.(chatModel)
	if m.totalUnread() != 1 {
		t.Fatal("replayed batch double-counted unread")
	}
}
func TestHangingMessageIndent(t *testing.T) {
	text := hangingMessage("Alice 12:34  This is a message that wraps onto a few continuation lines.", 32)
	lines := strings.Split(ansi.Strip(text), "\n")
	if len(lines) < 2 {
		t.Fatal("fixture did not wrap")
	}
	for _, line := range lines[1:] {
		if !strings.HasPrefix(line, "  ") {
			t.Fatalf("continuation not indented: %q", line)
		}
		if lipgloss.Width(line) > 32 {
			t.Fatal("wrapped line overflow")
		}
	}
}

type fakeReadBackend struct {
	fakeChatBackend
	reads int
}

func (f *fakeReadBackend) markRead(_ context.Context, m native.Message) chatReadResult {
	f.reads++
	return chatReadResult{Peer: m.Peer, MessageID: m.ID, Accepted: true}
}
func TestReadStateRequiresVisibleFocusedBottom(t *testing.T) {
	m := testChatModel()
	backend := &fakeReadBackend{}
	m.backend = backend
	due := chatReadDue{Peer: "alice", MessageID: "1"}
	m.terminalFocused = false
	next, cmd := m.update(due)
	m = next.(chatModel)
	if cmd != nil || backend.reads != 0 {
		t.Fatal("background terminal marked conversation read")
	}
	m.terminalFocused = true
	m.mediaOpen = true
	_, cmd = m.update(due)
	if cmd != nil {
		t.Fatal("attachment overlay marked conversation read")
	}
	m.mediaOpen = false
	m.viewport.SetContent("one\ntwo\nthree")
	m.viewport.Height = 1
	m.viewport.GotoTop()
	_, cmd = m.update(due)
	if cmd != nil {
		t.Fatal("scrolled-back view marked conversation read")
	}
	m.viewport.GotoBottom()
	next, cmd = m.update(due)
	m = next.(chatModel)
	if cmd == nil {
		t.Fatal("visible boundary did not schedule read")
	}
	result := cmd()
	if backend.reads != 1 {
		t.Fatal("read not submitted")
	}
	next, _ = m.update(result)
	m = next.(chatModel)
	_, cmd = m.update(due)
	if cmd != nil {
		t.Fatal("same read boundary resubmitted")
	}
}

func TestDeletedSharedItemAndGroupSenderLabel(t *testing.T) {
	m := testChatModel()
	m.contacts["room@chatroom"] = native.Contact{Username: "room@chatroom", Nickname: "Team", Members: map[string]string{"alice": "Room Alias"}}
	if got := m.senderName("room@chatroom", "alice"); got != "Room Alias" {
		t.Fatal("roster name not resolved", got)
	}
	m.contacts["alice"] = native.Contact{Username: "alice", Nickname: "Alice", Remark: "My Alice"}
	if got := m.senderName("room@chatroom", "alice"); got != "My Alice" {
		t.Fatal("personal remark did not take precedence", got)
	}
	item := native.Message{Type: 49, Deleted: true, Text: "<msg><appmsg><title>Interview notes</title></appmsg></msg>"}
	if got := messagePreview(item); got != "[Deleted] Interview notes" {
		t.Fatal("deleted shared item not marked", got)
	}
}

func TestMessageLinksRejectTerminalControlsAndOtherSchemes(t *testing.T) {
	for _, value := range []string{"file:///etc/passwd", "javascript:alert(1)", "https://example.com/\x1b]52;c;payload\a", "https://user:password@example.com/"} {
		if _, ok := validWebURL(value); ok {
			t.Fatal("unsafe web URL accepted")
		}
	}
	links := messageLinks(native.Message{Type: 1, Text: "See https://example.com/a and https://example.com/b."})
	if len(links) != 2 || links[1] != "https://example.com/b" {
		t.Fatal(links)
	}
	if links = messageLinks(native.Message{Type: 1, Deleted: true, Text: "https://example.com/"}); len(links) != 0 {
		t.Fatal("deleted message still exposed a clickable link")
	}
}
func TestQuotedMessageViewerAndReplySelection(t *testing.T) {
	m := testChatModel()
	source := native.Message{ID: "99", Peer: "alice", From: "alice", To: "me", Type: 1, Text: "Original message", Timestamp: 10}
	quoted := native.Message{ID: "100", Peer: "alice", From: "alice", To: "me", Type: 49, Timestamp: 11, Text: `<msg><appmsg><type>57</type><title>Reply</title><refermsg><type>1</type><svrid>99</svrid><fromusr>alice</fromusr><chatusr>alice</chatusr><displayname>Alice</displayname><content>Original message</content></refermsg></appmsg></msg>`}
	m.addMessages([]native.Message{source, quoted}, false)
	m.openMessageActions()
	next, _ := m.messageActionKey("v")
	m = next.(chatModel)
	if m.actionsDetail == nil || m.actionsDetail.ID != "99" || m.actionsDetail.Text != "Original message" {
		t.Fatal("quoted target not resolved")
	}
	next, _ = m.messageActionKey("r")
	m = next.(chatModel)
	if m.actionsOpen || m.replyTargets["alice"].ID != "99" || m.focus != focusCompose {
		t.Fatal("reply selection did not return to composer")
	}
	m.selectPeer("bob")
	if _, ok := m.replyTargets["bob"]; ok {
		t.Fatal("reply target crossed conversation boundary")
	}
	c := m.contacts["alice"]
	c.Remark = "Private personal note"
	m.contacts["alice"] = c
	if publicQuoteName(m.contacts, source) != "Alice 王" {
		t.Fatal("private contact remark would leak in an outgoing quote")
	}
}

func TestTUIOfficialAccountsHiddenFromChatList(t *testing.T) {
	data := chatSnapshot{
		Contacts: map[string]native.Contact{
			"alice":   {Username: "alice", Nickname: "Alice"},
			"gh_news": {Username: "gh_news", Nickname: "Daily News"},
		},
		Messages: []native.Message{
			{ID: "1", From: "alice", To: "me", Peer: "alice", Type: 1, Text: "hello", Timestamp: 2},
			{ID: "2", From: "gh_news", To: "me", Peer: "gh_news", Type: 1, Text: "headline", Timestamp: 3},
		},
	}
	m := newChatModel(context.Background(), &fakeChatBackend{}, "me", native.AndroidTabletProfile, data, true)
	if len(m.rows) != 1 || m.rows[0].Peer != "alice" {
		t.Fatalf("official account leaked into chat list: %#v", m.rows)
	}
	m.unread["alice"] = 1
	m.unread["gh_news"] = 5
	if got := m.totalUnread(); got != 1 {
		t.Fatalf("official account unread counted: %d", got)
	}
	m.query = "news"
	m.rebuildRows()
	found := false
	for _, row := range m.rows {
		if row.Peer == "gh_news" {
			found = true
		}
	}
	if !found {
		t.Fatal("search should still find official accounts")
	}
	m.query = ""
	m.rebuildRows()
	m.selectPeer("gh_news")
	m.rebuildRows()
	if len(m.rows) != 2 || m.selected != "gh_news" {
		t.Fatalf("selected official account should stay visible: %#v", m.rows)
	}
}
