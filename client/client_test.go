package client

import (
	"bufio"
	"bytes"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"

	"github.com/flowbyte-com/go-imap"
)

type cmdScanner struct {
	scanner *bufio.Scanner
}

func (s *cmdScanner) ScanLine() string {
	s.scanner.Scan()
	return s.scanner.Text()
}

func (s *cmdScanner) ScanCmd() (tag string, cmd string) {
	parts := strings.SplitN(s.ScanLine(), " ", 2)
	return parts[0], parts[1]
}

func newCmdScanner(r io.Reader) *cmdScanner {
	return &cmdScanner{
		scanner: bufio.NewScanner(r),
	}
}

type serverConn struct {
	*cmdScanner
	net.Conn
	net.Listener
}

func (c *serverConn) Close() error {
	if err := c.Conn.Close(); err != nil {
		return err
	}
	return c.Listener.Close()
}

func (c *serverConn) WriteString(s string) (n int, err error) {
	return io.WriteString(c.Conn, s)
}

func newTestClient(t *testing.T) (c *Client, s *serverConn) {
	return newTestClientWithGreeting(t, "* OK [CAPABILITY IMAP4rev1 STARTTLS AUTH=PLAIN UNSELECT] Server ready.\r\n")
}

func newTestClientWithGreeting(t *testing.T, greeting string) (c *Client, s *serverConn) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() {
		conn, err := l.Accept()
		if err != nil {
			panic(err)
		}

		if _, err := io.WriteString(conn, greeting); err != nil {
			panic(err)
		}

		s = &serverConn{newCmdScanner(conn), conn, l}
		close(done)
	}()

	c, err = Dial(l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	<-done
	return
}

func setClientState(c *Client, state imap.ConnState, mailbox *imap.MailboxStatus) {
	c.locker.Lock()
	c.state = state
	c.mailbox = mailbox
	c.mailboxImmutable = mailbox != nil
	c.locker.Unlock()
}

func TestClient(t *testing.T) {
	c, s := newTestClient(t)
	defer s.Close()

	if ok, err := c.Support("IMAP4rev1"); err != nil {
		t.Fatal("c.Support(IMAP4rev1) =", err)
	} else if !ok {
		t.Fatal("c.Support(IMAP4rev1) = false, want true")
	}
}

func TestClient_SetDebug(t *testing.T) {
	c, s := newTestClient(t)
	defer s.Close()

	var b bytes.Buffer
	done := make(chan error)

	go func() {
		c.SetDebug(&b)
		done <- nil
	}()
	if tag, cmd := s.ScanCmd(); cmd != "NOOP" {
		t.Fatal("Bad command:", cmd)
	} else {
		s.WriteString(tag + " OK NOOP completed.\r\n")
	}
	// wait for SetDebug to finish.
	<-done

	go func() {
		_, err := c.Capability()
		done <- err
	}()

	tag, cmd := s.ScanCmd()
	if cmd != "CAPABILITY" {
		t.Fatal("Bad command:", cmd)
	}

	s.WriteString("* CAPABILITY IMAP4rev1\r\n")
	s.WriteString(tag + " OK CAPABILITY completed.\r\n")

	if err := <-done; err != nil {
		t.Fatal("c.Capability() =", err)
	}

	if b.Len() == 0 {
		t.Error("empty debug buffer")
	}
}

func TestClient_unilateral(t *testing.T) {
	c, s := newTestClient(t)
	defer s.Close()

	setClientState(c, imap.SelectedState, imap.NewMailboxStatus("INBOX", nil))

	updates := make(chan Update, 1)
	c.Updates = updates

	s.WriteString("* 42 EXISTS\r\n")
	if update, ok := (<-updates).(*MailboxUpdate); !ok || update.Mailbox.Messages != 42 {
		t.Errorf("Invalid messages count: expected %v but got %v", 42, update.Mailbox.Messages)
	}

	s.WriteString("* 587 RECENT\r\n")
	if update, ok := (<-updates).(*MailboxUpdate); !ok || update.Mailbox.Recent != 587 {
		t.Errorf("Invalid recent count: expected %v but got %v", 587, update.Mailbox.Recent)
	}

	s.WriteString("* 65535 EXPUNGE\r\n")
	if update, ok := (<-updates).(*ExpungeUpdate); !ok || update.SeqNum != 65535 {
		t.Errorf("Invalid expunged sequence number: expected %v but got %v", 65535, update.SeqNum)
	}

	s.WriteString("* 431 FETCH (FLAGS (\\Seen))\r\n")
	if update, ok := (<-updates).(*MessageUpdate); !ok || update.Message.SeqNum != 431 {
		t.Errorf("Invalid expunged sequence number: expected %v but got %v", 431, update.Message.SeqNum)
	}

	s.WriteString("* OK Reticulating splines...\r\n")
	if update, ok := (<-updates).(*StatusUpdate); !ok || update.Status.Info != "Reticulating splines..." {
		t.Errorf("Invalid info: got %v", update.Status.Info)
	}

	s.WriteString("* NO Kansai band competition is in 30 seconds !\r\n")
	if update, ok := (<-updates).(*StatusUpdate); !ok || update.Status.Info != "Kansai band competition is in 30 seconds !" {
		t.Errorf("Invalid warning: got %v", update.Status.Info)
	}

	s.WriteString("* BAD Battery level too low, shutting down.\r\n")
	if update, ok := (<-updates).(*StatusUpdate); !ok || update.Status.Info != "Battery level too low, shutting down." {
		t.Errorf("Invalid error: got %v", update.Status.Info)
	}
}

// TestClient_MaxLiteralSize is the fork regression for
// protocol-level IMAP literal-size enforcement. The test
// server announces a literal header `{N}\r\n` and then
// "sends" the body, but the client must reject the
// announcement before any of the body bytes are allocated
// or read from the conn. The fork's MaxLiteralSize option
// must cause ReadLiteral to return
// imap.ErrLiteralSizeExceeded without consuming the body
// bytes, the reader must close the conn on the parse error,
// the executor's loggedOut case must return a typed error
// that wraps the specific cause, and the client must be
// usable (or at least observable as closed) afterwards.
func TestClient_MaxLiteralSize(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	const cap = 64
	const announcedBodySize = 1024 // larger than cap; must be rejected before allocation

	greeting := "* OK [CAPABILITY IMAP4rev1] ready\r\n"
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		if _, err := io.WriteString(conn, greeting); err != nil {
			return
		}
		// Wait for the client to send a command (any
		// command; UidFetch writes a tagged line). When
		// we see it, respond with a FETCH response
		// whose BODY[] literal header is over the cap.
		// The literal body is never sent.
		br := bufio.NewReader(conn)
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				return
			}
			if !strings.HasPrefix(line, "*") {
				// Tagged command from the
				// client. Respond with a
				// FETCH whose body literal
				// exceeds the cap.
				resp := "* 1 FETCH (UID 1 BODY[] {" +
					strconv.Itoa(announcedBodySize) +
					"}\r\n"
				io.WriteString(conn, resp)
				// Do NOT write the body.
				// The client must close
				// the conn on the cap
				// violation before
				// reading any body bytes.
				// Wait for the client to
				// close the conn. The
				// client's close triggers
				// a read error here.
				buf := make([]byte, 1)
				_, _ = conn.Read(buf)
				return
			}
		}
	}()

	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}

	c, err := NewWithOptions(conn, DialOptions{MaxLiteralSize: MaxLiteralSize(cap)})
	if err != nil {
		t.Fatal(err)
	}
	// Move the client to the Selected state so FETCH is
	// accepted by the client-side state machine. The
	// server-side response never reaches us because the
	// cap fires during the read of the literal header.
	setClientState(c, imap.SelectedState, &imap.MailboxStatus{Name: "INBOX"})

	// Issue a FETCH that will trigger a literal read.
	// The in-memory server in the goroutine above has
	// already written the greeting and the literal
	// header. The client's read of the FETCH response
	// will hit the cap.
	seqSet := &imap.SeqSet{}
	seqSet.AddNum(1)
	ch := make(chan *imap.Message, 1)
	done2 := make(chan struct{})
	var fetchErr error
	go func() {
		defer close(done2)
		fetchErr = c.UidFetch(
			seqSet,
			[]imap.FetchItem{imap.FetchUid, imap.FetchBody, imap.FetchRFC822Size},
			ch,
		)
	}()
	<-done2

	// The Fetch call must return a non-nil error that
	// wraps imap.ErrLiteralSizeExceeded. The error path
	// is: reader hits the cap, stores the error, closes
	// the conn, exits. The executor's loggedOut case
	// wraps the stored error in a connectionClosedError.
	if fetchErr == nil {
		t.Fatal("Fetch returned nil error; want a literal-size violation")
	}
	if !errors.Is(fetchErr, imap.ErrLiteralSizeExceeded) {
		t.Errorf("Fetch err = %v; want errors.Is ErrLiteralSizeExceeded", fetchErr)
	}

	// Wait for the server goroutine to finish. It exits when
	// the client closes the conn, which proves the client
	// tore the connection down rather than leaving it in a
	// desynchronised state.
	<-done
}
