package client

// Regression tests for the final mailbox-publication correction (fork issue
// #3): Client.Mailbox must not expose the mutable working status while SELECT
// or EXAMINE is in progress, and Select must not resurrect a session that the
// reader has already logged out.

import (
	"errors"
	"io"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/flowbyte-com/go-imap"
)

func TestClient_MailboxHidesWorkingStatusDuringSelection(t *testing.T) {
	tests := []struct {
		name     string
		readOnly bool
		command  string
		final    string
	}{
		{name: "SELECT", readOnly: false, command: "SELECT INBOX", final: " OK SELECT completed\r\n"},
		{name: "EXAMINE", readOnly: true, command: "EXAMINE INBOX", final: " OK [READ-ONLY] EXAMINE completed\r\n"},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			c, s := newTestClient(t)
			defer s.Close()

			setClientState(c, imap.AuthenticatedState, nil)
			updates := make(chan Update, 8)
			c.Updates = updates

			done := make(chan selectResult, 1)
			go func() {
				mbox, err := c.Select("INBOX", tt.readOnly)
				done <- selectResult{mbox: mbox, err: err}
			}()

			tag, cmd := s.ScanCmd()
			if cmd != tt.command {
				t.Fatalf("client sent command %q, want %q", cmd, tt.command)
			}

			// The command has been written, so Select has installed its private
			// working status. It must not be visible through Client.Mailbox.
			if mbox := c.Mailbox(); mbox != nil {
				t.Fatalf("Client.Mailbox() = %#v during %s, want nil", mbox, tt.name)
			}

			c.locker.Lock()
			working := c.mailbox
			immutable := c.mailboxImmutable
			c.locker.Unlock()
			if working == nil {
				t.Fatalf("%s did not install a working status", tt.name)
			}
			if immutable {
				t.Fatalf("%s marked its working status as published", tt.name)
			}

			// Unsolicited updates during selection mutate only the working
			// status. The completion snapshot must include them.
			if _, err := s.WriteString("* 42 EXISTS\r\n* 7 RECENT\r\n"); err != nil {
				t.Fatalf("server write: %v", err)
			}
			exists := nextMailboxUpdate(t, updates)
			recent := nextMailboxUpdate(t, updates)
			if exists.Mailbox.Messages != 42 {
				t.Fatalf("EXISTS update Messages = %d, want 42", exists.Mailbox.Messages)
			}
			if recent.Mailbox.Recent != 7 {
				t.Fatalf("RECENT update Recent = %d, want 7", recent.Mailbox.Recent)
			}
			if mbox := c.Mailbox(); mbox != nil {
				t.Fatalf("Client.Mailbox() = %#v after in-progress updates, want nil", mbox)
			}

			if _, err := s.WriteString(tag + tt.final); err != nil {
				t.Fatalf("server write: %v", err)
			}

			var result selectResult
			select {
			case result = <-done:
			case <-time.After(5 * time.Second):
				t.Fatalf("%s did not return", tt.name)
			}
			if result.err != nil {
				t.Fatalf("%s returned error %v", tt.name, result.err)
			}
			if result.mbox == nil {
				t.Fatalf("%s returned a nil status", tt.name)
			}
			if result.mbox.Messages != 42 || result.mbox.Recent != 7 {
				t.Fatalf("completion snapshot = %#v, want 42 messages and 7 recent", result.mbox)
			}
			if result.mbox.ReadOnly != tt.readOnly {
				t.Fatalf("completion snapshot ReadOnly = %v, want %v", result.mbox.ReadOnly, tt.readOnly)
			}
			if latest := c.Mailbox(); latest != result.mbox {
				t.Fatalf("Client.Mailbox() = %#v, want the completion snapshot %#v", latest, result.mbox)
			}
			if state := c.State(); state != imap.SelectedState {
				t.Fatalf("client state = %v, want %v", state, imap.SelectedState)
			}

			// The published snapshot is immutable and earlier pointers keep
			// their values when a later update installs a replacement.
			if _, err := s.WriteString("* 43 EXISTS\r\n"); err != nil {
				t.Fatalf("server write: %v", err)
			}
			update := nextMailboxUpdate(t, updates)
			if result.mbox.Messages != 42 {
				t.Fatalf("completion snapshot changed in place: Messages = %d, want 42", result.mbox.Messages)
			}
			if latest := c.Mailbox(); latest != update.Mailbox || latest.Messages != 43 {
				t.Fatalf("Client.Mailbox() = %#v, want the latest 43-message snapshot", latest)
			}
		})
	}
}

// TestClient_SelectCompletionAfterBYEDoesNotRestoreSession holds Select inside
// its command write, then feeds the tagged completion and a following * BYE
// through the real response handlers. Both are therefore processed before
// Select resumes at the executor's select, making the logout-before-publication
// interleaving deterministic.
func TestClient_SelectCompletionAfterBYEDoesNotRestoreSession(t *testing.T) {
	conn := newBlockingWriteConn()
	c := newExecuteTestClient(conn)
	c.handleUnilateral()

	done := make(chan selectResult, 1)
	go func() {
		mbox, err := c.Select("INBOX", false)
		done <- selectResult{mbox: mbox, err: err}
	}()

	var command []byte
	select {
	case command = <-conn.started:
	case <-time.After(5 * time.Second):
		t.Fatal("Select did not start writing the command")
	}
	fields := strings.Fields(string(command))
	if len(fields) == 0 {
		t.Fatalf("empty command written: %q", command)
	}
	tag := fields[0]

	if mbox := c.Mailbox(); mbox != nil {
		t.Fatalf("Client.Mailbox() = %#v during SELECT, want nil", mbox)
	}

	if err := c.handle(imap.NewUntaggedResp([]interface{}{uint32(5), "EXISTS"})); err != nil {
		t.Fatalf("handle EXISTS: %v", err)
	}
	if err := c.handle(imap.NewUntaggedResp([]interface{}{uint32(2), "RECENT"})); err != nil {
		t.Fatalf("handle RECENT: %v", err)
	}

	// Wire order: the tagged completion is handled first, then BYE.
	if err := c.handle(&imap.StatusResp{
		Tag:  tag,
		Type: imap.StatusRespOk,
		Info: "SELECT completed",
	}); err != nil {
		t.Fatalf("handle tagged completion: %v", err)
	}
	if err := c.handle(&imap.StatusResp{
		Tag:  "*",
		Type: imap.StatusRespBye,
		Info: "logging out",
	}); err != nil {
		t.Fatalf("handle BYE: %v", err)
	}

	// Simulate the reader exiting after BYE, then let Select resume.
	close(c.loggedOut)
	close(conn.release)

	var result selectResult
	select {
	case result = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Select did not return")
	}

	// Issue #1: the valid tagged completion remains the command result.
	if result.err != nil {
		t.Fatalf("Select returned error %v, want the tagged OK result", result.err)
	}
	if result.mbox == nil || result.mbox.Messages != 5 || result.mbox.Recent != 2 {
		t.Fatalf("Select returned %#v, want the 5-message, 2-recent completion snapshot", result.mbox)
	}

	// The client must still reflect the logout the reader established.
	if state := c.State(); state != imap.LogoutState {
		t.Fatalf("client state = %v, want %v", state, imap.LogoutState)
	}
	if mbox := c.Mailbox(); mbox != nil {
		t.Fatalf("Client.Mailbox() = %#v after BYE, want nil", mbox)
	}

	c.locker.Lock()
	internal := c.mailbox
	immutable := c.mailboxImmutable
	c.locker.Unlock()
	if internal != nil {
		t.Fatalf("Select reinstalled mailbox %#v after BYE", internal)
	}
	if immutable {
		t.Fatal("Select marked a mailbox as published after BYE")
	}
}

func TestClient_SelectTaggedFailureLeavesNoPublishedMailbox(t *testing.T) {
	tests := []struct {
		name      string
		status    string
		bye       bool
		wantState imap.ConnState
	}{
		{name: "NO", status: "NO", wantState: imap.AuthenticatedState},
		{name: "BAD", status: "BAD", wantState: imap.AuthenticatedState},
		{name: "NO_then_BYE", status: "NO", bye: true, wantState: imap.LogoutState},
		{name: "BAD_then_BYE", status: "BAD", bye: true, wantState: imap.LogoutState},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			c, s := newTestClient(t)
			defer s.Close()

			setClientState(c, imap.AuthenticatedState, nil)

			done := make(chan selectResult, 1)
			go func() {
				mbox, err := c.Select("INBOX", false)
				done <- selectResult{mbox: mbox, err: err}
			}()

			tag, cmd := s.ScanCmd()
			if cmd != "SELECT INBOX" {
				t.Fatalf("client sent command %q, want SELECT INBOX", cmd)
			}
			if mbox := c.Mailbox(); mbox != nil {
				t.Fatalf("Client.Mailbox() = %#v during SELECT, want nil", mbox)
			}

			if _, err := s.WriteString("* 9 EXISTS\r\n"); err != nil {
				t.Fatalf("server write: %v", err)
			}
			wire := tag + " " + tt.status + " selection failed\r\n"
			if tt.bye {
				wire += "* BYE logging out\r\n"
			}
			if _, err := s.WriteString(wire); err != nil {
				t.Fatalf("server write: %v", err)
			}

			var result selectResult
			select {
			case result = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Select did not return")
			}
			if result.err == nil || result.err.Error() != "selection failed" {
				t.Fatalf("Select error = %v, want the tagged %s error", result.err, tt.status)
			}
			if result.mbox != nil {
				t.Fatalf("Select returned mailbox %#v on failure, want nil", result.mbox)
			}
			if tt.bye {
				select {
				case <-c.LoggedOut():
				case <-time.After(5 * time.Second):
					t.Fatal("client did not observe the BYE")
				}
			}
			if state := c.State(); state != tt.wantState {
				t.Fatalf("client state = %v, want %v", state, tt.wantState)
			}
			if mbox := c.Mailbox(); mbox != nil {
				t.Fatalf("Client.Mailbox() = %#v after failed SELECT, want nil", mbox)
			}

			c.locker.Lock()
			internal := c.mailbox
			immutable := c.mailboxImmutable
			c.locker.Unlock()
			if internal != nil || immutable {
				t.Fatalf("failed SELECT left mailbox %#v (immutable=%v)", internal, immutable)
			}
		})
	}
}

func TestClient_InterruptedSelectLeavesNoPublishedMailbox(t *testing.T) {
	c, s := newTestClient(t)
	defer s.Close()

	setClientState(c, imap.AuthenticatedState, nil)

	done := make(chan selectResult, 1)
	go func() {
		mbox, err := c.Select("INBOX", false)
		done <- selectResult{mbox: mbox, err: err}
	}()

	if _, cmd := s.ScanCmd(); cmd != "SELECT INBOX" {
		t.Fatalf("client sent command %q, want SELECT INBOX", cmd)
	}
	if mbox := c.Mailbox(); mbox != nil {
		t.Fatalf("Client.Mailbox() = %#v during SELECT, want nil", mbox)
	}

	if _, err := s.WriteString("* 9 EXISTS\r\n* BYE interrupted\r\n"); err != nil {
		t.Fatalf("server write: %v", err)
	}

	var result selectResult
	select {
	case result = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Select did not return")
	}
	if result.err == nil || !errors.Is(result.err, io.EOF) {
		t.Fatalf("Select error = %v, want a connection-closed error", result.err)
	}
	if result.mbox != nil {
		t.Fatalf("Select returned mailbox %#v on interruption, want nil", result.mbox)
	}
	if state := c.State(); state != imap.LogoutState {
		t.Fatalf("client state = %v, want %v", state, imap.LogoutState)
	}
	if mbox := c.Mailbox(); mbox != nil {
		t.Fatalf("Client.Mailbox() = %#v after interruption, want nil", mbox)
	}

	c.locker.Lock()
	internal := c.mailbox
	immutable := c.mailboxImmutable
	c.locker.Unlock()
	if internal != nil || immutable {
		t.Fatalf("interrupted SELECT left mailbox %#v (immutable=%v)", internal, immutable)
	}
}

func TestClient_ConcurrentMailboxReadsDuringSelection(t *testing.T) {
	c, s := newTestClient(t)
	defer s.Close()

	setClientState(c, imap.AuthenticatedState, nil)
	updates := make(chan Update, 8)
	c.Updates = updates

	done := make(chan selectResult, 1)
	go func() {
		mbox, err := c.Select("INBOX", false)
		done <- selectResult{mbox: mbox, err: err}
	}()

	tag, cmd := s.ScanCmd()
	if cmd != "SELECT INBOX" {
		t.Fatalf("client sent command %q, want SELECT INBOX", cmd)
	}

	var nonNil atomic.Bool
	stop := make(chan struct{})
	ready := make(chan struct{}, 8)
	var wg sync.WaitGroup

	const readers = 8
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()

			read := func() {
				if mbox := c.Mailbox(); mbox != nil {
					nonNil.Store(true)
					_ = mbox.Messages
					_ = mbox.Recent
					_ = mbox.ReadOnly
					_ = len(mbox.Flags)
					_ = len(mbox.Items)
				}
			}

			read()
			ready <- struct{}{}

			for {
				select {
				case <-stop:
					return
				default:
				}
				read()
				runtime.Gosched()
			}
		}()
	}

	for i := 0; i < readers; i++ {
		<-ready
	}

	if _, err := s.WriteString("* 11 EXISTS\r\n* 4 RECENT\r\n"); err != nil {
		t.Fatalf("server write: %v", err)
	}
	nextMailboxUpdate(t, updates)
	nextMailboxUpdate(t, updates)

	if mbox := c.Mailbox(); mbox != nil {
		t.Fatalf("Client.Mailbox() = %#v during SELECT, want nil", mbox)
	}

	close(stop)
	wg.Wait()

	if nonNil.Load() {
		t.Fatal("Client.Mailbox exposed the mutable working status during SELECT")
	}

	if _, err := s.WriteString(tag + " OK SELECT completed\r\n"); err != nil {
		t.Fatalf("server write: %v", err)
	}

	var result selectResult
	select {
	case result = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Select did not return")
	}
	if result.err != nil {
		t.Fatalf("Select returned error %v", result.err)
	}
	if result.mbox == nil || result.mbox.Messages != 11 || result.mbox.Recent != 4 {
		t.Fatalf("Select returned %#v, want the 11-message, 4-recent completion snapshot", result.mbox)
	}
	if latest := c.Mailbox(); latest != result.mbox {
		t.Fatalf("Client.Mailbox() = %#v, want the completion snapshot %#v", latest, result.mbox)
	}
}
