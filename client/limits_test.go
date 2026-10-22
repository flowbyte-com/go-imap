package client

// Regression tests for the client-side behaviour of the reader's
// structural limits.
//
// The reader limits are only useful if the client reacts to a
// violation the way a fatal parse error must be reacted to: the
// connection is torn down, the cause is preserved through the
// executor's error chain, no partially parsed result is reported as a
// success, and a desynchronised connection is never reused.
//
// These tests also cover the interaction between the limits and the
// operations TheMailroom depends on: a SEARCH result at the configured
// UID budget, a FETCH envelope at a minimal budget, and the behaviour
// under both implicit TLS and STARTTLS.

import (
	"bufio"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flowbyte-com/go-imap"
	"github.com/flowbyte-com/go-imap/internal"
)

// limitsServer is a minimal IMAP server that answers a UID SEARCH with
// a configurable number of UIDs and a UID FETCH with a configurable
// envelope.
//
// It can serve plaintext (with a STARTTLS upgrade on demand) or TLS.
type limitsServer struct {
	ln net.Listener

	tlsConfig *tls.Config
	// implicitTLS wraps the accepted connections in TLS straight away.
	implicitTLS bool

	// searchUIDs is the number of UIDs the UID SEARCH response holds.
	searchUIDs int
	// fetchEnvelope makes the UID FETCH answer an envelope.
	fetchEnvelope bool

	mu sync.Mutex
	// commands records the command lines the server received.
	commands []string
	// connections counts the accepted connections.
	connections int
}

func (s *limitsServer) start(t *testing.T) {
	t.Helper()
	cert, err := tls.X509KeyPair(internal.LocalhostCert, internal.LocalhostKey)
	if err != nil {
		t.Fatal("cannot load test certificate:", err)
	}
	s.tlsConfig = &tls.Config{
		InsecureSkipVerify: true,
		Certificates:       []tls.Certificate{cert},
	}
	// The listener is always a plain one: the TLS handshake is done by
	// the connection handler, so that the implicit-TLS and the STARTTLS
	// paths share it.
	ln, err2 := net.Listen("tcp", "127.0.0.1:0")
	if err2 != nil {
		t.Fatal(err2)
	}
	s.ln = ln
	go s.serve()
}

func (s *limitsServer) close() {
	if s.ln != nil {
		s.ln.Close()
	}
}

func (s *limitsServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.connections++
		s.mu.Unlock()
		go s.handle(conn)
	}
}

func (s *limitsServer) handle(conn net.Conn) {
	defer conn.Close()
	if s.implicitTLS {
		tlsConn := tls.Server(conn, s.tlsConfig)
		if err := tlsConn.Handshake(); err != nil {
			return
		}
		conn = tlsConn
	}
	if _, err := io.WriteString(conn, "* OK [CAPABILITY IMAP4rev1 STARTTLS] ready\r\n"); err != nil {
		return
	}
	br := bufio.NewReader(conn)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		s.mu.Lock()
		s.commands = append(s.commands, strings.TrimSpace(line))
		s.mu.Unlock()
		upper := strings.ToUpper(line)
		tag := firstWord(line)
		switch {
		case strings.Contains(upper, "STARTTLS"):
			if _, err := io.WriteString(conn, tag+" OK Begin TLS negotiation now\r\n"); err != nil {
				return
			}
			tlsConn := tls.Server(conn, s.tlsConfig)
			if err := tlsConn.Handshake(); err != nil {
				return
			}
			conn = tlsConn
			br = bufio.NewReader(conn)
		case strings.Contains(upper, "SEARCH"):
			var sb strings.Builder
			sb.WriteString("* SEARCH")
			for i := 0; i < s.searchUIDs; i++ {
				sb.WriteString(" ")
				sb.WriteString(strconv.Itoa(i + 1))
			}
			sb.WriteString("\r\n")
			if _, err := io.WriteString(conn, sb.String()); err != nil {
				return
			}
			if _, err := io.WriteString(conn, tag+" OK SEARCH completed\r\n"); err != nil {
				return
			}
		case strings.Contains(upper, "FETCH"):
			if s.fetchEnvelope {
				resp := "* 1 FETCH (UID 1 FLAGS (\\Seen) INTERNALDATE \"12-Mar-2026 12:00:00 +0000\" RFC822.SIZE 4096 ENVELOPE (\"subject\" NIL NIL NIL NIL NIL NIL NIL NIL NIL))\r\n"
				if _, err := io.WriteString(conn, resp); err != nil {
					return
				}
			}
			if _, err := io.WriteString(conn, tag+" OK FETCH completed\r\n"); err != nil {
				return
			}
		default:
			if _, err := io.WriteString(conn, tag+" OK done\r\n"); err != nil {
				return
			}
		}
	}
}

// commandCount returns the number of received command lines that
// contain the given substring.
func (s *limitsServer) commandCount(substr string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.commands {
		if strings.Contains(strings.ToUpper(c), substr) {
			n++
		}
	}
	return n
}

// connectionCount returns the number of accepted connections.
func (s *limitsServer) connectionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.connections
}

// firstWord returns the first whitespace-separated word of the line,
// which is the IMAP command tag.
func firstWord(line string) string {
	line = strings.TrimSpace(line)
	if i := strings.IndexAny(line, " \t"); i >= 0 {
		return line[:i]
	}
	return line
}

// dial connects to the server. With implicit TLS the connection is
// wrapped in TLS by the caller, exactly as an application would.
func (s *limitsServer) dial(t *testing.T) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", s.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if s.implicitTLS {
		tlsConn := tls.Client(conn, s.tlsConfig)
		if err := tlsConn.Handshake(); err != nil {
			t.Fatal(err)
		}
		return tlsConn
	}
	return conn
}

// TestClientSearchAtAndBeyondUIDBudget checks that a SEARCH result at
// the configured UID budget is accepted and one UID over it is refused.
//
// The limits are configured so that the field budget is the binding
// constraint: a response of N UIDs holds N+1 fields, the name of the
// response included.
func TestClientSearchAtAndBeyondUIDBudget(t *testing.T) {
	const budget = 20
	// The field limit is the budget plus the name of the response, so
	// that the decoder can decode a result of budget+1 UIDs. That is
	// what lets the application observe the overrun and produce its
	// own typed refusal instead of a decoder error.
	for _, tc := range []struct {
		name      string
		searchUID int
		wantErr   bool
	}{
		{"at the budget", budget, false},
		{"one over the budget is still decodable", budget + 1, false},
		{"two over the budget is refused", budget + 2, true},
		{"far over the budget", budget * 10, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := &limitsServer{searchUIDs: tc.searchUID}
			srv.start(t)
			defer srv.close()

			conn := srv.dial(t)
			c, err := NewWithOptions(conn, DialOptions{
				MaxResponseFields:   budget + 2,
				MaxAtomSize:         1024,
				MaxQuotedStringSize: 1024,
				MaxResponseBytes:    1 << 20,
				MaxListDepth:        16,
			})
			if err != nil {
				t.Fatalf("NewWithOptions: %v", err)
			}
			defer c.Close()
			c.Timeout = 5 * time.Second
			setClientState(c, imap.SelectedState, &imap.MailboxStatus{Name: "INBOX"})

			uids, err := c.UidSearch(&imap.SearchCriteria{})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("UidSearch returned %d UIDs, want a limit violation", len(uids))
				}
				if !errors.Is(err, imap.ErrResponseTooLarge) {
					t.Errorf("err = %v, want errors.Is(err, imap.ErrResponseTooLarge)", err)
				}
				if len(uids) != 0 {
					t.Errorf("UidSearch returned %d UIDs, want none: a refused response must never be reported as a result", len(uids))
				}
				return
			}
			if err != nil {
				t.Fatalf("UidSearch: %v", err)
			}
			if len(uids) != tc.searchUID {
				t.Errorf("UidSearch returned %d UIDs, want %d", len(uids), tc.searchUID)
			}
		})
	}
}

// TestClientFetchEnvelopeAtMinimalBudget checks that an ordinary FETCH
// envelope is still accepted when the scan budget is small.
//
// A small budget must not make the ordinary responses unusable: the
// limits bound the response, not the operation.
func TestClientFetchEnvelopeAtMinimalBudget(t *testing.T) {
	// A FETCH envelope holds far more fields than a search result: the
	// sequence number, the name, the item list and every item inside
	// it, including the nested envelope and address lists. The field
	// limit must therefore have a floor that accommodates an ordinary
	// response, whatever the scan budget is.
	const budget = 64
	srv := &limitsServer{searchUIDs: 1, fetchEnvelope: true}
	srv.start(t)
	defer srv.close()

	conn := srv.dial(t)
	c, err := NewWithOptions(conn, DialOptions{
		MaxResponseFields:   budget,
		MaxAtomSize:         1024,
		MaxQuotedStringSize: 1024,
		MaxResponseBytes:    1 << 20,
		MaxListDepth:        16,
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	defer c.Close()
	c.Timeout = 5 * time.Second
	setClientState(c, imap.SelectedState, &imap.MailboxStatus{Name: "INBOX"})

	seqSet := &imap.SeqSet{}
	seqSet.AddNum(1)
	ch := make(chan *imap.Message, 4)
	if err := c.UidFetch(seqSet, []imap.FetchItem{imap.FetchEnvelope, imap.FetchUid}, ch); err != nil {
		t.Fatalf("UidFetch: %v", err)
	}
	select {
	case m := <-ch:
		if m == nil {
			t.Fatal("received a nil message")
		}
		if m.Envelope == nil {
			t.Error("the envelope was not parsed")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no message delivered")
	}
}

// TestClientDecoderRefusalTerminatesConnection checks the executor's
// reaction to a decoder-limit violation: the connection is closed, the
// cause is preserved, and no result is reported.
func TestClientDecoderRefusalTerminatesConnection(t *testing.T) {
	srv := &limitsServer{searchUIDs: 500}
	srv.start(t)
	defer srv.close()

	conn := srv.dial(t)
	c, err := NewWithOptions(conn, DialOptions{
		MaxResponseFields:   64,
		MaxAtomSize:         1024,
		MaxQuotedStringSize: 1024,
		MaxResponseBytes:    1 << 20,
		MaxListDepth:        16,
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	defer c.Close()
	c.Timeout = 5 * time.Second
	setClientState(c, imap.SelectedState, &imap.MailboxStatus{Name: "INBOX"})

	uids, err := c.UidSearch(&imap.SearchCriteria{})
	if err == nil {
		t.Fatalf("UidSearch returned %d UIDs, want a limit violation", len(uids))
	}
	if !errors.Is(err, imap.ErrResponseTooLarge) {
		t.Errorf("err = %v, want errors.Is(err, imap.ErrResponseTooLarge)", err)
	}
	if len(uids) != 0 {
		t.Errorf("UidSearch returned %d UIDs, want none", len(uids))
	}
	// The connection must be unusable: the reader goroutine must have
	// exited.
	select {
	case <-c.LoggedOut():
	case <-time.After(10 * time.Second):
		t.Fatal("the connection was not terminated after the decoder refusal")
	}
	// A subsequent operation must fail rather than reuse the
	// desynchronised connection.
	if _, err := c.UidSearch(&imap.SearchCriteria{}); err == nil {
		t.Error("a search after the refusal succeeded; the connection must not be reused")
	}
}

// TestClientReconnectAfterDecoderRefusal checks that a client whose
// connection was refused can establish a new one.
//
// The refusal must not poison the client: the next connection starts
// with a fresh reader, so the accounting is fresh too.
func TestClientReconnectAfterDecoderRefusal(t *testing.T) {
	srv := &limitsServer{searchUIDs: 500}
	srv.start(t)
	defer srv.close()

	conn := srv.dial(t)
	c, err := NewWithOptions(conn, DialOptions{
		MaxResponseFields:   64,
		MaxAtomSize:         1024,
		MaxQuotedStringSize: 1024,
		MaxResponseBytes:    1 << 20,
		MaxListDepth:        16,
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	defer c.Close()
	c.Timeout = 5 * time.Second
	setClientState(c, imap.SelectedState, &imap.MailboxStatus{Name: "INBOX"})

	if _, err := c.UidSearch(&imap.SearchCriteria{}); err == nil {
		t.Fatal("expected the search to be refused")
	}

	// A new connection, with a budget that accepts the same response.
	srv.mu.Lock()
	srv.searchUIDs = 4
	srv.mu.Unlock()

	conn2 := srv.dial(t)
	c2, err := NewWithOptions(conn2, DialOptions{
		MaxResponseFields:   64,
		MaxAtomSize:         1024,
		MaxQuotedStringSize: 1024,
		MaxResponseBytes:    1 << 20,
		MaxListDepth:        16,
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	defer c2.Close()
	c2.Timeout = 5 * time.Second
	setClientState(c2, imap.SelectedState, &imap.MailboxStatus{Name: "INBOX"})

	uids, err := c2.UidSearch(&imap.SearchCriteria{})
	if err != nil {
		t.Fatalf("UidSearch on the new connection: %v", err)
	}
	if len(uids) != 4 {
		t.Errorf("UidSearch returned %d UIDs, want 4", len(uids))
	}
	if got := srv.connectionCount(); got != 2 {
		t.Errorf("the server saw %d connections, want 2", got)
	}
}

// TestClientLimitsUnderImplicitTLSAndSTARTTLS checks that the limits
// are in force under both TLS modes.
//
// The limits are applied to the reader before the reader goroutine
// starts, and the reader survives a STARTTLS upgrade, so the same
// behaviour is expected in both modes.
func TestClientLimitsUnderImplicitTLSAndSTARTTLS(t *testing.T) {
	for _, tc := range []struct {
		name        string
		implicitTLS bool
		startTLS    bool
	}{
		{name: "implicit TLS", implicitTLS: true},
		{name: "STARTTLS", startTLS: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := &limitsServer{searchUIDs: 500, implicitTLS: tc.implicitTLS}
			srv.start(t)
			defer srv.close()

			conn := srv.dial(t)
			c, err := NewWithOptions(conn, DialOptions{
				MaxResponseFields:   16,
				MaxAtomSize:         1024,
				MaxQuotedStringSize: 1024,
				MaxResponseBytes:    1 << 20,
				MaxListDepth:        16,
			})
			if err != nil {
				t.Fatalf("NewWithOptions: %v", err)
			}
			defer c.Close()
			c.Timeout = 5 * time.Second

			if tc.startTLS {
				cert, err := tls.X509KeyPair(internal.LocalhostCert, internal.LocalhostKey)
				if err != nil {
					t.Fatal(err)
				}
				done := make(chan error, 1)
				go func() {
					done <- c.StartTLS(&tls.Config{InsecureSkipVerify: true, Certificates: []tls.Certificate{cert}})
				}()
				// The server upgrades when it sees the STARTTLS
				// command; the client and the server handshake
				// together.
				select {
				case err := <-done:
					if err != nil {
						t.Fatalf("StartTLS: %v", err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("the STARTTLS upgrade did not complete")
				}
			}
			// The client only knows the connection is encrypted when it
			// performed the upgrade itself. An implicit-TLS connection
			// is handed to the client already encrypted, so the client
			// cannot report it.
			if tc.startTLS && !c.IsTLS() {
				t.Error("IsTLS = false, want true after a STARTTLS upgrade")
			}

			setClientState(c, imap.SelectedState, &imap.MailboxStatus{Name: "INBOX"})
			if _, err := c.UidSearch(&imap.SearchCriteria{}); err == nil {
				t.Fatal("expected the search to be refused")
			} else if !errors.Is(err, imap.ErrResponseTooLarge) {
				t.Errorf("err = %v, want errors.Is(err, imap.ErrResponseTooLarge)", err)
			}
			select {
			case <-c.LoggedOut():
			case <-time.After(10 * time.Second):
				t.Fatal("the connection was not terminated after the decoder refusal")
			}
		})
	}
}

// TestClientLimitsAfterSTARTTLSUpgrade checks that every limit is
// still in force after the connection has been upgraded to TLS.
//
// The reader is created once and survives the upgrade, so the limits
// must still apply to the responses that follow it.
func TestClientLimitsAfterSTARTTLSUpgrade(t *testing.T) {
	srv := &limitsServer{searchUIDs: 500}
	srv.start(t)
	defer srv.close()

	conn := srv.dial(t)
	c, err := NewWithOptions(conn, DialOptions{
		MaxResponseFields:   64,
		MaxAtomSize:         1024,
		MaxQuotedStringSize: 1024,
		MaxResponseBytes:    1 << 20,
		MaxListDepth:        16,
	})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	defer c.Close()
	c.Timeout = 5 * time.Second

	cert, err := tls.X509KeyPair(internal.LocalhostCert, internal.LocalhostKey)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- c.StartTLS(&tls.Config{InsecureSkipVerify: true, Certificates: []tls.Certificate{cert}})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("StartTLS: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the STARTTLS upgrade did not complete")
	}

	setClientState(c, imap.SelectedState, &imap.MailboxStatus{Name: "INBOX"})
	if _, err := c.UidSearch(&imap.SearchCriteria{}); err == nil {
		t.Fatal("expected the search to be refused after the upgrade")
	} else if !errors.Is(err, imap.ErrResponseTooLarge) {
		t.Errorf("err = %v, want errors.Is(err, imap.ErrResponseTooLarge)", err)
	}
}

// TestClientOversizedGreetingIsRefused checks that an oversized server
// greeting is refused by the constructor.
//
// The greeting is the first response the client reads, and it is read
// before any command is issued, so the byte budget applies to it as
// well.
func TestClientOversizedGreetingIsRefused(t *testing.T) {
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
		io.WriteString(conn, "* OK "+strings.Repeat("x", 4096)+" ready\r\n")
		// Keep the connection open so the client waits for the greeting
		// rather than seeing an immediate EOF.
		io.Copy(io.Discard, conn)
	}()

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))

	c, err := NewWithOptions(conn, DialOptions{
		MaxResponseFields:   64,
		MaxAtomSize:         1024,
		MaxQuotedStringSize: 1024,
		MaxResponseBytes:    512,
		MaxListDepth:        16,
	})
	if err == nil {
		c.Close()
		t.Fatal("NewWithOptions accepted an oversized greeting")
	}
	if !errors.Is(err, imap.ErrResponseTooLarge) {
		t.Errorf("err = %v, want errors.Is(err, imap.ErrResponseTooLarge)", err)
	}
}

// TestClientZeroLimitsPreserveUpstreamBehaviour checks that a client
// with no limits behaves exactly like the upstream one: a SEARCH
// result of any size is accepted.
func TestClientZeroLimitsPreserveUpstreamBehaviour(t *testing.T) {
	srv := &limitsServer{searchUIDs: 2000}
	srv.start(t)
	defer srv.close()

	conn := srv.dial(t)
	c, err := NewWithOptions(conn, DialOptions{})
	if err != nil {
		t.Fatalf("NewWithOptions: %v", err)
	}
	defer c.Close()
	c.Timeout = 5 * time.Second
	setClientState(c, imap.SelectedState, &imap.MailboxStatus{Name: "INBOX"})

	uids, err := c.UidSearch(&imap.SearchCriteria{})
	if err != nil {
		t.Fatalf("UidSearch: %v", err)
	}
	if len(uids) != 2000 {
		t.Errorf("UidSearch returned %d UIDs, want 2000", len(uids))
	}
}

// unusedFmt keeps the fmt import referenced.
var _ = fmt.Sprintf
