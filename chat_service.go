package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/kevmo314/wechat-cli/internal/native"
)

type chatSnapshot struct {
	Messages []native.Message
	Contacts map[string]native.Contact
}

func (s nativeStore) chatSnapshot(owner string) (chatSnapshot, error) {
	result := chatSnapshot{Contacts: map[string]native.Contact{}}
	files, err := filepath.Glob(filepath.Join(s.dir, "batches", "*.json"))
	if err != nil {
		return result, err
	}
	sort.Strings(files)
	seen := map[string]int{}
	for _, path := range files {
		var record nativeJournal
		if err = s.load(filepath.Join("batches", filepath.Base(path)), &record); err != nil {
			return result, err
		}
		if record.Owner != owner {
			continue
		}
		for _, m := range record.orderedMessages() {
			if i, ok := seen[m.ID]; ok {
				native.MergeMessageOrder(&result.Messages[i], m)
			} else {
				seen[m.ID] = len(result.Messages)
				result.Messages = append(result.Messages, m)
			}
		}
		mergeContacts(result.Contacts, record.Batch.Commands)
	}
	native.ProjectMessages(result.Messages)
	native.SortMessages(result.Messages)
	return result, nil
}
func mergeContacts(contacts map[string]native.Contact, commands []native.SyncCommand) {
	for _, cmd := range commands {
		if cmd.ID == 2 {
			if c, err := native.DecodeContact(cmd.Payload); err == nil {
				previous := contacts[c.Username]
				c.Flags = (previous.Flags & ^c.FlagMask) | (c.Flags & c.FlagMask)
				if len(c.Members) == 0 {
					c.Members = previous.Members
				}
				contacts[c.Username] = c
			}
		}
	}
}

type chatSyncResult struct {
	Messages []native.Message
	Contacts map[string]native.Contact
	Previews map[string]native.Message
	More     bool
	Err      error
	Retry    bool
}
type chatSendResult struct {
	ReplyTo    *native.Message
	Peer, Text string
	Message    native.Message
	Accepted   bool
	Err        error
}

type chatService struct {
	store   nativeStore
	owner   string
	mu      sync.Mutex
	session *native.Session
	host    string
}

func (c *chatService) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session != nil {
		c.session.Close()
		c.session = nil
	}
}
func (c *chatService) sync(ctx context.Context) chatSyncResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := chatSyncResult{Contacts: map[string]native.Contact{}}
	lock, err := c.store.lock(ctx)
	if err != nil {
		result.Err = err
		return result
	}
	defer lock.Close()
	state, err := c.store.state()
	if err == nil && (state.Account == nil || state.Account.Username != c.owner) {
		err = errors.New("account changed; reopen WeChat")
	}
	if err == nil && (c.session == nil || c.host != state.Account.Host) {
		if c.session != nil {
			c.session.Close()
			c.session = nil
		}
		c.session, err = native.Connect(ctx, state.Account.Host)
		c.host = state.Account.Host
	}
	var batch native.SyncBatch
	if err == nil {
		batch, err = c.session.Sync(ctx, state.Account, state.Cursor)
	}
	if err != nil {
		if c.session != nil {
			c.session.Close()
			c.session = nil
		}
		result.Err = err
		var network net.Error
		result.Retry = errors.As(err, &network) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, context.DeadlineExceeded)
		return result
	}
	for _, cmd := range batch.Commands {
		if cmd.ID == 5 {
			if m, e := native.DecodeMessage(cmd.Payload, c.owner); e == nil {
				result.Messages = append(result.Messages, m)
			}
		}
	}
	mergeContacts(result.Contacts, batch.Commands)
	if len(batch.Commands) > 0 {
		err = c.store.journal(nativeJournal{Owner: c.owner, Batch: batch, Messages: result.Messages})
	}
	if err == nil {
		state.Cursor = batch.Cursor
		err = c.store.save("state.json", state)
	}
	if err != nil {
		result.Err = fmt.Errorf("could not save synchronized messages: %w", err)
		return result
	}
	result.More = batch.Continue
	return result
}
func (c *chatService) send(ctx context.Context, peer, text string) chatSendResult {
	return c.deliver(ctx, peer, text, nil, "")
}
func (c *chatService) reply(ctx context.Context, target native.Message, text, name string) chatSendResult {
	return c.deliver(ctx, target.Peer, text, &target, name)
}
func (c *chatService) deliver(ctx context.Context, peer, text string, reply *native.Message, name string) chatSendResult {
	result := chatSendResult{Peer: peer, Text: text, ReplyTo: reply}
	lock, err := c.store.lock(ctx)
	if err != nil {
		result.Err = err
		return result
	}
	defer lock.Close()
	state, err := c.store.state()
	if err == nil && (state.Account == nil || state.Account.Username != c.owner) {
		err = errors.New("account changed; reopen WeChat")
	}
	if err != nil {
		result.Err = err
		return result
	}
	session, err := native.Connect(ctx, state.Account.Host)
	if err != nil {
		result.Err = err
		return result
	}
	defer session.Close()
	if reply != nil {
		result.Message, err = session.SendReply(ctx, state.Account, *reply, text, name)
	} else {
		var id string
		id, err = session.SendText(ctx, state.Account, peer, text)
		result.Message = native.Message{ID: id, From: c.owner, To: peer, Peer: peer, Text: text, Type: 1, Timestamp: uint32(time.Now().Unix()), Outgoing: true}
	}
	if err != nil {
		result.Err = err
		return result
	}
	result.Accepted = true
	if err = c.store.journal(nativeJournal{Owner: c.owner, Messages: []native.Message{result.Message}}); err != nil {
		result.Err = fmt.Errorf("accepted by WeChat, but archiving failed: %w", err)
		return result
	}
	if err = c.store.save("state.json", state); err != nil {
		result.Err = fmt.Errorf("accepted by WeChat, but session save failed: %w", err)
	}
	return result
}
