// Package client provides an IMAP client.
//
// It is not safe to use the same Client from multiple goroutines. In general,
// the IMAP protocol doesn't make it possible to send multiple independent
// IMAP commands on the same connection.
package client

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"github.com/flowbyte-com/go-imap"
	"github.com/flowbyte-com/go-imap/commands"
	"github.com/flowbyte-com/go-imap/responses"
)

// errClosed is used when a connection is closed while waiting for a command
// response. In TheMailroom's fork, errClosed wraps the
// underlying cause (typically a parse error such as
// imap.ErrLiteralSizeExceeded) so callers can errors.Is to
// distinguish a literal-size violation from a generic
// transport failure. The underlying error is set by the
// reader goroutine before closing the conn; see (*Client).readOnce.
var errClosed = &connectionClosedError{cause: io.EOF}

// connectionClosedError is the typed error returned by the
// executor's loggedOut case. It carries the cause set by
// the reader before it closed the conn. The original
// upstream implementation was a plain fmt.Errorf; the
// TheMailroom fork replaces it with this typed error so the
// specific cause (literal-size violation, network error,
// etc.) is preserved through the client surface.
//
// errors.Is / errors.As on this error follow the standard
// Unwrap semantics.
type connectionClosedError struct {
	cause error
}

func (e *connectionClosedError) Error() string {
	if e.cause == nil {
		return "imap: connection closed"
	}
	return "imap: connection closed: " + e.cause.Error()
}

func (e *connectionClosedError) Unwrap() error { return e.cause }

// newConnectionClosedError returns a connectionClosedError
// wrapping the given cause. If cause is nil, returns the
// default errClosed.
func newConnectionClosedError(cause error) error {
	if cause == nil {
		return errClosed
	}
	// io.EOF is the natural "connection closed" signal; do
	// not wrap it specifically so callers can use
	// errors.Is(err, io.EOF) if they want.
	if errors.Is(cause, io.EOF) {
		return &connectionClosedError{cause: io.EOF}
	}
	return &connectionClosedError{cause: cause}
}

// errUnregisterHandler is returned by a response handler to unregister itself.
var errUnregisterHandler = fmt.Errorf("imap: unregister handler")

// Update is an unilateral server update.
type Update interface {
	update()
}

// StatusUpdate is delivered when a status update is received.
type StatusUpdate struct {
	Status *imap.StatusResp
}

func (u *StatusUpdate) update() {}

// MailboxUpdate is delivered when a mailbox status changes.
//
// Mailbox is the snapshot that became current when the update was handled.
// Like the value returned by Client.Mailbox, it is immutable: later unilateral
// updates install a new snapshot instead of modifying this one in place.
type MailboxUpdate struct {
	Mailbox *imap.MailboxStatus
}

func (u *MailboxUpdate) update() {}

// ExpungeUpdate is delivered when a message is deleted.
type ExpungeUpdate struct {
	SeqNum uint32
}

func (u *ExpungeUpdate) update() {}

// MessageUpdate is delivered when a message attribute changes.
type MessageUpdate struct {
	Message *imap.Message
}

func (u *MessageUpdate) update() {}

// Client is an IMAP client.
type Client struct {
	conn       *imap.Conn
	isTLS      bool
	serverName string

	loggedOut chan struct{}
	continues chan<- bool
	upgrading bool

	handlers       []responses.Handler
	handlersLocker sync.Mutex

	// The current connection state.
	state imap.ConnState
	// The selected mailbox, if there is one.
	mailbox *imap.MailboxStatus
	// mailboxImmutable is true when mailbox has been published to the caller
	// (returned by Select or supplied by SetState). Published statuses are
	// never mutated in place: a unilateral update installs a fresh snapshot.
	// While Select is still building its status, updates may be applied to its
	// private working copy; MailboxUpdate values are independent snapshots.
	mailboxImmutable bool
	// The cached server capabilities.
	caps map[string]bool
	// state, mailbox, mailboxImmutable and caps may be accessed in different
	// goroutines. Protect access.
	locker sync.Mutex

	// A channel to which unilateral updates from the server will be sent. An
	// update can be one of: *StatusUpdate, *MailboxUpdate, *MessageUpdate,
	// *ExpungeUpdate. Note that blocking this channel blocks the whole client,
	// so it's recommended to use a separate goroutine and a buffered channel to
	// prevent deadlocks.
	Updates chan<- Update

	// ErrorLog specifies an optional logger for errors accepting connections and
	// unexpected behavior from handlers. By default, logging goes to os.Stderr
	// via the log package's standard logger. The logger must be safe to use
	// simultaneously from multiple goroutines.
	ErrorLog imap.Logger

	// Timeout specifies a maximum amount of time to wait on a command.
	//
	// A Timeout of zero means no timeout. This is the default.
	Timeout time.Duration

	// lastReadErr is the most recent error observed by the
	// reader goroutine before it closed the conn. The
	// executor's loggedOut case returns this error wrapped
	// in errClosed, so callers can errors.Is to distinguish
	// a literal-size violation from a generic transport
	// failure. Set under readersMu; read under callersMu.
	readersMu   sync.Mutex
	lastReadErr error
}

// MaxLiteralSize is the per-call constructor option that
// sets the protocol-level IMAP literal-size cap. It is added
// by TheMailroom's fork (github.com/flowbyte-com/go-imap) to
// satisfy the pre-production security requirement in
// github.com/flowbyte-com/themailroom/docs/PHASE2_DESIGN.md
// §10. A value of zero (the default) means "no cap".
//
// When set, the imap.Reader rejects literals whose announced
// size exceeds this value BEFORE allocating a buffer. The
// fork's reader-side changes ensure the error is propagated
// to the executor with enough information for the caller to
// distinguish a literal-size violation from other failures.
//
// The cap is applied to the reader at construction time, before
// the server greeting is parsed and before the background
// reader goroutine starts. See NewWithOptions.
type MaxLiteralSize uint32

// DialOptions carries the per-connection limits that are applied to
// the IMAP reader at construction time.
//
// Every limit is zero by default, which means "no limit". See
// NewWithOptions for the ordering guarantees.
type DialOptions struct {
	MaxLiteralSize MaxLiteralSize

	// MaxResponseFields is the maximum number of fields one response
	// may contain, counted across every nesting level. Zero means no
	// limit.
	MaxResponseFields uint32

	// MaxAtomSize is the maximum length in bytes of a single atom.
	// Zero means no limit.
	MaxAtomSize uint32

	// MaxQuotedStringSize is the maximum length in bytes of a single
	// quoted string. Zero means no limit.
	MaxQuotedStringSize uint32

	// MaxResponseBytes is the maximum number of bytes one response may
	// consume, including the tag, the status or continuation text, the
	// literal length declarations, the literal bodies, the atoms, the
	// quoted strings and every delimiter between them. Zero means no
	// limit.
	MaxResponseBytes uint32

	// MaxListDepth is the maximum nesting depth of the lists in one
	// response. Zero means no limit.
	MaxListDepth uint32
}

func (c *Client) registerHandler(h responses.Handler) {
	if h == nil {
		return
	}

	c.handlersLocker.Lock()
	c.handlers = append(c.handlers, h)
	c.handlersLocker.Unlock()
}

func (c *Client) handle(resp imap.Resp) error {
	c.handlersLocker.Lock()
	for i := len(c.handlers) - 1; i >= 0; i-- {
		if err := c.handlers[i].Handle(resp); err != responses.ErrUnhandled {
			if err == errUnregisterHandler {
				c.handlers = append(c.handlers[:i], c.handlers[i+1:]...)
				err = nil
			}
			c.handlersLocker.Unlock()
			return err
		}
	}
	c.handlersLocker.Unlock()
	return responses.ErrUnhandled
}

func (c *Client) reader() {
	defer close(c.loggedOut)
	// Loop while connected.
	for {
		connected, err := c.readOnce()
		if err != nil {
			c.ErrorLog.Println("error reading response:", err)
		}
		if !connected {
			return
		}
	}
}

func (c *Client) readOnce() (bool, error) {
	if c.State() == imap.LogoutState {
		return false, nil
	}

	resp, err := imap.ReadResp(c.conn.Reader)
	if err == io.EOF || c.State() == imap.LogoutState {
		return false, nil
	} else if err != nil {
		if imap.IsParseError(err) {
			// TheMailroom fork: a parse error
			// desynchronises the response stream; the
			// conn is no longer usable. Store the
			// specific error so the executor's
			// loggedOut case can surface it, then
			// close the conn and exit the reader.
			//
			// The error is returned as well as
			// connected=false, so the greeting path
			// (which calls readOnce directly) reports
			// the specific cause instead of a bare
			// io.EOF.
			c.readersMu.Lock()
			c.lastReadErr = err
			c.readersMu.Unlock()
			c.conn.Close()
			return false, err
		}
		return false, err
	}

	if err := c.handle(resp); err != nil && err != responses.ErrUnhandled {
		c.ErrorLog.Println("cannot handle response ", resp, err)
	}
	return true, nil
}

func (c *Client) writeReply(reply []byte) error {
	if _, err := c.conn.Writer.Write(reply); err != nil {
		return err
	}
	// Flush reply
	return c.conn.Writer.Flush()
}

type handleResult struct {
	status *imap.StatusResp
	err    error
}

// takeQueuedResult returns a command result the reader has already
// delivered to doneHandle.
//
// This is used when the executor observes loggedOut. The reader sends
// a tagged completion to doneHandle before it can handle a following
// * BYE and close loggedOut. A channel send happens-before the receive
// that observes the close, so once loggedOut is closed a queued result
// is visible here. A non-blocking check made only before the main
// select would not be sufficient: the result can be queued between
// that check and the select choosing loggedOut.
func takeQueuedResult(doneHandle <-chan handleResult) (handleResult, bool) {
	select {
	case result := <-doneHandle:
		return result, true
	default:
		return handleResult{}, false
	}
}

func (c *Client) execute(cmdr imap.Commander, h responses.Handler) (*imap.StatusResp, error) {
	cmd := cmdr.Command()
	cmd.Tag = generateTag()

	var replies <-chan []byte
	if replier, ok := h.(responses.Replier); ok {
		replies = replier.Replies()
	}

	if c.Timeout > 0 {
		err := c.conn.SetDeadline(time.Now().Add(c.Timeout))
		if err != nil {
			return nil, err
		}
	} else {
		// It's possible the client had a timeout set from a previous command, but no
		// longer does. Ensure we respect that. The zero time means no deadline.
		if err := c.conn.SetDeadline(time.Time{}); err != nil {
			return nil, err
		}
	}

	// Check if we are upgrading.
	upgrading := c.upgrading

	// Add handler before sending command, to be sure to get the response in time
	// (in tests, the response is sent right after our command is received, so
	// sometimes the response was received before the setup of this handler)
	doneHandle := make(chan handleResult, 1)
	unregister := make(chan struct{})
	c.registerHandler(responses.HandlerFunc(func(resp imap.Resp) error {
		select {
		case <-unregister:
			// If an error occured while sending the command, abort
			return errUnregisterHandler
		default:
		}

		if s, ok := resp.(*imap.StatusResp); ok && s.Tag == cmd.Tag {
			// This is the command's status response, we're done
			doneHandle <- handleResult{s, nil}
			// Special handling of connection upgrading.
			if upgrading {
				c.upgrading = false
				// Wait for upgrade to finish.
				c.conn.Wait()
			}
			// Cancel any pending literal write
			select {
			case c.continues <- false:
			default:
			}
			return errUnregisterHandler
		}

		if h != nil {
			// Pass the response to the response handler
			if err := h.Handle(resp); err != nil && err != responses.ErrUnhandled {
				// If the response handler returns an error, abort
				doneHandle <- handleResult{nil, err}
				return errUnregisterHandler
			} else {
				return err
			}
		}
		return responses.ErrUnhandled
	}))

	// Send the command to the server
	if err := cmd.WriteTo(c.conn.Writer); err != nil {
		// Error while sending the command
		close(unregister)

		if err, ok := err.(imap.LiteralLengthErr); ok {
			// Expected > Actual
			//  The server is waiting for us to write
			//  more bytes, we don't have them. Run.
			// Expected < Actual
			//  We are about to send a potentially truncated message, we don't
			//  want this (ths terminating CRLF is not sent at this point).
			c.conn.Close()
			return nil, err
		}

		return nil, err
	}
	// Flush writer if we are upgrading
	if upgrading {
		if err := c.conn.Writer.Flush(); err != nil {
			// Error while sending the command
			close(unregister)
			return nil, err
		}
	}

	for {
		select {
		case reply := <-replies:
			// Response handler needs to send a reply (Used for AUTHENTICATE)
			if err := c.writeReply(reply); err != nil {
				close(unregister)
				return nil, err
			}
		case <-c.loggedOut:
			close(unregister)

			// The reader may have queued the command's tagged
			// completion before it processed a subsequent * BYE
			// and closed loggedOut. If both cases are ready, the
			// select above may have chosen this one. The result
			// wins: a completed command is not a disconnect.
			//
			// If no result is queued, this is a genuine
			// disconnect (I/O error, parse/decoder failure or
			// BYE without a tagged completion) and the error
			// path below is preserved.
			if result, ok := takeQueuedResult(doneHandle); ok {
				return result.status, result.err
			}

			// If the connection is closed (such as from an I/O error), ensure we
			// realize this and don't block waiting on a response that will never
			// come. loggedOut is a channel that closes when the reader goroutine
			// ends. TheMailroom fork: surface the
			// specific cause (e.g. a literal-size
			// violation) via the typed errClosed so
			// callers can errors.Is it.
			c.readersMu.Lock()
			cause := c.lastReadErr
			c.readersMu.Unlock()
			if cause != nil {
				return nil, newConnectionClosedError(cause)
			}
			return nil, errClosed
		case result := <-doneHandle:
			return result.status, result.err
		}
	}
}

// State returns the current connection state.
func (c *Client) State() imap.ConnState {
	c.locker.Lock()
	state := c.state
	c.locker.Unlock()
	return state
}

// Mailbox returns the latest selected-mailbox status snapshot. It returns nil
// if there isn't one.
//
// The returned status is immutable: the client never modifies it after
// returning it. A unilateral mailbox update installs a new snapshot, which can
// be obtained from a later Mailbox call or received as a *MailboxUpdate. A
// pointer obtained earlier keeps the values it had when it was returned.
// Callers that need the current values should call Mailbox again or consume
// Updates, rather than retaining the status returned by Select.
func (c *Client) Mailbox() *imap.MailboxStatus {
	c.locker.Lock()
	mbox := c.mailbox
	c.locker.Unlock()
	return mbox
}

// SetState sets this connection's internal state.
//
// This function should not be called directly, it must only be used by
// libraries implementing extensions of the IMAP protocol.
//
// The mailbox is treated as an immutable snapshot: the client will not modify
// it in place when unilateral updates arrive. Unilateral updates replace the
// client's current snapshot and leave the caller's pointer unchanged.
func (c *Client) SetState(state imap.ConnState, mailbox *imap.MailboxStatus) {
	c.locker.Lock()
	c.state = state
	c.mailbox = mailbox
	c.mailboxImmutable = mailbox != nil
	c.locker.Unlock()
}

// Execute executes a generic command. cmdr is a value that can be converted to
// a raw command and h is a response handler. The function returns when the
// command has completed or failed, in this case err is nil. A non-nil err value
// indicates a network error.
//
// This function should not be called directly, it must only be used by
// libraries implementing extensions of the IMAP protocol.
func (c *Client) Execute(cmdr imap.Commander, h responses.Handler) (*imap.StatusResp, error) {
	return c.execute(cmdr, h)
}

func (c *Client) handleContinuationReqs() {
	c.registerHandler(responses.HandlerFunc(func(resp imap.Resp) error {
		if _, ok := resp.(*imap.ContinuationReq); ok {
			go func() {
				c.continues <- true
			}()
			return nil
		}
		return responses.ErrUnhandled
	}))
}

func (c *Client) gotStatusCaps(args []interface{}) {
	c.locker.Lock()

	c.caps = make(map[string]bool)
	for _, cap := range args {
		if cap, ok := cap.(string); ok {
			c.caps[cap] = true
		}
	}

	c.locker.Unlock()
}

// The server can send unilateral data. This function handles it.
func (c *Client) handleUnilateral() {
	c.registerHandler(responses.HandlerFunc(func(resp imap.Resp) error {
		switch resp := resp.(type) {
		case *imap.StatusResp:
			if resp.Tag != "*" {
				return responses.ErrUnhandled
			}

			switch resp.Type {
			case imap.StatusRespOk, imap.StatusRespNo, imap.StatusRespBad:
				if c.Updates != nil {
					c.Updates <- &StatusUpdate{resp}
				}
			case imap.StatusRespBye:
				c.locker.Lock()
				c.state = imap.LogoutState
				c.mailbox = nil
				c.mailboxImmutable = false
				c.locker.Unlock()

				c.conn.Close()

				if c.Updates != nil {
					c.Updates <- &StatusUpdate{resp}
				}
			default:
				return responses.ErrUnhandled
			}
		case *imap.DataResp:
			name, fields, ok := imap.ParseNamedResp(resp)
			if !ok {
				return responses.ErrUnhandled
			}

			switch name {
			case "CAPABILITY":
				c.gotStatusCaps(fields)
			case "EXISTS":
				messages, err := imap.ParseNumber(fields[0])
				if err != nil {
					break
				}

				c.locker.Lock()
				mbox := c.mailbox
				var updateStatus *imap.MailboxStatus
				if mbox != nil {
					if c.mailboxImmutable {
						mbox = cloneMailboxStatus(mbox)
						c.mailbox = mbox
						updateStatus = mbox
					}
					mbox.Messages = messages
					mbox.ItemsLocker.Lock()
					if mbox.Items == nil {
						mbox.Items = make(map[imap.StatusItem]interface{})
					}
					mbox.Items[imap.StatusMessages] = nil
					mbox.ItemsLocker.Unlock()
					if updateStatus == nil {
						// Select is still building the status. Keep updating
						// its working copy, but deliver an independent
						// snapshot so the consumer cannot observe a later
						// in-place change.
						updateStatus = cloneMailboxStatus(mbox)
					}
				}
				c.locker.Unlock()

				if updateStatus != nil && c.Updates != nil {
					c.Updates <- &MailboxUpdate{updateStatus}
				}
			case "RECENT":
				recent, err := imap.ParseNumber(fields[0])
				if err != nil {
					break
				}

				c.locker.Lock()
				mbox := c.mailbox
				var updateStatus *imap.MailboxStatus
				if mbox != nil {
					if c.mailboxImmutable {
						mbox = cloneMailboxStatus(mbox)
						c.mailbox = mbox
						updateStatus = mbox
					}
					mbox.Recent = recent
					mbox.ItemsLocker.Lock()
					if mbox.Items == nil {
						mbox.Items = make(map[imap.StatusItem]interface{})
					}
					mbox.Items[imap.StatusRecent] = nil
					mbox.ItemsLocker.Unlock()
					if updateStatus == nil {
						updateStatus = cloneMailboxStatus(mbox)
					}
				}
				c.locker.Unlock()

				if updateStatus != nil && c.Updates != nil {
					c.Updates <- &MailboxUpdate{updateStatus}
				}
			case "EXPUNGE":
				seqNum, _ := imap.ParseNumber(fields[0])

				if c.Updates != nil {
					c.Updates <- &ExpungeUpdate{seqNum}
				}
			case "FETCH":
				seqNum, _ := imap.ParseNumber(fields[0])
				fields, _ := fields[1].([]interface{})

				msg := &imap.Message{SeqNum: seqNum}
				if err := msg.Parse(fields); err != nil {
					break
				}

				if c.Updates != nil {
					c.Updates <- &MessageUpdate{msg}
				}
			default:
				return responses.ErrUnhandled
			}
		default:
			return responses.ErrUnhandled
		}
		return nil
	}))
}

func (c *Client) handleGreetAndStartReading() error {
	var greetErr error
	gotGreet := false

	c.registerHandler(responses.HandlerFunc(func(resp imap.Resp) error {
		status, ok := resp.(*imap.StatusResp)
		if !ok {
			greetErr = fmt.Errorf("invalid greeting received from server: not a status response")
			return errUnregisterHandler
		}

		c.locker.Lock()
		switch status.Type {
		case imap.StatusRespPreauth:
			c.state = imap.AuthenticatedState
		case imap.StatusRespBye:
			c.state = imap.LogoutState
		case imap.StatusRespOk:
			c.state = imap.NotAuthenticatedState
		default:
			c.state = imap.LogoutState
			c.locker.Unlock()
			greetErr = fmt.Errorf("invalid greeting received from server: %v", status.Type)
			return errUnregisterHandler
		}
		c.locker.Unlock()

		if status.Code == imap.CodeCapability {
			c.gotStatusCaps(status.Arguments)
		}

		gotGreet = true
		return errUnregisterHandler
	}))

	// call `readOnce` until we get the greeting or an error
	for !gotGreet {
		connected, err := c.readOnce()
		// Check for read errors
		if err != nil {
			// return read errors
			return err
		}
		// Check for invalid greet
		if greetErr != nil {
			// return read errors
			return greetErr
		}
		// Check if connection was closed.
		if !connected {
			// connection closed.
			return io.EOF
		}
	}

	// We got the greeting, now start the reader goroutine.
	go c.reader()

	return nil
}

// Upgrade a connection, e.g. wrap an unencrypted connection with an encrypted
// tunnel.
//
// This function should not be called directly, it must only be used by
// libraries implementing extensions of the IMAP protocol.
func (c *Client) Upgrade(upgrader imap.ConnUpgrader) error {
	return c.conn.Upgrade(upgrader)
}

// Writer returns the imap.Writer for this client's connection.
//
// This function should not be called directly, it must only be used by
// libraries implementing extensions of the IMAP protocol.
func (c *Client) Writer() *imap.Writer {
	return c.conn.Writer
}

// IsTLS checks if this client's connection has TLS enabled.
func (c *Client) IsTLS() bool {
	return c.isTLS
}

// LoggedOut returns a channel which is closed when the connection to the server
// is closed.
func (c *Client) LoggedOut() <-chan struct{} {
	return c.loggedOut
}

// SetDebug defines an io.Writer to which all network activity will be logged.
// If nil is provided, network activity will not be logged.
func (c *Client) SetDebug(w io.Writer) {
	// Need to send a command to unblock the reader goroutine.
	cmd := new(commands.Noop)
	err := c.Upgrade(func(conn net.Conn) (net.Conn, error) {
		// Flag connection as in upgrading
		c.upgrading = true
		if status, err := c.execute(cmd, nil); err != nil {
			return nil, err
		} else if err := status.Err(); err != nil {
			return nil, err
		}

		// Wait for reader to block.
		c.conn.WaitReady()

		c.conn.SetDebug(w)
		return conn, nil
	})
	if err != nil {
		log.Println("SetDebug:", err)
	}

}

// New creates a new client from an existing connection.
//
// New is the upstream constructor. It is equivalent to
// NewWithOptions(conn, DialOptions{}), i.e. no literal-size cap.
func New(conn net.Conn) (*Client, error) {
	return NewWithOptions(conn, DialOptions{})
}

// NewWithOptions creates a new client from an existing
// connection, applying the given options.
//
// A zero value for every option means "no cap" and is equivalent to
// New.
//
// The caps are applied to the IMAP reader *at construction time*,
// before the reader is used for anything. In particular they are set
// before:
//
//   - the server greeting is parsed, and
//   - the background reader goroutine is started.
//
// This ordering matters. The reader goroutine reads
// Reader.MaxLiteralSize in ReadLiteral; if the cap were assigned
// after the goroutine started, the assignment would be a data
// race with that read, and the goroutine would have a window in
// which an over-cap literal was accepted with no cap at all
// (i.e. an unbounded allocation).
//
// This constructor is added by TheMailroom's fork
// (github.com/flowbyte-com/go-imap) to expose the protocol-level
// IMAP literal-size cap without requiring callers to reach into
// unexported fields. See the fork's CHANGES.md.
func NewWithOptions(conn net.Conn, opts DialOptions) (*Client, error) {
	continues := make(chan bool)
	w := imap.NewClientWriter(nil, continues)
	r := imap.NewReader(nil)

	// Apply the caps to the reader before it is installed in the
	// Conn, so that they are in effect for the very first byte
	// read from the connection. The fields are only ever written
	// here, before the reader goroutine exists, so there is no
	// concurrent access.
	if opts.MaxLiteralSize > 0 {
		r.MaxLiteralSize = uint32(opts.MaxLiteralSize)
	}
	r.MaxResponseFields = opts.MaxResponseFields
	r.MaxAtomSize = opts.MaxAtomSize
	r.MaxQuotedStringSize = opts.MaxQuotedStringSize
	r.MaxResponseBytes = opts.MaxResponseBytes
	r.MaxListDepth = opts.MaxListDepth

	c := &Client{
		conn:      imap.NewConn(conn, r, w),
		loggedOut: make(chan struct{}),
		continues: continues,
		state:     imap.ConnectingState,
		ErrorLog:  log.New(os.Stderr, "imap/client: ", log.LstdFlags),
	}

	c.handleContinuationReqs()
	c.handleUnilateral()
	if err := c.handleGreetAndStartReading(); err != nil {
		return c, err
	}

	plusOk, _ := c.Support("LITERAL+")
	minusOk, _ := c.Support("LITERAL-")
	// We don't use non-sync literal if it is bigger than 4096 bytes, so
	// LITERAL- is fine too.
	c.conn.AllowAsyncLiterals = plusOk || minusOk

	return c, nil
}

// Dial connects to an IMAP server using an unencrypted connection.
func Dial(addr string) (*Client, error) {
	return DialWithDialer(new(net.Dialer), addr)
}

type Dialer interface {
	// Dial connects to the given address.
	Dial(network, addr string) (net.Conn, error)
}

// DialWithDialer connects to an IMAP server using an unencrypted connection
// using dialer.Dial.
//
// Among other uses, this allows to apply a dial timeout.
func DialWithDialer(dialer Dialer, addr string) (*Client, error) {
	conn, err := dialer.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}

	// We don't return to the caller until we try to receive a greeting. As such,
	// there is no way to set the client's Timeout for that action. As a
	// workaround, if the dialer has a timeout set, use that for the connection's
	// deadline.
	if netDialer, ok := dialer.(*net.Dialer); ok && netDialer.Timeout > 0 {
		err := conn.SetDeadline(time.Now().Add(netDialer.Timeout))
		if err != nil {
			return nil, err
		}
	}

	c, err := New(conn)
	if err != nil {
		return nil, err
	}

	c.serverName, _, _ = net.SplitHostPort(addr)
	return c, nil
}

// DialTLS connects to an IMAP server using an encrypted connection.
func DialTLS(addr string, tlsConfig *tls.Config) (*Client, error) {
	return DialWithDialerTLS(new(net.Dialer), addr, tlsConfig)
}

// DialWithDialerTLS connects to an IMAP server using an encrypted connection
// using dialer.Dial.
//
// Among other uses, this allows to apply a dial timeout.
func DialWithDialerTLS(dialer Dialer, addr string, tlsConfig *tls.Config) (*Client, error) {
	conn, err := dialer.Dial("tcp", addr)
	if err != nil {
		return nil, err
	}

	serverName, _, _ := net.SplitHostPort(addr)
	if tlsConfig == nil {
		tlsConfig = &tls.Config{}
	}
	if tlsConfig.ServerName == "" {
		tlsConfig = tlsConfig.Clone()
		tlsConfig.ServerName = serverName
	}
	tlsConn := tls.Client(conn, tlsConfig)

	// We don't return to the caller until we try to receive a greeting. As such,
	// there is no way to set the client's Timeout for that action. As a
	// workaround, if the dialer has a timeout set, use that for the connection's
	// deadline.
	if netDialer, ok := dialer.(*net.Dialer); ok && netDialer.Timeout > 0 {
		err := tlsConn.SetDeadline(time.Now().Add(netDialer.Timeout))
		if err != nil {
			return nil, err
		}
	}

	c, err := New(tlsConn)
	if err != nil {
		return nil, err
	}

	c.isTLS = true
	c.serverName = serverName
	return c, nil
}
