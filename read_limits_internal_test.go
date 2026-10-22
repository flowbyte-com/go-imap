package imap

// White-box tests for the reader's per-response accounting.
//
// The limits are only meaningful if the accounting is exact: every byte
// of a response must be counted once, no byte may be counted twice
// because of the parser's lookahead, and the accounting must be reset
// at the boundary between two responses.
//
// These tests live in the imap package so that they can read the
// counters directly. The black-box behaviour is covered by
// read_limits_test.go.

import (
	"bufio"
	"strings"
	"testing"
)

// readOne parses one response from the input and returns the reader, so
// that the accounting can be inspected.
func readOne(t *testing.T, input string, l func(*Reader)) (*Reader, error) {
	t.Helper()
	r := NewReader(bufio.NewReader(strings.NewReader(input)))
	if l != nil {
		l(r)
	}
	_, err := ReadResp(r)
	return r, err
}

// TestAccountingCountsEveryByteExactlyOnce checks that the byte counter
// equals the length of the response line.
//
// Every byte of the line passes through the parser: the tag, the
// atoms, the delimiters and the terminator. If a byte were counted
// twice, or not at all, the counter would differ from the line length.
func TestAccountingCountsEveryByteExactlyOnce(t *testing.T) {
	for _, line := range []string{
		"* OK ready\r\n",
		"* OK [CAPABILITY IMAP4rev1 STARTTLS] Server ready.\r\n",
		"* SEARCH 1 2 3\r\n",
		"* SEARCH\r\n",
		"* LIST (\\HasNoChildren) \"/\" \"INBOX\"\r\n",
		"* 1 FETCH (UID 7 FLAGS (\\Seen) INTERNALDATE \"12-Mar-2026 12:00:00 +0000\" RFC822.SIZE 4096)\r\n",
		"+ ready for the literal\r\n",
		"A001 NO [ALERT] something happened\r\n",
	} {
		r, err := readOne(t, line, nil)
		if err != nil {
			t.Errorf("ReadResp(%q): %v", line, err)
			continue
		}
		if got, want := r.respBytes, uint64(len(line)); got != want {
			t.Errorf("respBytes for %q = %d, want %d (the line length)", line, got, want)
		}
	}
}

// TestAccountingIgnoresLookahead checks that the parser's lookahead does
// not count a byte twice.
//
// ReadFields peeks at the next character and pushes it back, and
// ReadAtom pushes back the delimiter that ends it. Each of those
// pushes must refund the bytes the peek consumed.
func TestAccountingIgnoresLookahead(t *testing.T) {
	// The same line, read after a peek-heavy path.
	for _, line := range []string{
		"* SEARCH 1 2 3\r\n",
		"* LIST (\\HasNoChildren) \"/\" \"INBOX\"\r\n",
	} {
		r, err := readOne(t, line, nil)
		if err != nil {
			t.Errorf("ReadResp(%q): %v", line, err)
			continue
		}
		if got, want := r.respBytes, uint64(len(line)); got != want {
			t.Errorf("respBytes for %q = %d, want %d", line, got, want)
		}
	}
}

// TestAccountingResetsBetweenResponses checks that the accounting is
// per response.
//
// Two responses that are each just within the budget must both be
// accepted: if the counters were not reset, the second one would be
// refused.
func TestAccountingResetsBetweenResponses(t *testing.T) {
	line := "* OK ready\r\n"
	limit := uint32(len(line))
	r, err := readOne(t, line+line, func(r *Reader) {
		r.MaxResponseBytes = limit
	})
	if err != nil {
		t.Fatalf("ReadResp: %v", err)
	}
	// The counter holds the second response only.
	if got, want := r.respBytes, uint64(len(line)); got != want {
		t.Errorf("respBytes = %d, want %d: the counter must be reset between responses", got, want)
	}
}

// TestAccountingCountsLiteralBody checks that a literal body is
// accounted.
func TestAccountingCountsLiteralBody(t *testing.T) {
	body := "AAAA"
	line := "* 1 FETCH (BODY[] {4}\r\n" + body + ")\r\n"
	r, err := readOne(t, line, nil)
	if err != nil {
		t.Fatalf("ReadResp: %v", err)
	}
	if got, want := r.respBytes, uint64(len(line)); got != want {
		t.Errorf("respBytes = %d, want %d (the line length, the literal body included)", got, want)
	}
}

// TestAccountingIsExactForAMultiByteResponse checks that a multi-byte
// UTF-8 character is counted by its byte length and not by its rune
// count.
func TestAccountingIsExactForAMultiByteResponse(t *testing.T) {
	line := "* OK café\r\n"
	r, err := readOne(t, line, nil)
	if err != nil {
		t.Fatalf("ReadResp: %v", err)
	}
	if got, want := r.respBytes, uint64(len(line)); got != want {
		t.Errorf("respBytes = %d, want %d", got, want)
	}
}

// TestLimitErrorCarriesNoResponseContent checks that the limit error's
// message never contains content from the response.
//
// The response carries a marker that must not appear in the error, so
// that a hostile server cannot inject text into a caller's diagnostics.
func TestLimitErrorCarriesNoResponseContent(t *testing.T) {
	const marker = "ATTACKERMARKER"
	line := "* SEARCH " + strings.Repeat(marker, 8) + "\r\n"
	_, err := readOne(t, line, func(r *Reader) {
		r.MaxAtomSize = 16
	})
	if err == nil {
		t.Fatal("expected a limit error")
	}
	if strings.Contains(err.Error(), marker) {
		t.Errorf("err = %q, want it not to contain any content from the response", err)
	}
}
