package imap_test

// Regression tests for the reader's structural limits.
//
// The reader is the component that turns a server's bytes into Go
// values. Before the limits, three of its paths could allocate an
// unbounded amount of memory from a single response:
//
//   - ReadFields grew a []interface{} with one entry per field, with
//     no limit on the field count;
//   - ReadAtom grew a string one character at a time, so a long atom
//     cost quadratic time and memory;
//   - ReadInfo and the literal length declaration were read with a
//     single ReadString, so the whole line was accumulated before its
//     size could be looked at.
//
// The limits bound all of them. Every limit is zero by default, so a
// caller that does not opt in keeps the upstream behaviour exactly.
//
// These tests use deliberately small limits, so that a regression
// (the limit not being enforced) is visible immediately and cheaply,
// without the test allocating gigabytes.

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/flowbyte-com/go-imap"
)

// decoderLimits configures a reader for the tests. A zero value means
// "no limit", exactly as in the public API.
type decoderLimits struct {
	fields uint32
	atom   uint32
	quoted uint32
	bytes  uint32
	depth  uint32
}

// limitedReader returns a reader with the given limits over the input.
func limitedReader(input string, l decoderLimits) *imap.Reader {
	r := imap.NewReader(bufio.NewReader(strings.NewReader(input)))
	r.MaxResponseFields = l.fields
	r.MaxAtomSize = l.atom
	r.MaxQuotedStringSize = l.quoted
	r.MaxResponseBytes = l.bytes
	r.MaxListDepth = l.depth
	return r
}

// requireLimitError asserts that err is a decoder-limit violation and
// not a generic malformed-response error.
func requireLimitError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected a decoder-limit error, got nil")
	}
	if !errors.Is(err, imap.ErrResponseTooLarge) {
		t.Fatalf("err = %v, want errors.Is(err, imap.ErrResponseTooLarge)", err)
	}
	// A limit violation must be distinguishable from a malformed
	// response, and it must not be mistaken for a literal-size
	// violation.
	if errors.Is(err, imap.ErrLiteralSizeExceeded) {
		t.Errorf("err = %v, want it not to be a literal-size violation", err)
	}
	if !imap.IsParseError(err) {
		t.Errorf("err = %v, want it to be a parse error so the connection is torn down", err)
	}
}

// TestReaderAtomSizeBoundary covers the exact boundary of the atom
// limit: one byte under is accepted, the limit itself is accepted, and
// one byte over is refused.
func TestReaderAtomSizeBoundary(t *testing.T) {
	const limit = 16
	for _, tc := range []struct {
		name    string
		atomLen int
		wantErr bool
	}{
		{"limit-1 accepted", limit - 1, false},
		{"limit accepted", limit, false},
		{"limit+1 refused", limit + 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			atom := strings.Repeat("x", tc.atomLen)
			line := "* SEARCH " + atom + "\r\n"
			// Only the atom limit is armed: the byte budget is large
			// enough for the whole line.
			r := limitedReader(line, decoderLimits{atom: limit, bytes: 4096})
			_, err := imap.ReadResp(r)
			if tc.wantErr {
				requireLimitError(t, err)
				return
			}
			if err != nil {
				t.Fatalf("ReadResp: %v", err)
			}
		})
	}
}

// TestReadRespDoesNotSwallowALimitViolation checks that a limit
// violation in the response's second atom is not swallowed.
//
// ReadResp parses the second atom speculatively: if it can be read and
// it names a status, the response is a status response, otherwise it is
// a data response whose name is that atom. A limit violation in the
// middle of that atom used to be discarded, and the fallback then
// parsed the *remainder* of the oversized atom as if it were a field,
// so a partially parsed data response was accepted.
func TestReadRespDoesNotSwallowALimitViolation(t *testing.T) {
	// The second atom is seventeen bytes long, one over the limit.
	atom := strings.Repeat("x", 17)
	for _, line := range []string{
		"* " + atom + "\r\n",
		"* SEARCH " + atom + "\r\n",
	} {
		r := limitedReader(line, decoderLimits{atom: 16, bytes: 4096})
		resp, err := imap.ReadResp(r)
		if err == nil {
			t.Fatalf("ReadResp(%q) returned %v, want a limit violation", line, resp)
		}
		if !errors.Is(err, imap.ErrResponseTooLarge) {
			t.Errorf("ReadResp(%q): err = %v, want errors.Is(err, imap.ErrResponseTooLarge)", line, err)
		}
		if resp != nil {
			t.Errorf("ReadResp(%q) returned a response, want none: a partially parsed response must never be accepted", line)
		}
	}
}

// TestReadRespLimitViolationAfterTheSecondAtom checks the other
// speculative read: the space that separates the response's name from
// the rest.
//
// The space is consumed and then pushed back, so the response stream
// stays aligned and the fallback would re-derive the same violation.
// The violation is nevertheless returned immediately, so the response is
// never parsed twice.
func TestReadRespLimitViolationAfterTheSecondAtom(t *testing.T) {
	// "* SEARCH " is nine bytes: the tag, the space, the name and the
	// space. A budget of eight leaves no room for the space.
	line := "* SEARCH x\r\n"
	r := limitedReader(line, decoderLimits{bytes: 8})
	resp, err := imap.ReadResp(r)
	if err == nil {
		t.Fatalf("ReadResp returned %v, want a limit violation", resp)
	}
	if !errors.Is(err, imap.ErrResponseTooLarge) {
		t.Errorf("err = %v, want errors.Is(err, imap.ErrResponseTooLarge)", err)
	}
	if resp != nil {
		t.Error("a response was returned despite the limit violation")
	}
}

// TestReaderAtomSizeCountsUTF8Bytes checks that the atom limit is
// measured in the bytes the atom holds, not in the bytes the response
// used.
//
// An invalid byte is decoded into the replacement rune, which occupies
// three bytes in the atom but only one byte in the response.
func TestReaderAtomSizeCountsUTF8Bytes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		atom    string
		limit   uint32
		wantErr bool
	}{
		{"ascii at the limit", strings.Repeat("x", 16), 16, false},
		{"ascii one over the limit", strings.Repeat("x", 17), 16, true},
		{"two byte characters at the limit", strings.Repeat("\u00e9", 8), 16, false},
		{"two byte characters over the limit", strings.Repeat("\u00e9", 9), 16, true},
		{"three byte characters at the limit", strings.Repeat("\u20ac", 5), 15, false},
		{"three byte characters over the limit", strings.Repeat("\u20ac", 6), 15, true},
		{"four byte characters at the limit", strings.Repeat("\U0001d11e", 4), 16, false},
		{"four byte characters over the limit", strings.Repeat("\U0001d11e", 5), 16, true},
		// An invalid byte becomes the replacement rune, which is three
		// bytes in the atom but one byte in the response.
		{"invalid utf-8 at the limit", strings.Repeat("\xff", 5), 15, false},
		{"invalid utf-8 over the limit", strings.Repeat("\xff", 5), 14, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line := "* " + tc.atom + "\r\n"
			r := limitedReader(line, decoderLimits{atom: tc.limit, bytes: 4096})
			resp, err := imap.ReadResp(r)
			if tc.wantErr {
				requireLimitError(t, err)
				if resp != nil {
					t.Error("a response was returned despite the limit violation")
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadResp: %v", err)
			}
			data, ok := resp.(*imap.DataResp)
			if !ok {
				t.Fatalf("resp = %T, want a data response", resp)
			}
			// The retained content must respect the byte bound.
			name, ok := data.Fields[0].(string)
			if !ok {
				t.Fatalf("field 0 = %T, want the response name", data.Fields[0])
			}
			if uint32(len(name)) > tc.limit {
				t.Errorf("the retained atom is %d bytes, want at most %d", len(name), tc.limit)
			}
		})
	}
}

// TestReaderQuotedStringSizeCountsUTF8Bytes checks that the
// quoted-string limit is measured in the bytes the quoted string holds.
//
// The limit applies to the content, which is what the reader
// allocates. The source bytes of an escape sequence are accounted by
// the response's byte budget instead.
func TestReaderQuotedStringSizeCountsUTF8Bytes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		limit   uint32
		wantErr bool
	}{
		{"ascii at the limit", strings.Repeat("x", 16), 16, false},
		{"ascii one over the limit", strings.Repeat("x", 17), 16, true},
		{"two byte characters at the limit", strings.Repeat("\u00e9", 8), 16, false},
		{"two byte characters over the limit", strings.Repeat("\u00e9", 9), 16, true},
		{"three byte characters at the limit", strings.Repeat("\u20ac", 5), 15, false},
		{"three byte characters over the limit", strings.Repeat("\u20ac", 6), 15, true},
		{"four byte characters at the limit", strings.Repeat("\U0001d11e", 4), 16, false},
		{"four byte characters over the limit", strings.Repeat("\U0001d11e", 5), 16, true},
		{"invalid utf-8 at the limit", strings.Repeat("\xff", 5), 15, false},
		{"invalid utf-8 over the limit", strings.Repeat("\xff", 5), 14, true},
		// An escaped quote and an escaped backslash are one content
		// byte each, but two source bytes each.
		{"escaped quotes at the limit", strings.Repeat(`\"`, 16), 16, false},
		{"escaped quotes over the limit", strings.Repeat(`\"`, 17), 16, true},
		{"escaped backslashes at the limit", strings.Repeat(`\\`, 16), 16, false},
		{"escaped backslashes over the limit", strings.Repeat(`\\`, 17), 16, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line := `* SEARCH "` + tc.content + `"` + "\r\n"
			r := limitedReader(line, decoderLimits{quoted: tc.limit, bytes: 1 << 16})
			resp, err := imap.ReadResp(r)
			if tc.wantErr {
				requireLimitError(t, err)
				if resp != nil {
					t.Error("a response was returned despite the limit violation")
				}
				return
			}
			if err != nil {
				t.Fatalf("ReadResp: %v", err)
			}
			data, ok := resp.(*imap.DataResp)
			if !ok {
				t.Fatalf("resp = %T, want a data response", resp)
			}
			content, ok := data.Fields[1].(string)
			if !ok {
				t.Fatalf("field 1 = %T, want the quoted string", data.Fields[1])
			}
			if uint32(len(content)) > tc.limit {
				t.Errorf("the retained quoted string is %d bytes, want at most %d", len(content), tc.limit)
			}
		})
	}
}

// TestReaderQuotedStringSizeBoundary covers the quoted-string limit,
// including the case where the quoted string contains escaped
// characters.
//
// The limit applies to the *content* of the quoted string, which is
// what the reader allocates. The source bytes of an escape sequence
// are accounted by the response's byte budget instead.
func TestReaderQuotedStringSizeBoundary(t *testing.T) {
	const limit = 16
	for _, tc := range []struct {
		name    string
		content string
		wantErr bool
	}{
		{"limit-1 accepted", strings.Repeat("x", limit-1), false},
		{"limit accepted", strings.Repeat("x", limit), false},
		{"limit+1 refused", strings.Repeat("x", limit+1), true},
		{"escapes are counted once", strings.Repeat(`\"`, limit), false},
		{"escapes over the limit", strings.Repeat(`\"`, limit+1), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			line := `* SEARCH "` + tc.content + `"` + "\r\n"
			r := limitedReader(line, decoderLimits{quoted: limit, bytes: 4096})
			_, err := imap.ReadResp(r)
			if tc.wantErr {
				requireLimitError(t, err)
				return
			}
			if err != nil {
				t.Fatalf("ReadResp: %v", err)
			}
		})
	}
}

// TestReaderFieldCountBoundary covers the field-count limit and the
// same limit applied to deeply nested fields.
//
// The counter spans every nesting level: a response that holds its
// fields inside nested lists is bounded by the same counter as a flat
// one.
func TestReaderFieldCountBoundary(t *testing.T) {
	// The response "* SEARCH 1 2 3 4 5 6 7" holds eight fields: the
	// name "SEARCH" and the seven UIDs.
	const limit = 8
	for _, tc := range []struct {
		name    string
		line    string
		wantErr bool
	}{
		{
			name:    "limit-1 accepted",
			line:    "* SEARCH 1 2 3 4 5 6\r\n",
			wantErr: false,
		},
		{
			name:    "limit accepted",
			line:    "* SEARCH 1 2 3 4 5 6 7\r\n",
			wantErr: false,
		},
		{
			name:    "limit+1 refused",
			line:    "* SEARCH 1 2 3 4 5 6 7 8\r\n",
			wantErr: true,
		},
		{
			// The same number of fields, but nested inside a list: the
			// list itself is a field too.
			name:    "nested limit accepted",
			line:    "* SEARCH (1 2 3 4 5 6)\r\n",
			wantErr: false,
		},
		{
			name:    "nested limit+1 refused",
			line:    "* SEARCH (1 2 3 4 5 6 7)\r\n",
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := limitedReader(tc.line, decoderLimits{fields: limit, bytes: 4096})
			_, err := imap.ReadResp(r)
			if tc.wantErr {
				requireLimitError(t, err)
				return
			}
			if err != nil {
				t.Fatalf("ReadResp: %v", err)
			}
		})
	}
}

// TestReaderListDepthBoundary covers the nesting-depth limit and a
// response made of many empty nested lists.
//
// The depth is checked before the nested list is parsed, so a deeply
// nested response is refused before the recursion can exhaust the
// goroutine stack.
func TestReaderListDepthBoundary(t *testing.T) {
	const limit = 4
	for _, tc := range []struct {
		name    string
		line    string
		wantErr bool
	}{
		{
			name:    "depth-1 accepted",
			line:    "* SEARCH ()\r\n",
			wantErr: false,
		},
		{
			name:    "depth accepted",
			line:    "* SEARCH " + strings.Repeat("(", limit) + strings.Repeat(")", limit) + "\r\n",
			wantErr: false,
		},
		{
			name:    "depth+1 refused",
			line:    "* SEARCH " + strings.Repeat("(", limit+1) + strings.Repeat(")", limit+1) + "\r\n",
			wantErr: true,
		},
		{
			name:    "excessive empty nested lists refused",
			line:    "* SEARCH " + strings.Repeat("(", 5000) + strings.Repeat(")", 5000) + "\r\n",
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := limitedReader(tc.line, decoderLimits{depth: limit, bytes: 1 << 20})
			_, err := imap.ReadResp(r)
			if tc.wantErr {
				requireLimitError(t, err)
				return
			}
			if err != nil {
				t.Fatalf("ReadResp: %v", err)
			}
		})
	}
}

// TestReaderResponseBytesBoundary covers the response-byte limit at its
// exact boundary.
//
// The limit covers every byte of the response: the tag, the atoms, the
// delimiters and the line terminator.
func TestReaderResponseBytesBoundary(t *testing.T) {
	line := "* OK ready\r\n"
	limit := uint32(len(line))
	// The response is exactly `limit` bytes long, so a budget of
	// `limit` accepts it and a budget of `limit - 1` refuses it.
	for _, tc := range []struct {
		name    string
		limit   uint32
		wantErr bool
	}{
		{"limit-1 refused", limit - 1, true},
		{"limit accepted", limit, false},
		{"limit+1 accepted", limit + 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := limitedReader(line, decoderLimits{bytes: tc.limit})
			_, err := imap.ReadResp(r)
			if tc.wantErr {
				requireLimitError(t, err)
				return
			}
			if err != nil {
				t.Fatalf("ReadResp: %v", err)
			}
		})
	}
}

// TestReaderOversizedStatusInfo covers an oversized status text.
//
// The status text is read with ReadInfo, which used to accumulate the
// whole line before its size could be checked.
func TestReaderOversizedStatusInfo(t *testing.T) {
	const limit = 64
	for _, tc := range []struct {
		name    string
		line    string
		wantErr bool
	}{
		{
			name:    "short status text accepted",
			line:    "* OK " + strings.Repeat("x", 10) + "\r\n",
			wantErr: false,
		},
		{
			name:    "oversized status text refused",
			line:    "* OK " + strings.Repeat("x", limit) + "\r\n",
			wantErr: true,
		},
		{
			name:    "oversized continuation text refused",
			line:    "+ " + strings.Repeat("x", limit) + "\r\n",
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := limitedReader(tc.line, decoderLimits{bytes: limit})
			_, err := imap.ReadResp(r)
			if tc.wantErr {
				requireLimitError(t, err)
				return
			}
			if err != nil {
				t.Fatalf("ReadResp: %v", err)
			}
		})
	}
}

// TestReaderOversizedLiteralLengthDeclaration covers a literal length
// declaration that is longer than any legitimate one.
//
// The declaration used to be read with a single ReadString, so a
// declaration that was never terminated could be accumulated without
// limit.
func TestReaderOversizedLiteralLengthDeclaration(t *testing.T) {
	for _, tc := range []struct {
		name    string
		decl    string
		wantErr bool
	}{
		{"short declaration accepted", "42", false},
		{"long declaration refused", strings.Repeat("9", 64), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The declaration is followed by a body of the announced
			// length, so that a short declaration is accepted and the
			// failure is the declaration and not the missing body.
			n, convErr := strconv.Atoi(tc.decl)
			if convErr != nil {
				n = 0
			}
			body := ""
			if n > 0 {
				body = strings.Repeat("A", n)
			}
			line := "* 1 FETCH (UID 1 BODY[] {" + tc.decl + "}\r\n" + body + ")\r\n"
			r := limitedReader(line, decoderLimits{bytes: 1 << 20})
			_, err := imap.ReadResp(r)
			if tc.wantErr {
				requireLimitError(t, err)
				return
			}
			if err != nil {
				t.Fatalf("ReadResp: %v", err)
			}
		})
	}
}

// TestReaderLiteralExceedingResponseBudget covers a literal that is
// within the literal-size cap but overruns the aggregate response
// budget.
//
// The body must be refused before it is allocated, so the reader must
// fail without waiting for a body the server may never send.
func TestReaderLiteralExceedingResponseBudget(t *testing.T) {
	const literalCap = 4096
	const responseBudget = 256
	// The body fits the literal cap but not the response budget.
	body := strings.Repeat("A", literalCap)
	line := fmt.Sprintf("* 1 FETCH (UID 1 BODY[] {%d}\r\n%s)\r\n", len(body), body)

	r := limitedReader(line, decoderLimits{bytes: responseBudget})
	_, err := imap.ReadResp(r)
	requireLimitError(t, err)
}

// TestReaderNormalLiteralAccepted covers a literal within both the
// literal-size cap and the response budget: it must be parsed and
// delivered unchanged.
func TestReaderNormalLiteralAccepted(t *testing.T) {
	const literalCap = 4096
	const responseBudget = 1 << 16
	body := strings.Repeat("A", literalCap)
	line := fmt.Sprintf("* 1 FETCH (UID 1 BODY[] {%d}\r\n%s)\r\n", len(body), body)

	r := limitedReader(line, decoderLimits{bytes: responseBudget})
	resp, err := imap.ReadResp(r)
	if err != nil {
		t.Fatalf("ReadResp: %v", err)
	}
	data, ok := resp.(*imap.DataResp)
	if !ok {
		t.Fatalf("resp = %T, want a data response", resp)
	}
	if len(data.Fields) != 3 {
		t.Fatalf("got %d fields, want 3", len(data.Fields))
	}
	// The fields are the sequence number, "FETCH" and the item list.
	items, ok := data.Fields[2].([]interface{})
	if !ok {
		t.Fatalf("field 2 = %T, want the item list", data.Fields[2])
	}
	// The items are "UID", the UID, "BODY[]" and the literal.
	if len(items) != 4 {
		t.Fatalf("got %d items, want 4", len(items))
	}
	section, err := imap.ParseString(items[3])
	if err != nil {
		t.Fatal(err)
	}
	if section != body {
		t.Errorf("body length = %d, want %d", len(section), len(body))
	}
}

// TestReaderMaximumSizeLiteralRemainsValid checks that a literal of
// exactly the permitted maximum size is still accepted once the
// response's byte budget leaves room for the protocol overhead.
//
// The literal cap and the response budget are independent controls: the
// literal cap bounds one literal, the response budget bounds the
// response. A caller that sets the response budget to the literal
// maximum alone would refuse a maximum-size literal, because the
// response also carries the items around it.
func TestReaderMaximumSizeLiteralRemainsValid(t *testing.T) {
	const literalCap = 4096
	// The response budget leaves room for the literal and for the
	// response's own overhead: the items, the delimiters and the
	// literal's length declaration.
	const overhead = 1024
	body := strings.Repeat("A", literalCap)
	line := fmt.Sprintf("* 1 FETCH (UID 1 BODY[] {%d}\r\n%s)\r\n", len(body), body)

	r := limitedReader(line, decoderLimits{bytes: literalCap + overhead})
	r.MaxLiteralSize = literalCap
	resp, err := imap.ReadResp(r)
	if err != nil {
		t.Fatalf("ReadResp: %v", err)
	}
	data, ok := resp.(*imap.DataResp)
	if !ok {
		t.Fatalf("resp = %T, want a data response", resp)
	}
	items, ok := data.Fields[2].([]interface{})
	if !ok {
		t.Fatalf("field 2 = %T, want the item list", data.Fields[2])
	}
	section, err := imap.ParseString(items[3])
	if err != nil {
		t.Fatal(err)
	}
	if len(section) != literalCap {
		t.Errorf("body length = %d, want %d", len(section), literalCap)
	}
}

// TestReaderCountersResetBetweenResponses checks that the accounting is
// per response.
//
// Two responses that are each just within the limit must both be
// accepted. If the counters were not reset, the second one would be
// refused.
func TestReaderCountersResetBetweenResponses(t *testing.T) {
	line := "* OK ready\r\n"
	limit := uint32(len(line))
	r := limitedReader(line+line, decoderLimits{bytes: limit})
	for i := 0; i < 2; i++ {
		if _, err := imap.ReadResp(r); err != nil {
			t.Fatalf("ReadResp %d: %v", i, err)
		}
	}
	// A third response that is one byte over the limit must be
	// refused, which shows the counter is live again after the reset.
	long := "* OK readyx\r\n"
	r2 := limitedReader(long, decoderLimits{bytes: limit})
	if _, err := imap.ReadResp(r2); err == nil {
		t.Fatal("expected the over-budget response to be refused")
	} else {
		requireLimitError(t, err)
	}
}

// TestReaderMalformedAndTruncatedResponses checks that a malformed or
// truncated response is still reported as a malformed response and not
// as a limit violation, and that the reader never panics.
func TestReaderMalformedAndTruncatedResponses(t *testing.T) {
	for _, tc := range []struct {
		name string
		line string
	}{
		{"truncated in the middle of an atom", "* SEARCH abc"},
		{"truncated in the middle of a quoted string", `* SEARCH "abc`},
		{"truncated in the middle of a literal", "* 1 FETCH (BODY[] {10}\r\nabc"},
		{"a field that is not a number", "* SEARCH (1 2)\r\n"},
		{"an unbalanced list", "* SEARCH (1 2\r\n"},
		{"an unbalanced response code", "* OK [CAPABILITY (unclosed\r\n"},
		{"a stray closing bracket", "* SEARCH ]\r\n"},
		{"an empty response", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := limitedReader(tc.line, decoderLimits{
				fields: 16,
				atom:   64,
				quoted: 64,
				bytes:  4096,
				depth:  4,
			})
			_, err := imap.ReadResp(r)
			if err == nil {
				// A malformed response may still parse; the point of
				// the test is that nothing panics and that a limit
				// violation is not reported for a malformed response.
				return
			}
			if errors.Is(err, imap.ErrResponseTooLarge) {
				t.Errorf("err = %v, want a malformed-response error, not a limit violation", err)
			}
		})
	}
}

// TestReaderNoLimitsPreservesUpstreamBehaviour checks that a reader
// with no limits accepts what the upstream reader accepts.
func TestReaderNoLimitsPreservesUpstreamBehaviour(t *testing.T) {
	inputs := []string{
		"* OK [CAPABILITY IMAP4rev1 STARTTLS AUTH=PLAIN UNSELECT] Server ready.\r\n",
		"* SEARCH 1 2 3\r\n",
		`* SEARCH "a quoted string"` + "\r\n",
		"* LIST (\\HasNoChildren) \"/\" \"INBOX\"\r\n",
		"* 3 EXISTS\r\n",
		"* OK [UIDVALIDITY 1] Untagged EXISTS response\r\n",
		"A002 OK [READ-WRITE] SELECT completed\r\n",
		"* 1 FETCH (UID 7 FLAGS (\\Seen) INTERNALDATE \"12-Mar-2026 12:00:00 +0000\" RFC822.SIZE 4096)\r\n",
		"+ ready for literal\r\n",
	}
	for _, in := range inputs {
		r := imap.NewReader(bufio.NewReader(strings.NewReader(in)))
		if _, err := imap.ReadResp(r); err != nil {
			t.Errorf("ReadResp(%q): %v", in, err)
		}
	}
}

// TestReaderAtomIsLinear checks that a long atom is read in linear
// memory.
//
// The upstream reader grew the atom with a string concatenation, so a
// 64 KiB atom allocated about 2 GiB. The threshold here is far above
// what a linear reader needs and far below what the quadratic one
// allocated, so the test is not sensitive to the exact figures.
func TestReaderAtomIsLinear(t *testing.T) {
	const atomLen = 64 * 1024
	line := "* SEARCH " + strings.Repeat("x", atomLen) + "\r\n"

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	r := limitedReader(line, decoderLimits{bytes: 1 << 20})
	if _, err := imap.ReadResp(r); err != nil {
		t.Fatalf("ReadResp: %v", err)
	}
	runtime.ReadMemStats(&after)

	// A linear reader allocates a small multiple of the atom's length.
	// The threshold is a mebibyte, which is sixteen times the atom.
	const budget = 1 << 20
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > budget {
		t.Errorf("reading a %d byte atom allocated %d bytes, want at most %d: the atom must be built in linear space",
			atomLen, allocated, budget)
	}
}

// unusedIO keeps the io import referenced.
var _ = io.Discard
