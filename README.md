# go-imap fork

A narrowly scoped fork of `github.com/emersion/go-imap` v1.2.1
that adds protocol-level IMAP literal-size enforcement,
protocol-level IMAP response limits and two client concurrency
fixes. This fork is used by TheMailroom to satisfy the
pre-production security requirements documented in
`docs/PHASE2_DESIGN.md` §10 and in TheMailroom's Phase 3 security
work.

## Upstream

- Upstream: `github.com/emersion/go-imap` v1.2.1
- License: BSD-2-Clause (preserved; see upstream `LICENSE`)
- This fork is a temporary measure. The plan is to upstream
  the changes once the design is validated. See
  `docs/PHASE2_DESIGN.md` §10 for the migration path back to
  upstream or to a different library.

## Changes from upstream

The fork is a minimal patch:

1. `read.go`: `ReadLiteral` now returns an exported
   `ErrLiteralSizeExceeded` error (in addition to
   `parseError`) when the announced literal size exceeds
   `Reader.MaxLiteralSize`. The allocation that follows
   (`b := make([]byte, n)`) is unchanged — the check happens
   *before* the allocation, so a 1 GiB announcement is
   rejected before the buffer is allocated.

2. `client/client.go`:
   - `client.Options` gains a `MaxLiteralSize uint32` field.
   - `client.New` propagates the option to the embedded
     `*imap.Reader.MaxLiteralSize`.
   - `readOnce` closes the connection on a parse error and
     stores the underlying error on the `Client`. The
     executor's `<-c.loggedOut` case returns the stored
     error so the caller can distinguish a literal-size
     violation from other connection failures.
   - `errClosed` is replaced with a typed
     `*connectionClosedError` that wraps the underlying
     error via `Unwrap`, preserving the specific cause.

3. `read.go`, `response.go` and `client/client.go`: the reader
   gains five structural limits, all of them **zero by default,
   which means "no limit"**:
   - `MaxResponseFields` — fields per response, counted across
     every nesting level;
   - `MaxAtomSize` — bytes per atom;
   - `MaxQuotedStringSize` — bytes per quoted string;
   - `MaxResponseBytes` — bytes per response, counting every
     byte the parser consumes: the tag, the status or
     continuation text, the literal length declarations, the
     literal bodies, the atoms, the quoted strings and the
     delimiters between them;
   - `MaxListDepth` — nesting depth of the lists.

   They are set through `client.DialOptions` and applied by
   `client.NewWithOptions` before the reader goroutine starts,
   exactly like the literal-size cap. A violation is reported as
   `imap.ErrResponseTooLarge`, which is a fatal parse error: the
   parse is aborted, the connection is closed, and the cause is
   preserved through the executor's error chain.

   `ReadAtom` also builds the atom with a `strings.Builder`, so
   the work and the allocation are linear in the atom's length
   instead of quadratic.

   The literal length declaration is read with a fixed sixteen
   character bound, which is a deliberate safety exception: it
   applies even when the configurable limits are disabled, because an
   unbounded declaration read would reintroduce the unbounded
   allocation the limits exist to prevent.

4. `client/client.go`: when `execute` observes the reader closing
   `loggedOut`, it re-checks the command-result channel before
   reporting a disconnect. A tagged completion queued before a
   following `* BYE` is returned as the command result, while a
   genuine mid-command disconnect still returns the typed
   connection-closed error.

5. `client/client.go`, `client/cmd_auth.go` and
   `client/mailbox_status.go`: selected-mailbox statuses are
   immutable snapshots. `Select` returns a completion snapshot,
   unilateral `EXISTS`/`RECENT` updates install new snapshots and
   `MailboxUpdate` carries the snapshot that became current.
   `Client.Mailbox` returns the latest snapshot. A status pointer
   that was already returned keeps its original values; it is no
   longer updated in place.

   See `CHANGES.md` for the full description, the recommended
   configuration, the compatibility note and the residual risks.

## Verification

The fork preserves all upstream tests. The literal-size cap has
a boundary test (`TestLiteralSizeCap` in `read_test.go`) and the
response limits have a dedicated suite:

- `read_limits_test.go` — the reader's behaviour at each limit's
  boundary, the malformed and truncated responses, and the
  compatibility with the unlimited reader;
- `read_limits_internal_test.go` — the exactness of the byte
  accounting, including the parser's lookahead, the literal body
  and a multi-byte character;
- `read_limits_bench_test.go` — the benchmarks that show the
  linear atom construction and the bounded search response;
- `client/limits_test.go` — the client's reaction to a violation:
  the connection is terminated, the cause is preserved, no
  partially parsed result is reported, a reconnect works, and the
  behaviour is the same under implicit TLS and STARTTLS;
- `client/execute_test.go` — tagged `OK`/`NO`/`BAD` results win over
  an immediately following `* BYE`, handler errors are preserved,
  and genuine disconnect/parser/decoder failures still propagate;
- `client/mailbox_snapshot_test.go` — SELECT/EXAMINE snapshots,
  unsolicited `EXISTS`/`RECENT` updates, map/slice independence,
  `MailboxUpdate`, `Client.Mailbox` and concurrent reads with
  disconnect.

TheMailroom has its own integration tests in
`internal/imapclient/correctness_test.go` that cover both
implicit-TLS and STARTTLS.

## License

Preserved from upstream: BSD-2-Clause. See upstream `LICENSE`.
