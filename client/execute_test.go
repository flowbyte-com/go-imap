package client

// Regression tests for the executor's loggedOut/tagged-completion race
// (fork issue #1).
//
// The reader sends a command's tagged completion to doneHandle before it
// processes a later * BYE and closes loggedOut. When both channels are ready,
// Go's select may still choose loggedOut. execute must therefore re-check
// doneHandle when the loggedOut case is selected, not only before the select.

import (
	"bufio"
	"errors"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flowbyte-com/go-imap"
	"github.com/flowbyte-com/go-imap/commands"
	"github.com/flowbyte-com/go-imap/responses"
)

// blockingWriteConn lets a test hold execute() inside the command write. That
// makes the state at the following select deterministic: the tagged result and
// the logout can both be ready before the executor evaluates it.
type blockingWriteConn struct {
	started chan []byte
	release chan struct{}
	once    sync.Once
}

func newBlockingWriteConn() *blockingWriteConn {
	return &blockingWriteConn{
		started: make(chan []byte, 1),
		release: make(chan struct{}),
	}
}

func (c *blockingWriteConn) Write(p []byte) (int, error) {
	c.once.Do(func() {
		c.started <- append([]byte(nil), p...)
	})
	<-c.release
	return len(p), nil
}

func (c *blockingWriteConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *blockingWriteConn) Close() error                     { return nil }
func (c *blockingWriteConn) LocalAddr() net.Addr              { return nil }
func (c *blockingWriteConn) RemoteAddr() net.Addr             { return nil }
func (c *blockingWriteConn) SetDeadline(time.Time) error      { return nil }
func (c *blockingWriteConn) SetReadDeadline(time.Time) error  { return nil }
func (c *blockingWriteConn) SetWriteDeadline(time.Time) error { return nil }

func newExecuteTestClient(conn net.Conn) *Client {
	continues := make(chan bool)
	return &Client{
		conn:      imap.NewConn(conn, imap.NewReader(nil), imap.NewClientWriter(nil, continues)),
		loggedOut: make(chan struct{}),
		continues: continues,
		state:     imap.AuthenticatedState,
		ErrorLog:  log.New(io.Discard, "", 0),
	}
}

// TestExecute_QueuedTaggedResultWinsWhenLogoutSelected arranges for a tagged
// completion and a logout to be ready together, then runs the real execute
// select. Whether select chooses doneHandle or loggedOut, execute must return
// the original tagged status.
func TestExecute_QueuedTaggedResultWinsWhenLogoutSelected(t *testing.T) {
	for _, statusType := range []imap.StatusRespType{
		imap.StatusRespOk,
		imap.StatusRespNo,
		imap.StatusRespBad,
	} {
		statusType := statusType
		t.Run(string(statusType), func(t *testing.T) {
			for i := 0; i < 128; i++ {
				conn := newBlockingWriteConn()
				c := newExecuteTestClient(conn)

				done := make(chan struct{})
				var gotStatus *imap.StatusResp
				var gotErr error
				go func() {
					gotStatus, gotErr = c.execute(new(commands.Noop), nil)
					close(done)
				}()

				var command []byte
				select {
				case command = <-conn.started:
				case <-time.After(5 * time.Second):
					t.Fatal("execute did not start writing the command")
				}

				fields := strings.Fields(string(command))
				if len(fields) == 0 {
					t.Fatalf("empty command written: %q", command)
				}
				tag := fields[0]

				want := &imap.StatusResp{
					Tag:  tag,
					Type: statusType,
					Info: "tagged completion",
				}
				if err := c.handle(want); err != nil {
					t.Fatalf("handle tagged completion: %v", err)
				}

				// Both the result and the logout are now ready. Release the
				// command write so execute evaluates its select.
				close(c.loggedOut)
				close(conn.release)

				select {
				case <-done:
				case <-time.After(5 * time.Second):
					t.Fatal("execute did not return")
				}

				if gotErr != nil {
					t.Fatalf("iteration %d: execute returned error %v, want the tagged %s result", i, gotErr, statusType)
				}
				if gotStatus != want {
					t.Fatalf("iteration %d: execute returned status %#v, want the original tagged status %#v", i, gotStatus, want)
				}
			}
		})
	}
}

// TestExecute_QueuedHandlerErrorWinsWhenLogoutSelected covers a command
// handler error queued before the logout. It must be returned unchanged.
func TestExecute_QueuedHandlerErrorWinsWhenLogoutSelected(t *testing.T) {
	sentinel := errors.New("handler failure")

	for i := 0; i < 128; i++ {
		conn := newBlockingWriteConn()
		c := newExecuteTestClient(conn)

		done := make(chan struct{})
		var gotStatus *imap.StatusResp
		var gotErr error
		go func() {
			h := responses.HandlerFunc(func(imap.Resp) error { return sentinel })
			gotStatus, gotErr = c.execute(new(commands.Noop), h)
			close(done)
		}()

		select {
		case <-conn.started:
		case <-time.After(5 * time.Second):
			t.Fatal("execute did not start writing the command")
		}

		if err := c.handle(&imap.DataResp{Tag: "*"}); err != nil {
			t.Fatalf("handle response: %v", err)
		}

		close(c.loggedOut)
		close(conn.release)

		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("execute did not return")
		}

		if !errors.Is(gotErr, sentinel) {
			t.Fatalf("iteration %d: execute error = %v, want handler error %v", i, gotErr, sentinel)
		}
		if gotStatus != nil {
			t.Fatalf("iteration %d: execute status = %#v, want nil", i, gotStatus)
		}
	}
}

// TestClient_TaggedCompletionImmediatelyFollowedByBYE exercises the wire
// ordering from issue #1: the tagged completion and * BYE are written
// together, so the reader can process both before execute resumes.
func TestClient_TaggedCompletionImmediatelyFollowedByBYE(t *testing.T) {
	cases := []struct {
		name     string
		status   string
		wantErr  string
		wantFail bool
	}{
		{name: "OK", status: "OK"},
		{name: "NO", status: "NO", wantErr: "completion", wantFail: true},
		{name: "BAD", status: "BAD", wantErr: "completion", wantFail: true},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			for i := 0; i < 64; i++ {
				c, s := newTestClient(t)

				done := make(chan error, 1)
				go func() {
					done <- c.Noop()
				}()

				tag, cmd := s.ScanCmd()
				if cmd != "NOOP" {
					t.Fatalf("client sent %q, want NOOP", cmd)
				}

				if _, err := s.WriteString(tag + " " + tc.status + " completion\r\n* BYE logging out\r\n"); err != nil {
					t.Fatalf("server write: %v", err)
				}

				select {
				case err := <-done:
					if tc.wantFail {
						if err == nil {
							t.Fatalf("iteration %d: Noop returned nil, want tagged %s error", i, tc.status)
						}
						if err.Error() != tc.wantErr {
							t.Fatalf("iteration %d: Noop error = %q, want tagged status %q", i, err, tc.wantErr)
						}
					} else if err != nil {
						t.Fatalf("iteration %d: Noop returned error %v, want tagged OK result", i, err)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("Noop did not return")
				}

				s.Close()
			}
		})
	}
}

// TestClient_BYEWithoutTaggedCompletion verifies that a logout with no
// command result is still a disconnect, not a success.
func TestClient_BYEWithoutTaggedCompletion(t *testing.T) {
	for i := 0; i < 16; i++ {
		c, s := newTestClient(t)

		done := make(chan error, 1)
		go func() {
			done <- c.Noop()
		}()

		if _, cmd := s.ScanCmd(); cmd != "NOOP" {
			t.Fatalf("client sent %q, want NOOP", cmd)
		}
		if _, err := s.WriteString("* BYE logging out\r\n"); err != nil {
			t.Fatalf("server write: %v", err)
		}

		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("iteration %d: Noop returned nil after BYE without completion", i)
			}
			if !errors.Is(err, io.EOF) {
				t.Fatalf("iteration %d: Noop error = %v, want connection-closed error", i, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Noop did not return")
		}

		s.Close()
	}
}

// TestClient_ConnectionFailureBeforeCompletion verifies that a genuine
// disconnect still propagates as an error.
func TestClient_ConnectionFailureBeforeCompletion(t *testing.T) {
	for i := 0; i < 16; i++ {
		c, s := newTestClient(t)

		done := make(chan error, 1)
		go func() {
			done <- c.Noop()
		}()

		if _, cmd := s.ScanCmd(); cmd != "NOOP" {
			t.Fatalf("client sent %q, want NOOP", cmd)
		}
		s.Conn.Close()

		select {
		case err := <-done:
			if err == nil {
				t.Fatalf("iteration %d: Noop returned nil after the connection failed", i)
			}
			if !errors.Is(err, io.EOF) {
				t.Fatalf("iteration %d: Noop error = %v, want connection-closed error", i, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Noop did not return")
		}

		s.Listener.Close()
	}
}

// TestClient_ParserFailureBeforeCompletion verifies that a parser failure
// still terminates the waiting command with the parse cause.
func TestClient_ParserFailureBeforeCompletion(t *testing.T) {
	c, s := newTestClient(t)
	defer s.Close()

	done := make(chan error, 1)
	go func() {
		done <- c.Noop()
	}()

	if _, cmd := s.ScanCmd(); cmd != "NOOP" {
		t.Fatalf("client sent %q, want NOOP", cmd)
	}
	if _, err := s.WriteString("* 1 FETCH (BODY[] \"unterminated\r\n"); err != nil {
		t.Fatalf("server write: %v", err)
	}

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Noop returned nil after a parser failure")
		}
		if !strings.Contains(err.Error(), "quoted string") {
			t.Fatalf("Noop error = %v, want the parser cause", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Noop did not return")
	}
}

// TestClient_DecoderFailureBeforeCompletion verifies that a decoder-limit
// failure is still reported as a typed connection close, not as success.
func TestClient_DecoderFailureBeforeCompletion(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	serverDone := make(chan struct{})
	go func() {
		defer close(serverDone)
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()

		if _, err := io.WriteString(conn, "* OK [CAPABILITY IMAP4rev1] ready\r\n"); err != nil {
			return
		}

		br := bufio.NewReader(conn)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if strings.HasPrefix(line, "*") {
				continue
			}
			// Over the client's 16-byte literal cap. The body is never sent.
			_, _ = io.WriteString(conn, "* 1 FETCH (BODY[] {1024}\r\n")
			buf := make([]byte, 1)
			_, _ = conn.Read(buf)
			return
		}
	}()

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewWithOptions(conn, DialOptions{MaxLiteralSize: 16})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	done := make(chan error, 1)
	go func() {
		_, err := c.execute(new(commands.Noop), nil)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("execute returned nil after a decoder-limit failure")
		}
		if !errors.Is(err, imap.ErrLiteralSizeExceeded) {
			t.Fatalf("execute error = %v, want errors.Is ErrLiteralSizeExceeded", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("execute did not return")
	}

	select {
	case <-serverDone:
	case <-time.After(5 * time.Second):
		t.Fatal("server did not observe the client closing the connection")
	}
}
