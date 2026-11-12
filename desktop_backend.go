package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/term"
	"github.com/kevmo314/wechat-cli/internal/native"
)

type bridgeLast struct {
	ID        string `json:"id"`
	Timestamp int64  `json:"timestamp"`
	Sender    string `json:"sender"`
	Preview   string `json:"preview"`
	Type      string `json:"type"`
}

type bridgeChat struct {
	ID     string      `json:"id"`
	Name   string      `json:"name"`
	Kind   string      `json:"kind"`
	Unread int         `json:"unread"`
	Pinned bool        `json:"pinned"`
	Last   *bridgeLast `json:"last"`
}

type bridgeChatsResponse struct {
	Chats []bridgeChat `json:"chats"`
}

type bridgeMessage struct {
	ID         string `json:"id"`
	ChatID     string `json:"chat_id"`
	SenderID   string `json:"sender_id"`
	SenderName string `json:"sender_name"`
	Direction  string `json:"direction"`
	Type       string `json:"type"`
	Text       string `json:"text"`
	Timestamp  int64  `json:"timestamp"`
	Sequence   uint32 `json:"sequence"`
}

type bridgeMessagesResponse struct {
	Messages []bridgeMessage `json:"messages"`
}

type bridgeSendResponse struct {
	Accepted  bool   `json:"accepted"`
	Verified  bool   `json:"verified"`
	ClientID  string `json:"client_id"`
	MessageID string `json:"message_id"`
	Timestamp int64  `json:"timestamp"`
}

type bridgeHealth struct {
	OK       bool   `json:"ok"`
	ReadOnly bool   `json:"read_only"`
	Error    string `json:"error"`
	Message  string `json:"message"`
	Account  struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"account"`
}

type bridgeAPIError struct {
	status    int
	code      string
	message   string
	retryable bool
}

func (e *bridgeAPIError) Error() string {
	if e.code == "" {
		return fmt.Sprintf("bridge HTTP %d: %s", e.status, e.message)
	}
	return fmt.Sprintf("bridge %s: %s", e.code, e.message)
}

type desktopBackend struct {
	baseURL    string
	token      string
	owner      string
	client     *http.Client
	sendClient *http.Client
	mu         sync.Mutex
	focused    string
	names      map[string]string
}

func (b *desktopBackend) focus(peer string) {
	b.mu.Lock()
	b.focused = peer
	b.mu.Unlock()
}

func (b *desktopBackend) get(ctx context.Context, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(b.baseURL, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+b.token)
	req.Header.Set("Accept", "application/json")
	resp, err := b.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var envelope struct {
			Error struct {
				Code      string `json:"code"`
				Message   string `json:"message"`
				Retryable bool   `json:"retryable"`
			} `json:"error"`
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = json.Unmarshal(body, &envelope)
		return &bridgeAPIError{
			status:    resp.StatusCode,
			code:      envelope.Error.Code,
			message:   envelope.Error.Message,
			retryable: envelope.Error.Retryable,
		}
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 8<<20))
	return decoder.Decode(out)
}

func (b *desktopBackend) post(ctx context.Context, path string, body []byte, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(b.baseURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+b.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	client := b.sendClient
	if client == nil {
		client = b.client
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var envelope struct {
			Error struct {
				Code      string `json:"code"`
				Message   string `json:"message"`
				Retryable bool   `json:"retryable"`
			} `json:"error"`
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = json.Unmarshal(raw, &envelope)
		return &bridgeAPIError{status: resp.StatusCode, code: envelope.Error.Code, message: envelope.Error.Message, retryable: envelope.Error.Retryable}
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}

func (b *desktopBackend) sync(ctx context.Context) chatSyncResult {
	result := chatSyncResult{
		Contacts: map[string]native.Contact{},
		Previews: map[string]native.Message{},
	}
	var chats bridgeChatsResponse
	if err := b.get(ctx, "/v1/chats?limit=200", &chats); err != nil {
		result.Err = err
		result.Retry = bridgeRetryable(err)
		return result
	}
	b.mu.Lock()
	focus := b.focused
	b.mu.Unlock()
	names := make(map[string]string, len(chats.Chats))
	for _, chat := range chats.Chats {
		names[chat.ID] = chat.Name
		result.Contacts[chat.ID] = native.Contact{Username: chat.ID, Nickname: chat.Name, Official: chat.Kind == "official"}
		if chat.Last != nil {
			result.Previews[chat.ID] = native.Message{
				ID:        chat.Last.ID,
				From:      chat.Last.Sender,
				To:        b.owner,
				Peer:      chat.ID,
				Type:      bridgeTypeCode(chat.Last.Type),
				Text:      chat.Last.Preview,
				Timestamp: bridgeTimestamp(chat.Last.Timestamp),
			}
		}
	}
	b.mu.Lock()
	b.names = names
	b.mu.Unlock()
	if focus == "" {
		for _, chat := range chats.Chats {
			if chat.Kind != "official" {
				focus = chat.ID
				b.focus(focus)
				break
			}
		}
	}
	if focus == "" {
		return result
	}
	var messages bridgeMessagesResponse
	path := "/v1/messages?chat_id=" + url.QueryEscape(focus) + "&limit=100"
	if err := b.get(ctx, path, &messages); err != nil {
		result.Err = err
		result.Retry = bridgeRetryable(err)
		return result
	}
	for _, message := range messages.Messages {
		outgoing := message.Direction == "out"
		from := message.SenderID
		if from == "" {
			if outgoing {
				from = b.owner
			} else {
				from = focus
			}
		}
		result.Messages = append(result.Messages, native.Message{
			ID:        message.ID,
			From:      from,
			To:        focus,
			Peer:      focus,
			Type:      bridgeTypeCode(message.Type),
			Text:      message.Text,
			Timestamp: bridgeTimestamp(message.Timestamp),
			Outgoing:  outgoing,
			Sequence:  message.Sequence,
		})
	}
	return result
}

func (b *desktopBackend) send(ctx context.Context, peer, text string) chatSendResult {
	result := chatSendResult{Peer: peer, Text: text}
	b.mu.Lock()
	name := b.names[peer]
	b.mu.Unlock()
	clientID, err := newClientID()
	if err != nil {
		result.Err = err
		return result
	}
	body, err := json.Marshal(map[string]string{
		"chat_id":   peer,
		"chat_name": name,
		"text":      text,
		"client_id": clientID,
	})
	if err != nil {
		result.Err = err
		return result
	}
	var response bridgeSendResponse
	if err := b.post(ctx, "/v1/send", body, &response); err != nil {
		result.Err = err
		return result
	}
	if !response.Accepted || !response.Verified {
		result.Err = errors.New("bridge did not confirm the message")
		return result
	}
	result.Accepted = true
	result.Message = native.Message{
		ID: response.MessageID, From: b.owner, To: peer, Peer: peer,
		Type: 1, Text: text, Timestamp: bridgeTimestamp(response.Timestamp), Outgoing: true,
	}
	return result
}

func newClientID() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(value[:]), nil
}

func bridgeRetryable(err error) bool {
	var apiErr *bridgeAPIError
	if errors.As(err, &apiErr) {
		return apiErr.retryable || apiErr.status == http.StatusTooManyRequests || apiErr.status >= 500
	}
	return true
}

func bridgeTimestamp(value int64) uint32 {
	if value <= 0 {
		return 0
	}
	if value > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(value)
}

func bridgeTypeCode(kind string) int32 {
	switch kind {
	case "text":
		return 1
	case "image":
		return 3
	case "voice":
		return 34
	case "video":
		return 43
	case "sticker":
		return 47
	case "location":
		return 48
	case "file":
		return 49
	case "call":
		return 50
	case "system":
		return 10000
	case "redpacket":
		return 49
	default:
		return 1
	}
}

func defaultBridgeTokenPath() string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		if dir, err := os.UserConfigDir(); err == nil {
			base = dir
		}
	}
	return filepath.Join(base, "wechat-bridge", "token")
}

func desktopTUI(ctx context.Context, args []string) error {
	f := flags("desktop")
	bridgeURL := f.String("bridge-url", "http://127.0.0.1:8765", "bridge base URL")
	token := f.String("token", "", "bridge bearer token (prefer --token-file)")
	tokenFile := f.String("token-file", defaultBridgeTokenPath(), "file containing the bridge bearer token")
	if err := parse(f, args); err != nil {
		return err
	}
	if !term.IsTerminal(os.Stdin.Fd()) || !term.IsTerminal(os.Stdout.Fd()) {
		return errors.New("the desktop TUI requires a terminal")
	}
	if *token == "" {
		data, err := os.ReadFile(*tokenFile)
		if err != nil {
			return fmt.Errorf("read bridge token: %w", err)
		}
		*token = strings.TrimSpace(string(data))
	}
	if *token == "" {
		return errors.New("bridge token is empty")
	}
	backend := &desktopBackend{
		baseURL:    strings.TrimRight(*bridgeURL, "/"),
		token:      *token,
		client:     &http.Client{Timeout: 30 * time.Second},
		sendClient: &http.Client{Timeout: 150 * time.Second},
		names:      map[string]string{},
	}
	healthCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var health bridgeHealth
	if err := backend.get(healthCtx, "/v1/health", &health); err != nil {
		return fmt.Errorf("bridge health: %w", err)
	}
	if !health.OK {
		return fmt.Errorf("bridge is not ready: %s", health.Message)
	}
	backend.owner = health.Account.Name
	if backend.owner == "" {
		backend.owner = health.Account.ID
	}
	if backend.owner == "" {
		backend.owner = "me"
	}
	model := newChatModel(ctx, backend, backend.owner, "desktop", chatSnapshot{}, false)
	model.readOnly = health.ReadOnly
	if model.readOnly {
		model.composer.Placeholder = "Read-only: sending is disabled"
	}
	program := tea.NewProgram(model, tea.WithAltScreen(), tea.WithMouseCellMotion(), tea.WithReportFocus(), tea.WithContext(ctx))
	if _, err := program.Run(); err != nil && !errors.Is(err, tea.ErrProgramKilled) && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
