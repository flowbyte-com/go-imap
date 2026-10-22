package imap_test

// Benchmarks for the reader.
//
// They exist to make the change measurable: the baseline (upstream
// v1.2.1 and this fork before the decoder limits) reads a long atom
// in quadratic time and materialises a SEARCH response of any size,
// while the limited reader refuses both.
//
// The benchmarks are deliberately small enough to run in CI: the longest
// atom is 16 KiB, which the baseline handles in about 150 MB of
// allocation per iteration. A much longer atom would make the baseline
// benchmark take minutes.

import (
	"bufio"
	"fmt"
	"strings"
	"testing"

	"github.com/flowbyte-com/go-imap"
)

// benchmarkSearchLine builds a SEARCH response with n UIDs.
func benchmarkSearchLine(n int) string {
	var sb strings.Builder
	sb.WriteString("* SEARCH")
	for i := 0; i < n; i++ {
		sb.WriteString(" 1")
	}
	sb.WriteString("\r\n")
	return sb.String()
}

// BenchmarkReadSearchResponse measures the cost of reading a SEARCH
// response of n UIDs, with and without the field limit.
func BenchmarkReadSearchResponse(b *testing.B) {
	for _, n := range []int{1000, 10000, 100000} {
		line := benchmarkSearchLine(n)
		b.Run(fmt.Sprintf("uids=%d/unlimited", n), func(b *testing.B) {
			b.SetBytes(int64(len(line)))
			for i := 0; i < b.N; i++ {
				r := imap.NewReader(bufio.NewReader(strings.NewReader(line)))
				if _, err := imap.ReadResp(r); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("uids=%d/limited", n), func(b *testing.B) {
			b.SetBytes(int64(len(line)))
			for i := 0; i < b.N; i++ {
				r := imap.NewReader(bufio.NewReader(strings.NewReader(line)))
				r.MaxResponseFields = 1024
				r.MaxResponseBytes = 1 << 20
				_, _ = imap.ReadResp(r)
			}
		})
	}
}

// BenchmarkReadLongAtom measures the cost of reading a long atom.
//
// The baseline builds the atom with a string concatenation, so the cost
// is quadratic in the atom's length. The limited reader refuses the
// atom before it is built.
func BenchmarkReadLongAtom(b *testing.B) {
	const atomLen = 16 * 1024
	line := "* SEARCH " + strings.Repeat("x", atomLen) + "\r\n"
	b.Run("unlimited", func(b *testing.B) {
		b.SetBytes(int64(len(line)))
		for i := 0; i < b.N; i++ {
			r := imap.NewReader(bufio.NewReader(strings.NewReader(line)))
			if _, err := imap.ReadResp(r); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("limited", func(b *testing.B) {
		b.SetBytes(int64(len(line)))
		for i := 0; i < b.N; i++ {
			r := imap.NewReader(bufio.NewReader(strings.NewReader(line)))
			r.MaxAtomSize = 4096
			r.MaxResponseBytes = 1 << 20
			_, _ = imap.ReadResp(r)
		}
	})
}

// BenchmarkReadFetchEnvelope measures the cost of reading an ordinary
// FETCH envelope, which is the response the limits must not make
// unusable.
func BenchmarkReadFetchEnvelope(b *testing.B) {
	line := "* 1 FETCH (UID 7 FLAGS (\\Seen) INTERNALDATE \"12-Mar-2026 12:00:00 +0000\" RFC822.SIZE 4096 ENVELOPE (\"subject\" NIL NIL NIL NIL NIL NIL NIL NIL NIL))\r\n"
	b.SetBytes(int64(len(line)))
	for i := 0; i < b.N; i++ {
		r := imap.NewReader(bufio.NewReader(strings.NewReader(line)))
		if _, err := imap.ReadResp(r); err != nil {
			b.Fatal(err)
		}
	}
}
