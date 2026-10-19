# go-imap fork

A narrowly scoped fork of `github.com/emersion/go-imap` v1.2.1
that adds protocol-level IMAP literal-size enforcement. This fork
is used by TheMailroom to satisfy the pre-production security
requirement documented in `docs/PHASE2_DESIGN.md` §10.

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

## Verification

The fork preserves all upstream tests. A new test
`TestLiteralSizeCap` in `read_test.go` exercises the boundary
at cap-1, cap, cap+1. The TheMailroom fork has its own
integration tests in `internal/imapclient/correctness_test.go`
that cover both implicit-TLS and STARTTLS.

## License

Preserved from upstream: BSD-2-Clause. See upstream `LICENSE`.
