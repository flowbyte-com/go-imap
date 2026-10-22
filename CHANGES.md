# Changes from upstream v1.2.1

This fork adds **protocol-level IMAP literal-size enforcement** and
**protocol-level IMAP response limits** to
`github.com/emersion/go-imap` v1.2.1.

It is used by TheMailroom to satisfy the mandatory pre-production
security requirement in
`github.com/flowbyte-com/themailroom/docs/PHASE2_DESIGN.md` §10.

## Files changed

The response limits change three upstream source files (`read.go`,
`response.go` and `client/client.go`); the literal-size cap changed
two (`read.go` and `client/client.go`). Everything else is
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

## Response limits (second change set)

The literal-size cap bounds one literal. It does not bound a
*response*: before this change set, three of the reader's paths could
allocate an unbounded amount of memory from a single response.

### The allocation points that are now bounded

1. **`ReadFields` grew a `[]interface{}` with one entry per field**,
   with no limit on the field count. A SEARCH response with a million
   UIDs allocated 128 MB.
2. **`ReadAtom` grew a string one character at a time**
   (`atom += string(char)`), so the cost of a long atom was
   *quadratic* in its length: a 64 KiB atom allocated about 2 GiB, and
   a 256 KiB atom about 35 GB.
3. **`ReadInfo` and the literal length declaration were read with a
   single `ReadString`**, so the whole line was accumulated before its
   size could be looked at.

### The new limits

The limits live on `imap.Reader` and are set through
`client.DialOptions`, exactly like the existing `MaxLiteralSize`:

```go
type DialOptions struct {
    MaxLiteralSize    MaxLiteralSize
    MaxResponseFields uint32 // fields per response, at any nesting level
    MaxAtomSize       uint32 // bytes per atom
    MaxQuotedStringSize uint32 // bytes per quoted string
    MaxResponseBytes  uint32 // bytes per response
    MaxListDepth      uint32 // nesting depth of the lists
}
```

Every limit is **zero by default, which means "no limit"**, so a
caller that does not opt in keeps the upstream behaviour exactly.
`client.NewWithOptions` applies them to the reader *before* the reader
is installed in the connection and before the background reader
goroutine starts, so they are in effect for the very first byte read
from the connection and there is no window in which they are not.

The limits survive a STARTTLS upgrade: `imap.Conn.Upgrade` calls
`init()`, which resets the buffered reader but keeps the same
`*imap.Reader` and therefore the same limits.

### What the response-byte limit covers

Every byte of a response is counted exactly once, at the point where
the parser consumes it: the response tag, the status or continuation
text, the literal length declarations, the literal bodies, the atoms,
the quoted strings and the delimiters between them.

The accounting is exact. `Reader.ReadRune` charges the rune's byte
size, and `Reader.UnreadRune` refunds it, so the parser's lookahead
never counts a byte twice. The counters are reset at the start of every
response, so the budget applies to each response separately.

The literal body is charged against the budget *before* it is
allocated, so a literal that would overrun the budget is refused
before the buffer exists.

### The literal length declaration: a fixed safety exception

The literal length declaration is read with a rune loop whose length is
bounded by `maxLiteralLengthDeclaration`, sixteen characters. A length
is a decimal number of at most ten digits, optionally followed by a
`+` for a non-synchronising literal, so the longest legitimate
declaration is eleven characters.

This limit is a **fixed safety exception**: it applies even when the
configurable decoder limits are disabled. The declaration used to be
read with a single `ReadString`, which could accumulate an unbounded
amount of memory for a declaration that was never terminated, so
removing the bound would reintroduce exactly the unbounded allocation
the limits exist to prevent.

The exception only changes the *error* a malformed declaration
produces: it is now a decoder-limit violation rather than a parse
error. A legitimate declaration is unaffected.

### The error

A violation is reported as `imap.ErrResponseTooLarge`, a value-type
error that implements `Is`, so `errors.Is(err, imap.ErrResponseTooLarge)`
is true. It is also recognised by `imap.IsParseError`, so the executor
treats it as fatal: it aborts the parse, closes the connection and
surfaces the cause through the connection-closed error, whose `Unwrap`
keeps the sentinel reachable. A desynchronised connection is never
reused and a partially parsed response is never reported as a
success.

The error's message never contains any content from the response, so a
hostile server cannot inject text into a caller's diagnostics. A limit
violation is distinguishable from a generic malformed response.

### The linear atom construction

`ReadAtom` now builds the atom with a `strings.Builder`, so the work
and the allocation are linear in its length. Measured on a 16 KiB atom:
the baseline took 15.5 ms and allocated 142 MB per read, the new
reader takes 0.15 ms and allocates 67 KB - a hundredfold faster and
two thousandfold smaller, and the gap grows with the square of the
atom's length.

### The compatibility floor

The field limit applies to **every** response, not only to a search
result. An ordinary FETCH envelope holds about thirty fields (the
sequence number, the name, the item list and every item inside it,
including the nested envelope and address lists), so a field limit
below that makes ordinary responses unusable. A caller that derives
the field limit from a small scan budget must therefore apply a
floor.

TheMailroom's recommended configuration, when it enables the limits:

| Option | Value | Why |
|---|---|---|
| `MaxResponseFields` | `max(max_list_scan_messages + 2, 64)` | the budget plus the response's name, so that a result of *budget + 1* UIDs is still decodable and the application can produce its own typed refusal; the floor keeps an ordinary FETCH envelope usable |
| `MaxAtomSize` | 64 KiB | generously above any legitimate atom |
| `MaxQuotedStringSize` | 64 KiB | generously above any legitimate quoted string |
| `MaxResponseBytes` | `max_message_bytes + 1 MiB` | the largest legitimate literal body, plus the response's own overhead |
| `MaxListDepth` | 64 | generously above the deepest legitimate nesting |

### Corrections found in review

Three defects in the first version of this change set were found in
review and are fixed here.

**A limit violation in the response's second atom was swallowed.**
`ReadResp` parses the second atom speculatively: if it can be read and
it names a status, the response is a status response, otherwise it is a
data response whose name is that atom. The speculative parse discarded
the error, and the fallback then parsed the *remainder* of the oversized
atom as if it were a field, so a partially parsed data response was
accepted. A limit violation is now returned immediately. The generic
parse error keeps the upstream fallback, which is load-bearing: a data
response whose name is a list is read through it.

**The size of an atom and of a quoted string was measured in the wrong
unit.** Both checks assumed that a rune occupies one byte. A rune
occupies one to four bytes, and an invalid byte is decoded into the
replacement rune, which occupies three bytes in the parsed value but
only one byte in the response. The checks now use the number of bytes
the builder will actually append, so the retained content always
respects its configured byte bound.

**The field counter and the list depth counter could wrap.** Both are
compared against their limit *before* they are incremented now, so a
counter that has reached its maximum cannot wrap around and slip past
the limit. The uncapped byte counter saturates instead of wrapping.

### Residual risks

The limits bound one response. They do **not** bound:

- the number of responses a server sends on one connection;
- the work the **server** does to evaluate a SEARCH: the messages it
  reads, the CPU it spends and the time it takes are server-side
  computation that the client cannot observe or limit. The
  per-command timeout bounds the client's wait, not the server's work;
- the wall-clock duration of an operation.

The limits are not a substitute for a per-command timeout or for a
whole-operation resource budget.

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
