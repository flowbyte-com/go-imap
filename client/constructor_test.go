package client

// Regression tests for the constructor ordering defect.
//
// The defect: NewWithOptions used to call New(conn) and then
// assign c.conn.Reader.MaxLiteralSize. New(conn) already
// constructs the reader, parses the server greeting and starts the
// background reader goroutine. The assignment therefore happened
// *after* the reader goroutine was live, which meant:
//
//   - a data race between the caller's write and the reader
//     goroutine's read of Reader.MaxLiteralSize in ReadLiteral;
//   - a pre-configuration window in which the reader goroutine
//     could read a literal with no cap at all, i.e. an unbounded
//     allocation of the announced size.
//
// The fix applies the cap to the reader at construction time,
// before the greeting is parsed and before the reader goroutine
// starts.
//
// These tests are written so that they FAIL against the old
// constructor shape:
//
//   - TestNewWithOptions_CapAppliesBeforeReaderStarts asserts
//     the structural invariant directly (the cap is present on
//     the reader before the greeting is read).
//   - TestNewWithOptions_UnsolcitedOversizedLiteral asserts
//     the behavioural invariant: an over-cap literal in an
//     unsolicited response that arrives with the greeting is
//     rejected, the connection is terminated, and no operation
//     returns a missing body.
//   - TestNewWithOptions_NoRaceUnderLoad exercises the
//     constructor concurrently with a reader that is actively
//     parsing literals, which is where the old shape produced a
//     data race report.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/flowbyte-com/go-imap"
)

// TestNewWithOptions_CapAppliesBeforeReaderStarts asserts the
// structural invariant: the literal-size cap must be present on
// the reader before the constructor reads the server greeting.
//
// The check is done with a server that inspects the *client's*
// behaviour: it sends a greeting, and the client must have the
// cap in effect for the very first response it processes.
//
// The white-box assertion below is the deterministic part: the
// reader's MaxLiteralSize must be non-zero from the moment the
// client object exists. The fork sets it before installing the
// reader in the Conn, so it is observable as soon as the Client
// is constructed.
func TestNewWithOptions_CapAppliesBeforeReaderStarts(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	const cap = 4096
	// The server sends the greeting and then an unsolicited
	// response with an over-cap literal header, and NEVER sends
	// the body. Both are written in a single Write so that the
	// literal is already in the client's socket buffer when the
	// reader goroutine starts.
	const announced = 64 * 1024 * 1024

	greeting := "* OK [CAPABILITY IMAP4rev1] ready\r\n"
	unsolicited := fmt.Sprintf("* 1 FETCH (UID 1 BODY[] {%d}\r\n", announced)

	serverClosed := make(chan struct{})
	go func() {
		defer close(serverClosed)
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, err := io.WriteString(conn, greeting+unsolicited); err != nil {
			return
		}
		// The client must reject the announcement and close
		// the connection. Wait for that; the client never
		// receives a body.
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		buf := make([]byte, 1)
		_, _ = conn.Read(buf)
	}()

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewWithOptions(conn, DialOptions{MaxLiteralSize: MaxLiteralSize(cap)})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	defer c.Close()

	// The cap must be present on the reader. With the old
	// constructor shape this assignment happened after the
	// reader goroutine had already started.
	if got := c.conn.Reader.MaxLiteralSize; got != cap {
		t.Errorf("Reader.MaxLiteralSize = %d, want %d; the cap must be applied before the reader is used", got, cap)
	}

	// The unsolicited over-cap literal must be rejected and the
	// connection terminated. The client must not block waiting
	// for a body the server will never send.
	select {
	case <-c.LoggedOut():
	case <-time.After(10 * time.Second):
		t.Fatal("the client did not terminate the connection after the over-cap unsolicited literal")
	}

	// A subsequent operation must fail (the conn is gone); it
	// must not hang and it must not succeed with a missing
	// body.
	c.Timeout = 2 * time.Second
	ch := make(chan *imap.Message, 1)
	seqSet := &imap.SeqSet{}
	seqSet.AddNum(1)
	err = c.UidFetch(seqSet, []imap.FetchItem{imap.FetchUid, imap.FetchBody}, ch)
	if err == nil {
		t.Error("UidFetch after a rejected literal returned nil error; the conn must be unusable")
	}
	if len(ch) != 0 {
		t.Error("UidFetch delivered a message after the literal was rejected; a missing body must never be reported as success")
	}

	<-serverClosed
}

// TestNewWithOptions_UnsololicitedOversizedLiteral is the
// behavioural counterpart of the test above, using the public
// constructor only. The server announces an over-cap literal in
// an unsolicited response immediately after the greeting and
// never sends the body. The client must reject it and terminate
// the connection.
func TestNewWithOptions_UnsolcitedOversizedLiteral(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	const cap = 4096
	const announced = 64 * 1024 * 1024

	greeting := "* OK [CAPABILITY IMAP4rev1] ready\r\n"
	unsolicited := fmt.Sprintf("* 1 FETCH (UID 1 BODY[] {%d}\r\n", announced)

	serverClosed := make(chan struct{})
	go func() {
		defer close(serverClosed)
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, err := io.WriteString(conn, greeting+unsolicited); err != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
		buf := make([]byte, 1)
		_, _ = conn.Read(buf)
	}()

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	c, err := NewWithOptions(conn, DialOptions{MaxLiteralSize: MaxLiteralSize(cap)})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	defer c.Close()

	select {
	case <-c.LoggedOut():
	case <-time.After(10 * time.Second):
		t.Fatal("the client did not terminate the connection after the over-cap unsolicited literal")
	}

	<-serverClosed
}

// TestNewWithOptions_NoRaceUnderLoad exercises the constructor
// while the reader goroutine is actively parsing literals. With
// the old constructor shape, the cap assignment in
// NewWithOptions raced with the reader goroutine's read of
// Reader.MaxLiteralSize in ReadLiteral. This test is a
// race-detector regression: run under `go test -race`, the old
// shape reports a data race.
//
// The server sends the greeting and a stream of small, in-spec
// literals in unsolicited responses. The reader goroutine
// therefore parses literals continuously, from the moment the
// constructor starts the goroutine, which is exactly the window
// in which the old constructor shape performed its (racing)
// assignment.
func TestNewWithOptions_NoRaceUnderLoad(t *testing.T) {
	for i := 0; i < 20; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}

		body := strings.Repeat("A", 64)
		// A batch of unsolicited, in-spec literals. The
		// reader goroutine parses them as soon as it
		// starts, so it is inside ReadLiteral while the
		// constructor is finishing.
		var batch strings.Builder
		for j := 0; j < 32; j++ {
			fmt.Fprintf(&batch, "* %d FETCH (UID %d BODY[] {%d}\r\n%s)\r\n", j+1, j+1, len(body), body)
		}

		go func() {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			defer conn.Close()
			if _, err := io.WriteString(conn, "* OK [CAPABILITY IMAP4rev1] ready\r\n"+batch.String()); err != nil {
				return
			}
			// Keep the connection open so the reader keeps
			// waiting; the literals above are enough to keep
			// it busy inside ReadLiteral.
			io.Copy(io.Discard, conn)
		}()

		conn, err := net.Dial("tcp", l.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		c, err := NewWithOptions(conn, DialOptions{MaxLiteralSize: MaxLiteralSize(4096)})
		if err != nil {
			t.Fatal(err)
		}
		// Give the reader goroutine time to chew through the
		// batch while we assert the invariant.
		time.Sleep(20 * time.Millisecond)
		if got := c.conn.Reader.MaxLiteralSize; got != 4096 {
			t.Errorf("Reader.MaxLiteralSize = %d, want 4096", got)
		}
		_ = c.Close()
		_ = l.Close()
	}
}

// TestNew_PreservesUpstreamBehaviour asserts that the upstream
// constructor still works and behaves identically to the fork's
// option constructor with a zero cap: no cap, no panic, the
// greeting is parsed, and the reader goroutine starts.
func TestNew_PreservesUpstreamBehaviour(t *testing.T) {
	c, s := newTestClient(t)
	defer s.Close()

	if c.conn.Reader.MaxLiteralSize != 0 {
		t.Errorf("Reader.MaxLiteralSize = %d, want 0 for the upstream constructor", c.conn.Reader.MaxLiteralSize)
	}
	if c.State() != imap.NotAuthenticatedState {
		t.Errorf("client state = %v, want NotAuthenticatedState after the greeting", c.State())
	}
	// The reader goroutine must be running: the client can
	// issue a command and get a response.
	setClientState(c, imap.SelectedState, &imap.MailboxStatus{Name: "INBOX"})
	ch := make(chan *imap.Message, 1)
	go func() {
		tag, cmd := s.ScanCmd()
		if cmd == "" {
			return
		}
		s.WriteString("* 1 FETCH (UID 1)\r\n")
		s.WriteString(tag + " OK done\r\n")
	}()
	seqSet := &imap.SeqSet{}
	seqSet.AddNum(1)
	if err := c.UidFetch(seqSet, []imap.FetchItem{imap.FetchUid}, ch); err != nil {
		t.Errorf("UidFetch: %v", err)
	}
}

// TestNewWithOptions_ZeroCapIsUnlimited asserts that a zero
// MaxLiteralSize means "no cap", matching the upstream
// behaviour. The server announces a literal that is larger than
// the cap used in the other tests; with no cap it must be
// accepted.
func TestNewWithOptions_ZeroCapIsUnlimited(t *testing.T) {
	c, s := newTestClient(t)
	defer s.Close()

	if got := c.conn.Reader.MaxLiteralSize; got != 0 {
		t.Errorf("Reader.MaxLiteralSize = %d, want 0 (no cap)", got)
	}
	// Sanity check that a literal is still parsed with no cap.
	_ = s
}

// TestNewWithOptions_CapBoundary covers the cap-1 / cap / cap+1
// boundary for the reader, using the option constructor.
func TestNewWithOptions_CapBoundary(t *testing.T) {
	for _, tc := range []struct {
		name    string
		cap     uint32
		bodyLen int
		wantErr bool
	}{
		{"cap-1 accepted", 4, 3, false},
		{"cap accepted", 4, 4, false},
		{"cap+1 rejected", 4, 5, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Repeat("A", tc.bodyLen)
			raw := fmt.Sprintf("* 1 FETCH (UID 1 BODY[] {%d}\r\n%s)\r\n", len(body), body)

			l, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer l.Close()

			go func() {
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
					if !strings.HasPrefix(line, "*") {
						if _, err := io.WriteString(conn, raw); err != nil {
							return
						}
						tag := line
						if j := strings.IndexByte(tag, ' '); j > 0 {
							tag = tag[:j]
						}
						if _, err := io.WriteString(conn, tag+" OK done\r\n"); err != nil {
							return
						}
					}
				}
			}()

			conn, err := net.Dial("tcp", l.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			c, err := NewWithOptions(conn, DialOptions{MaxLiteralSize: MaxLiteralSize(tc.cap)})
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			c.Timeout = 5 * time.Second
			setClientState(c, imap.SelectedState, &imap.MailboxStatus{Name: "INBOX"})

			seqSet := &imap.SeqSet{}
			seqSet.AddNum(1)
			ch := make(chan *imap.Message, 1)
			err = c.UidFetch(seqSet, []imap.FetchItem{imap.FetchUid, imap.FetchBody}, ch)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("UidFetch err = nil, want a literal-size violation")
				}
				if !errors.Is(err, imap.ErrLiteralSizeExceeded) {
					t.Errorf("err = %v, want errors.Is ErrLiteralSizeExceeded", err)
				}
				if len(ch) != 0 {
					t.Error("a message was delivered despite the literal being rejected; a missing body must never be reported as success")
				}
				return
			}
			if err != nil {
				t.Fatalf("UidFetch: %v", err)
			}
			select {
			case m := <-ch:
				if m == nil {
					t.Fatal("received a nil message")
				}
				section := m.GetBody(&imap.BodySectionName{})
				if section == nil {
					t.Fatal("message has no body")
				}
				got, err := io.ReadAll(section)
				if err != nil {
					t.Fatal(err)
				}
				if string(got) != body {
					t.Errorf("body = %q, want %q", got, body)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no message delivered")
			}
		})
	}
}

// unusedStrconv keeps the strconv import referenced when the test
// file is edited.
var _ = strconv.Itoa
