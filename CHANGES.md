# Changes from upstream v1.2.1

This fork adds **protocol-level IMAP literal-size enforcement** to
`github.com/emersion/go-imap` v1.2.1.

It is used by TheMailroom to satisfy the mandatory pre-production
security requirement in
`github.com/flowbyte-com/themailroom/docs/PHASE2_DESIGN.md` §10.

## Files changed

Only two upstream source files are modified. Everything else is
byte-identical to upstream v1.2.1 apart from the module import
path rewrite (`github.com/emersion/go-imap` →
`github.com/flowbyte-com/go-imap`), which is mechanical.

### `read.go`

1. **New exported error `ErrLiteralSizeExceeded`.**

   ```go
   var ErrLiteralSizeExceeded = errors.New("imap: literal exceeding maximum size")
   ```

2. **New unexported type `literalSizeExceededError`.** It is a
   *value* type (not a pointer) that:
   - implements `Error()` returning the sentinel's message;
   - implements `Is(target error) bool` so
     `errors.Is(err, imap.ErrLiteralSizeExceeded)` is true;
   - is detected by the updated `IsParseError` so the existing
     executor logic that distinguishes parse failures from
     transport failures keeps working.

3. **`ReadLiteral` returns the new error instead of a generic
   parse error** when the announced literal size exceeds
   `Reader.MaxLiteralSize`:

   ```go
   if r.MaxLiteralSize > 0 && uint32(n) > r.MaxLiteralSize {
       return nil, literalSizeExceededError{}
   }
   ```

   **The allocation that follows is unchanged and is still
   skipped**: `b := make([]byte, n)` is only reached when the
   size check passes. A 1 GiB announcement is therefore rejected
   before any 1 GiB buffer is allocated.

4. **`IsParseError` extended** to also match
   `literalSizeExceededError` (in addition to the original
   `*parseError`).

### `client/client.go`

1. **New exported `MaxLiteralSize uint32` type and
   `DialOptions` struct**, plus a new constructor
   `NewWithOptions(conn net.Conn, opts DialOptions) (*Client, error)`.
   `NewWithOptions` sets `Reader.MaxLiteralSize` on the embedded
   reader. `New` is unchanged and is equivalent to
   `NewWithOptions(conn, DialOptions{})`.

2. **New `Client` fields** `readersMu sync.Mutex` and
   `lastReadErr error`. `lastReadErr` records the most recent
   read error observed by the reader goroutine.

3. **`readOnce` now closes the connection on a parse error.**

   Upstream:

   ```go
   if imap.IsParseError(err) {
       return true, err   // keep the (desynchronised) conn alive
   }
   ```

   Fork:

   ```go
   if imap.IsParseError(err) {
       c.readersMu.Lock()
       c.lastReadErr = err
       c.readersMu.Unlock()
       c.conn.Close()
       return false, err
   }
   ```

   The error is returned as well as `connected=false`. The
   greeting path calls `readOnce` directly and checks
   `err != nil`; returning `(false, nil)` there made it report
   a bare `io.EOF` and lose the specific cause. The reader
   goroutine already exits on `connected=false`, so its
   behaviour is unchanged.

   **Why this is required.** A parse error leaves the response
   stream desynchronised: the bytes of the rejected literal are
   still in flight, so every subsequent read produces further
   parse errors. Upstream keeps the connection alive, which means
   the executor can block indefinitely waiting for a tagged
   response that will never arrive. Closing the conn makes the
   reader goroutine exit, which closes `Client.loggedOut`, which
   unblocks the executor with a definite error.

4. **`errClosed` is now a typed `*connectionClosedError`** that
   wraps the underlying cause and exposes it via `Unwrap`. The
   executor's `<-c.loggedOut` case returns
   `newConnectionClosedError(c.lastReadErr)` when a read error was
   recorded, so the caller can `errors.Is(err,
   imap.ErrLiteralSizeExceeded)` to distinguish a literal-size
   violation from a generic transport failure.

   Upstream: `var errClosed = fmt.Errorf("imap: connection closed")`
   Fork: `var errClosed = &connectionClosedError{cause: io.EOF}`

   The `Error()` string of the generic case is unchanged
   (`"imap: connection closed"`), so existing string-matching
   callers keep working.

## Why these changes are sufficient

* **Pre-allocation.** The size check in `ReadLiteral` runs
  before `make([]byte, n)`. No buffer proportional to the
  announced size is ever allocated for a rejected literal.
* **Reliable propagation.** `readOnce` closes the conn on a
  parse error, so the reader goroutine exits and the executor
  is unblocked with a definite error instead of hanging.
* **Distinguishable cause.** The specific error is stored on the
  `Client` and re-wrapped by the executor, so the caller can
  distinguish a literal-size violation from a network failure,
  an authentication failure, or an unrelated parse error.
* **No silent success.** A rejected literal can never produce a
  successful response with a missing body: the response is never
  parsed into a `*imap.Message`, so the response handler never
  sees it, and the executor returns an error.

## Tests added

* `read_test.go`: `TestReader_MaxLiteralSize_Boundary` — the
  boundary at cap-1, cap, cap+1; asserts the error is both a
  parse error and `errors.Is(ErrLiteralSizeExceeded)`.
* `client/client_test.go`: `TestClient_MaxLiteralSize` — an
  end-to-end client test where the server announces an
  over-cap literal and never sends the body. Asserts the
  `UidFetch` call returns an error that
  `errors.Is(..., imap.ErrLiteralSizeExceeded)`, and that the
  server observes the client closing the conn (proving the
  connection was torn down and the body was never read).
* `client/cmd_noauth_test.go`:
  `TestClient_MaxLiteralSize_SurvivesStartTLS` — the cap
  lives on the `*imap.Reader`, which is created once at
  construction. The STARTTLS upgrade replaces the net.Conn but
  must not replace the Reader, or the cap would silently stop
  applying to the post-upgrade (decrypted) stream.
* `client/constructor_test.go` (constructor-ordering
  regressions):
  - `TestNewWithOptions_CapAppliesBeforeReaderStarts` — the
    cap must be present on the reader before the constructor
    reads the server greeting. The server sends the greeting
    and an unsolicited over-cap literal in a single write and
    never sends the body; the client must reject the literal,
    terminate the connection, and never report a missing body
    as a success.
  - `TestNewWithOptions_UnsolcitedOversizedLiteral` — the
    behavioural counterpart of the above, using only the public
    constructor.
  - `TestNewWithOptions_NoRaceUnderLoad` — a race-detector
    regression: the reader goroutine parses a stream of
    literals while the constructor finishes. Against the
    pre-fix constructor shape this reports a data race
    between `Reader.ReadLiteral` (read.go) and
    `NewWithOptions` (client.go).
  - `TestNew_PreservesUpstreamBehaviour` — `New(conn)` keeps
    its upstream signature and behaviour and is equivalent to
    `NewWithOptions(conn, DialOptions{})`.
  - `TestNewWithOptions_ZeroCapIsUnlimited` — a zero cap
    means "no cap".
  - `TestNewWithOptions_CapBoundary` — cap-1 / cap / cap+1
    through the option constructor.

  All of these fail (with a data race report, or by hanging
  on a body the server never sends) against the pre-fix
  constructor shape.

All upstream tests are preserved and pass unchanged.

## Route back to upstream

The changes are small and self-contained:

1. `read.go`: export the literal-size error and detect it in
   `IsParseError`.
2. `client/client.go`: add a `MaxLiteralSize` constructor
   option, close the conn on parse errors, and preserve the
   read error through the executor.
3. `client/client.go`: **apply the cap before the reader is
   used.** `NewWithOptions` must not call `New` and then assign
   `c.conn.Reader.MaxLiteralSize`. `New` constructs the reader,
   parses the server greeting and starts the background reader
   goroutine, so a post-construction assignment would (a) race
   with the reader goroutine's read of `Reader.MaxLiteralSize`
   in `ReadLiteral`, and (b) leave a window in which the live
   reader goroutine could accept an over-cap literal with no
   cap at all, i.e. an unbounded allocation.

   The constructor is therefore refactored so the cap is
   assigned to the `*imap.Reader` at creation time, before the
   reader is installed in the `Conn`, before the greeting is
   parsed, and before the reader goroutine exists. `New(conn)`
   keeps its upstream signature and behaviour and is equivalent
   to `NewWithOptions(conn, DialOptions{})`.

   `Reader.MaxLiteralSize` is now written in exactly one place
   (the constructor) and only before any goroutine that could
   read it exists. The other write in the tree is
   `server/conn.go`, which sets the limit on the *server's*
   reader at connection construction, before the server's
   `send` goroutine and before its command loop start; it is
   likewise not a post-start mutation.

These are suitable for an upstream pull request against
`github.com/emersion/go-imap`. The alternative — replacing the
library with a different IMAP client — is documented in
TheMailroom's `docs/PHASE2_DESIGN.md` §10 as the fallback if
upstream declines.

Until the fork is either upstreamed or replaced, TheMailroom
depends on this module by its **public module path and an
immutable version tag** — there is **no `replace` directive**.
The dependency is therefore resolved from the public module
proxy (or a direct VCS fetch) exactly like any other
dependency, and the build is reproducible from `go.mod` +
`go.sum` alone.

The version is pinned to an immutable tag. Tags in this
repository are never moved: a change to the fork gets a new
tag, and the previous tag keeps pointing at the commit it was
published from.

Verification procedure:

```sh
go mod verify                              # module contents match go.sum
go list -m github.com/flowbyte-com/go-imap  # the pinned version
go test -race -count=1 ./...               # TheMailroom's full suite
```

TheMailroom additionally asserts the fork's source-level
ordering (the cap check precedes the buffer allocation) so an
upstream upgrade that reintroduced the allocation would be
caught immediately.

See TheMailroom's `docs/PHASE2_DESIGN.md` §10 and
`docs/THREAT_MODEL.md` T4.

## License

Upstream `github.com/emersion/go-imap` is BSD-2-Clause. The
license is preserved verbatim in `LICENSE`.
