package imap

import (
	"bytes"
	"errors"
	"io"
	"strconv"
	"strings"
)

const (
	sp            = ' '
	cr            = '\r'
	lf            = '\n'
	dquote        = '"'
	literalStart  = '{'
	literalEnd    = '}'
	listStart     = '('
	listEnd       = ')'
	respCodeStart = '['
	respCodeEnd   = ']'
)

const (
	crlf    = "\r\n"
	nilAtom = "NIL"
)

// TODO: add CTL to atomSpecials
var (
	quotedSpecials = string([]rune{dquote, '\\'})
	respSpecials   = string([]rune{respCodeEnd})
	atomSpecials   = string([]rune{listStart, listEnd, literalStart, sp, '%', '*'}) + quotedSpecials + respSpecials
)

type parseError struct {
	error
}

func newParseError(text string) error {
	return &parseError{errors.New(text)}
}

// IsParseError returns true if the provided error is a parse error produced by
// Reader. It matches both the original *parseError type and the
// literalSizeExceededError value (added by TheMailroom's fork
// for protocol-level IMAP literal-size enforcement).
func IsParseError(err error) bool {
	if _, ok := err.(*parseError); ok {
		return true
	}
	// literalSizeExceededError is a value type (not a
	// pointer) but implements the parse-error contract for
	// the executor's handle loop.
	if _, ok := err.(literalSizeExceededError); ok {
		return true
	}
	// A decoder-limit violation is fatal in the same way: the response
	// stream is abandoned and the connection is closed.
	if _, ok := err.(responseTooLargeError); ok {
		return true
	}
	return false
}

// ErrLiteralSizeExceeded is returned by ReadLiteral when the
// announced literal size exceeds Reader.MaxLiteralSize. It is
// a *parseError (so imap.IsParseError returns true) AND an
// exported sentinel (so callers can errors.Is it). The
// allocation that would follow the size check is skipped
// entirely, so a 1 GiB announcement is rejected before any
// 1 GiB buffer is allocated.
//
// This error type is added by TheMailroom's fork
// (github.com/flowbyte-com/go-imap) to satisfy the pre-production
// security requirement in
// github.com/flowbyte-com/themailroom/docs/PHASE2_DESIGN.md §10.
// The fork is a temporary measure; the change is intended to
// be upstreamed. See the fork's CHANGES.md for the full change
// set against upstream.
var ErrLiteralSizeExceeded = errors.New("imap: literal exceeding maximum size")

// ErrResponseTooLarge is returned when a response exceeds one of the
// reader's structural limits: MaxResponseFields, MaxAtomSize,
// MaxQuotedStringSize, MaxResponseBytes or MaxListDepth.
//
// The error is a *parseError (so imap.IsParseError returns true) AND
// an exported sentinel (so callers can errors.Is it). The executor
// therefore treats it as fatal: it aborts the parse, closes the
// connection and surfaces the cause through the connection-closed
// error, so a desynchronised connection is never reused.
//
// The message never contains any content from the response, so a
// hostile server cannot inject text into a caller's diagnostics.
//
// This error type is added by TheMailroom's fork
// (github.com/flowbyte-com/go-imap) to bound the memory a single
// response can make the reader allocate. See the fork's CHANGES.md.
var ErrResponseTooLarge = errors.New("imap: response exceeding maximum size")

// responseTooLargeError is a decoder-limit violation. The reason is
// one of a fixed set of strings; it never contains response content.
type responseTooLargeError struct{ reason string }

func (e responseTooLargeError) Error() string {
	if e.reason == "" {
		return ErrResponseTooLarge.Error()
	}
	return ErrResponseTooLarge.Error() + ": " + e.reason
}
func (e responseTooLargeError) Is(target error) bool { return target == ErrResponseTooLarge }

// literalSizeExceededError wraps ErrLiteralSizeExceeded so it
// is both a *parseError (preserving the existing
// imap.IsParseError detection in the executor's handle
// loop) and detectable via errors.Is(ErrLiteralSizeExceeded).
type literalSizeExceededError struct{}

func (literalSizeExceededError) Error() string { return ErrLiteralSizeExceeded.Error() }
func (literalSizeExceededError) Is(target error) bool {
	return target == ErrLiteralSizeExceeded
}

// A string reader.
type StringReader interface {
	// ReadString reads until the first occurrence of delim in the input,
	// returning a string containing the data up to and including the delimiter.
	// See https://golang.org/pkg/bufio/#Reader.ReadString
	ReadString(delim byte) (line string, err error)
}

type reader interface {
	io.Reader
	io.RuneScanner
	StringReader
}

// ParseNumber parses a number.
func ParseNumber(f interface{}) (uint32, error) {
	// Useful for tests
	if n, ok := f.(uint32); ok {
		return n, nil
	}

	var s string
	switch f := f.(type) {
	case RawString:
		s = string(f)
	case string:
		s = f
	default:
		return 0, newParseError("expected a number, got a non-atom")
	}

	nbr, err := strconv.ParseUint(string(s), 10, 32)
	if err != nil {
		return 0, &parseError{err}
	}

	return uint32(nbr), nil
}

// ParseString parses a string, which is either a literal, a quoted string or an
// atom.
func ParseString(f interface{}) (string, error) {
	if s, ok := f.(string); ok {
		return s, nil
	}

	// Useful for tests
	if a, ok := f.(RawString); ok {
		return string(a), nil
	}

	if l, ok := f.(Literal); ok {
		b := make([]byte, l.Len())
		if _, err := io.ReadFull(l, b); err != nil {
			return "", err
		}
		return string(b), nil
	}

	return "", newParseError("expected a string")
}

// Convert a field list to a string list.
func ParseStringList(f interface{}) ([]string, error) {
	fields, ok := f.([]interface{})
	if !ok {
		return nil, newParseError("expected a string list, got a non-list")
	}

	list := make([]string, len(fields))
	for i, f := range fields {
		var err error
		if list[i], err = ParseString(f); err != nil {
			return nil, newParseError("cannot parse string in string list: " + err.Error())
		}
	}
	return list, nil
}

func trimSuffix(str string, suffix rune) string {
	return str[:len(str)-1]
}

// An IMAP reader.
//
// Every limit is zero by default, which means "no limit". The limits
// are set once, before the reader is used, and are never changed
// afterwards: the reader is read by a single goroutine, so a limit
// that could be changed at run time would be a data race.
//
// The limits are applied by the client before the background reader
// goroutine starts (see client.DialOptions and client.NewWithOptions),
// so they are in effect for the very first byte read from the
// connection.
type Reader struct {
	MaxLiteralSize uint32 // The maximum literal size.

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

	reader

	continues chan<- bool

	brackets   int
	inRespCode bool

	// The accounting for the response being parsed. It is reset at the
	// start of every response.
	respFields uint32
	respBytes  uint64
	listDepth  uint32
	// lastRuneSize is the size in bytes of the rune the reader last
	// returned. It is refunded when the rune is unread, so that the
	// parser's lookahead never counts a byte twice.
	lastRuneSize int
}

// beginResponse resets the per-response accounting. It is called at
// the start of every response.
func (r *Reader) beginResponse() {
	r.respFields = 0
	r.respBytes = 0
	r.listDepth = 0
	r.lastRuneSize = 0
}

// countField accounts one field against the current response's field
// budget.
//
// The counter spans every nesting level, because ReadList calls back
// into ReadFields. The name of a data response is counted as well, so
// the budget covers every field the response materialises.
func (r *Reader) countField() error {
	r.respFields++
	if r.MaxResponseFields > 0 && r.respFields > r.MaxResponseFields {
		return r.limitError("response exceeds the maximum number of fields")
	}
	return nil
}

// limitError returns a decoder-limit violation. The reason is one of a
// fixed set of strings and never contains response content.
func (r *Reader) limitError(reason string) error {
	return responseTooLargeError{reason: reason}
}

// charge accounts n bytes against the current response's byte budget.
//
// The arithmetic is overflow-safe: the limit is compared against the
// remaining allowance before anything is added, and the counter is
// only advanced once the total is known to fit.
func (r *Reader) charge(n int) error {
	if n <= 0 {
		return nil
	}
	if r.MaxResponseBytes == 0 {
		r.respBytes += uint64(n)
		return nil
	}
	limit := uint64(r.MaxResponseBytes)
	if r.respBytes > limit || uint64(n) > limit-r.respBytes {
		return r.limitError("response exceeds the maximum response size")
	}
	r.respBytes += uint64(n)
	return nil
}

// reserve checks that n more bytes fit in the current response's byte
// budget, without accounting them. It is used before an allocation
// whose size is known in advance, so that the allocation is refused
// before it is made.
func (r *Reader) reserve(n int) error {
	if n <= 0 || r.MaxResponseBytes == 0 {
		return nil
	}
	limit := uint64(r.MaxResponseBytes)
	if r.respBytes > limit || uint64(n) > limit-r.respBytes {
		return r.limitError("response exceeds the maximum response size")
	}
	return nil
}

// ReadRune reads the next rune and accounts its byte size against the
// current response's budget.
//
// It shadows the promoted method of the embedded reader, so that every
// byte the parser consumes is accounted exactly once, whatever path
// consumed it: the response tag, the status or continuation text, the
// literal length declarations, the atoms, the quoted strings and the
// delimiters between them.
func (r *Reader) ReadRune() (rune, int, error) {
	char, size, err := r.reader.ReadRune()
	if err != nil {
		return char, size, err
	}
	if err := r.charge(size); err != nil {
		return 0, 0, err
	}
	r.lastRuneSize = size
	return char, size, nil
}

// UnreadRune pushes the last rune back and refunds the bytes it was
// charged, so that the parser's lookahead never counts a byte twice.
func (r *Reader) UnreadRune() error {
	if err := r.reader.UnreadRune(); err != nil {
		return err
	}
	if r.lastRuneSize > 0 {
		r.respBytes -= uint64(r.lastRuneSize)
		r.lastRuneSize = 0
	}
	return nil
}

func (r *Reader) ReadSp() error {
	char, _, err := r.ReadRune()
	if err != nil {
		return err
	}
	if char != sp {
		return newParseError("expected a space")
	}
	return nil
}

func (r *Reader) ReadCrlf() (err error) {
	var char rune

	if char, _, err = r.ReadRune(); err != nil {
		return
	}
	if char == lf {
		return
	}
	if char != cr {
		err = newParseError("line doesn't end with a CR")
		return
	}

	if char, _, err = r.ReadRune(); err != nil {
		return
	}
	if char != lf {
		err = newParseError("line doesn't end with a LF")
	}

	return
}

func (r *Reader) ReadAtom() (interface{}, error) {
	r.brackets = 0

	// The atom is built with a strings.Builder, so the work and the
	// allocation are linear in its length. The previous string
	// concatenation grew the atom one character at a time, which made
	// the cost of a long atom quadratic.
	var atom strings.Builder
	for {
		char, size, err := r.ReadRune()
		if err != nil {
			return nil, err
		}

		// TODO: list-wildcards and \
		if r.brackets == 0 && (char == listStart || char == literalStart || char == dquote) {
			return nil, newParseError("atom contains forbidden char: " + string(char))
		}
		if char == cr || char == lf {
			break
		}
		if r.brackets == 0 && (char == sp || char == listEnd) {
			break
		}
		if char == respCodeEnd {
			if r.brackets == 0 {
				if r.inRespCode {
					break
				} else {
					return nil, newParseError("atom contains bad brackets nesting")
				}
			}
			r.brackets--
		}
		if char == respCodeStart {
			r.brackets++
		}

		// The size limit is checked before the rune is appended, so the
		// atom never grows past it.
		if r.MaxAtomSize > 0 && uint64(atom.Len())+uint64(size) > uint64(r.MaxAtomSize) {
			return nil, r.limitError("atom exceeds the maximum atom size")
		}
		atom.WriteRune(char)
	}

	r.UnreadRune()

	if atom.Len() == 0 {
		return nil, nil
	}
	if atom.String() == nilAtom {
		return nil, nil
	}

	return atom.String(), nil
}

func (r *Reader) ReadLiteral() (Literal, error) {
	char, _, err := r.ReadRune()
	if err != nil {
		return nil, err
	} else if char != literalStart {
		return nil, newParseError("literal string doesn't start with an open brace")
	}

	// The length declaration is read rune by rune rather than with a
	// single ReadString, so that it is bounded by the response's byte
	// budget while it is read, and so that a declaration that is never
	// terminated cannot be accumulated without limit.
	var decl strings.Builder
	for {
		char, _, err := r.ReadRune()
		if err != nil {
			return nil, err
		}
		if char == literalEnd {
			break
		}
		if decl.Len() >= maxLiteralLengthDeclaration {
			return nil, r.limitError("literal length declaration exceeds the maximum size")
		}
		decl.WriteRune(char)
	}
	lstr := decl.String()

	nonSync := strings.HasSuffix(lstr, "+")
	if nonSync {
		lstr = trimSuffix(lstr, '+')
	}

	n, err := strconv.ParseUint(lstr, 10, 32)
	if err != nil {
		return nil, newParseError("cannot parse literal length: " + err.Error())
	}
	if r.MaxLiteralSize > 0 && uint32(n) > r.MaxLiteralSize {
		return nil, literalSizeExceededError{}
	}
	// The announced length must fit in an int before it is used to
	// size an allocation, so that the allocation cannot overflow.
	if n > uint64(maxInt) {
		return nil, r.limitError("literal length exceeds the maximum literal size")
	}

	if err := r.ReadCrlf(); err != nil {
		return nil, err
	}

	// Send continuation request if necessary
	if r.continues != nil && !nonSync {
		r.continues <- true
	}

	// The body is charged against the response's byte budget before it
	// is allocated, so that a literal that would overrun the budget is
	// refused before the buffer exists.
	if err := r.charge(int(n)); err != nil {
		return nil, err
	}

	// Read literal
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return nil, err
	}
	return bytes.NewBuffer(b), nil
}

// maxLiteralLengthDeclaration is the maximum number of characters in a
// literal length declaration. A length is a decimal number of at most
// ten digits, optionally followed by a '+' for a non-synchronising
// literal.
const maxLiteralLengthDeclaration = 16

// maxInt is the largest value an int can hold on this platform. It is
// used to keep a length that comes from the wire from overflowing an
// int before it sizes an allocation.
const maxInt = int(^uint(0) >> 1)

func (r *Reader) ReadQuotedString() (string, error) {
	if char, _, err := r.ReadRune(); err != nil {
		return "", err
	} else if char != dquote {
		return "", newParseError("quoted string doesn't start with a double quote")
	}

	var buf bytes.Buffer
	var escaped bool
	for {
		char, _, err := r.ReadRune()
		if err != nil {
			return "", err
		}

		if char == '\\' && !escaped {
			escaped = true
		} else {
			if char == cr || char == lf {
				r.UnreadRune()
				return "", newParseError("CR or LF not allowed in quoted string")
			}
			if char == dquote && !escaped {
				break
			}

			if !strings.ContainsRune(quotedSpecials, char) && escaped {
				return "", newParseError("quoted string cannot contain backslash followed by a non-quoted-specials char")
			}

			// The size limit is checked before the rune is appended, so
			// the quoted string never grows past it.
			if r.MaxQuotedStringSize > 0 && uint64(buf.Len())+1 > uint64(r.MaxQuotedStringSize) {
				return "", r.limitError("quoted string exceeds the maximum quoted string size")
			}
			buf.WriteRune(char)
			escaped = false
		}
	}

	return buf.String(), nil
}

func (r *Reader) ReadFields() (fields []interface{}, err error) {
	var char rune
	for {
		if char, _, err = r.ReadRune(); err != nil {
			return
		}
		if err = r.UnreadRune(); err != nil {
			return
		}

		var field interface{}
		ok := true
		switch char {
		case literalStart:
			field, err = r.ReadLiteral()
		case dquote:
			field, err = r.ReadQuotedString()
		case listStart:
			field, err = r.ReadList()
		case listEnd:
			ok = false
		case cr:
			return
		default:
			field, err = r.ReadAtom()
		}

		if err != nil {
			return
		}
		if ok {
			if err := r.countField(); err != nil {
				return nil, err
			}
			fields = append(fields, field)
		}

		if char, _, err = r.ReadRune(); err != nil {
			return
		}
		if char == cr || char == lf || char == listEnd || char == respCodeEnd {
			if char == cr || char == lf {
				r.UnreadRune()
			}
			return
		}
		if char == listStart {
			r.UnreadRune()
			continue
		}
		if char != sp {
			err = newParseError("fields are not separated by a space")
			return
		}
	}
}

func (r *Reader) ReadList() (fields []interface{}, err error) {
	char, _, err := r.ReadRune()
	if err != nil {
		return
	}
	if char != listStart {
		err = newParseError("list doesn't start with an open parenthesis")
		return
	}

	// The depth is checked before the nested list is parsed, so a
	// deeply nested response is refused before the recursion can
	// exhaust the goroutine stack.
	r.listDepth++
	if r.MaxListDepth > 0 && r.listDepth > r.MaxListDepth {
		r.listDepth--
		err = r.limitError("response exceeds the maximum list nesting depth")
		return
	}
	defer func() { r.listDepth-- }()

	fields, err = r.ReadFields()
	if err != nil {
		return
	}

	r.UnreadRune()
	if char, _, err = r.ReadRune(); err != nil {
		return
	}
	if char != listEnd {
		err = newParseError("list doesn't end with a close parenthesis")
	}
	return
}

func (r *Reader) ReadLine() (fields []interface{}, err error) {
	fields, err = r.ReadFields()
	if err != nil {
		return
	}

	r.UnreadRune()
	err = r.ReadCrlf()
	return
}

func (r *Reader) ReadRespCode() (code StatusRespCode, fields []interface{}, err error) {
	char, _, err := r.ReadRune()
	if err != nil {
		return
	}
	if char != respCodeStart {
		err = newParseError("response code doesn't start with an open bracket")
		return
	}

	r.inRespCode = true
	fields, err = r.ReadFields()
	r.inRespCode = false
	if err != nil {
		return
	}

	if len(fields) == 0 {
		err = newParseError("response code doesn't contain any field")
		return
	}

	codeStr, ok := fields[0].(string)
	if !ok {
		err = newParseError("response code doesn't start with a string atom")
		return
	}
	if codeStr == "" {
		err = newParseError("response code is empty")
		return
	}
	code = StatusRespCode(strings.ToUpper(codeStr))

	fields = fields[1:]

	r.UnreadRune()
	char, _, err = r.ReadRune()
	if err != nil {
		return
	}
	if char != respCodeEnd {
		err = newParseError("response code doesn't end with a close bracket")
	}
	return
}

// ReadInfo reads the rest of the line: the status text of a status
// response or the text of a continuation request.
//
// The text is read rune by rune rather than with a single ReadString,
// so that the response's byte budget applies to it while it is read.
// A ReadString would first accumulate the whole line and only then
// could its size be checked, which is exactly the allocation the
// budget is meant to prevent.
func (r *Reader) ReadInfo() (info string, err error) {
	var b strings.Builder
	for {
		char, _, err := r.ReadRune()
		if err != nil {
			return "", err
		}
		if char == lf {
			break
		}
		b.WriteRune(char)
	}
	info = b.String()
	info = strings.TrimSuffix(info, string(cr))
	info = strings.TrimLeft(info, " ")

	return
}

func NewReader(r reader) *Reader {
	return &Reader{reader: r}
}

func NewServerReader(r reader, continues chan<- bool) *Reader {
	return &Reader{reader: r, continues: continues}
}

type Parser interface {
	Parse(fields []interface{}) error
}
