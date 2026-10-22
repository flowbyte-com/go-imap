package imap

import (
	"strings"
)

// Resp is an IMAP response. It is either a *DataResp, a
// *ContinuationReq or a *StatusResp.
type Resp interface {
	resp()
}

// ReadResp reads a single response from a Reader.
//
// The reader's per-response accounting is reset here, so the limits
// apply to each response separately and a sequence of responses is
// bounded response by response.
func ReadResp(r *Reader) (Resp, error) {
	r.beginResponse()

	atom, err := r.ReadAtom()
	if err != nil {
		return nil, err
	}
	tag, ok := atom.(string)
	if !ok {
		return nil, newParseError("response tag is not an atom")
	}

	if tag == "+" {
		if err := r.ReadSp(); err != nil {
			r.UnreadRune()
		}

		resp := &ContinuationReq{}
		resp.Info, err = r.ReadInfo()
		if err != nil {
			return nil, err
		}

		return resp, nil
	}

	if err := r.ReadSp(); err != nil {
		return nil, err
	}

	// Can be either data or status
	// Try to parse a status
	var fields []interface{}
	if atom, err := r.ReadAtom(); err == nil {
		fields = append(fields, atom)

		if err := r.ReadSp(); err == nil {
			if name, ok := atom.(string); ok {
				status := StatusRespType(name)
				switch status {
				case StatusRespOk, StatusRespNo, StatusRespBad, StatusRespPreauth, StatusRespBye:
					resp := &StatusResp{
						Tag:  tag,
						Type: status,
					}

					char, _, err := r.ReadRune()
					if err != nil {
						return nil, err
					}
					r.UnreadRune()

					if char == '[' {
						// Contains code & arguments
						resp.Code, resp.Arguments, err = r.ReadRespCode()
						if err != nil {
							return nil, err
						}
					}

					resp.Info, err = r.ReadInfo()
					if err != nil {
						return nil, err
					}

					return resp, nil
				}
			}
		} else if isLimitError(err) {
			// A decoder-limit violation is fatal. The space was
			// consumed and the response stream is no longer aligned
			// with the start of the response, so falling back to the
			// data path would parse the remainder of the response as
			// if it were a field. The error is returned as it is.
			return nil, err
		} else {
			r.UnreadRune()
		}
	} else if isLimitError(err) {
		// A decoder-limit violation is fatal. The atom was consumed
		// only in part, so the response stream is no longer aligned
		// with the start of the response: falling back to the data
		// path would parse the remainder of the oversized atom as if
		// it were a field, and the response would be accepted. The
		// error is returned as it is.
		//
		// A generic parse error is not fatal in the same way: the
		// upstream fallback re-reads the same bytes and reports the
		// same error, and it is load-bearing for a data response whose
		// name is a list.
		return nil, err
	} else {
		r.UnreadRune()
	}

	// Not a status so it's data
	resp := &DataResp{Tag: tag}

	// The name of the response is its first field, and it is counted
	// against the field budget like every other field.
	if err := r.countField(); err != nil {
		return nil, err
	}

	var remaining []interface{}
	remaining, err = r.ReadLine()
	if err != nil {
		return nil, err
	}

	resp.Fields = append(fields, remaining...)
	return resp, nil
}

// DataResp is an IMAP response containing data.
type DataResp struct {
	// The response tag. Can be either "" for untagged responses, "+" for continuation
	// requests or a previous command's tag.
	Tag string
	// The parsed response fields.
	Fields []interface{}
}

// NewUntaggedResp creates a new untagged response.
func NewUntaggedResp(fields []interface{}) *DataResp {
	return &DataResp{
		Tag:    "*",
		Fields: fields,
	}
}

func (r *DataResp) resp() {}

func (r *DataResp) WriteTo(w *Writer) error {
	tag := RawString(r.Tag)
	if tag == "" {
		tag = RawString("*")
	}

	fields := []interface{}{RawString(tag)}
	fields = append(fields, r.Fields...)
	return w.writeLine(fields...)
}

// ContinuationReq is a continuation request response.
type ContinuationReq struct {
	// The info message sent with the continuation request.
	Info string
}

func (r *ContinuationReq) resp() {}

func (r *ContinuationReq) WriteTo(w *Writer) error {
	if err := w.writeString("+"); err != nil {
		return err
	}

	if r.Info != "" {
		if err := w.writeString(string(sp) + r.Info); err != nil {
			return err
		}
	}

	return w.writeCrlf()
}

// ParseNamedResp attempts to parse a named data response.
func ParseNamedResp(resp Resp) (name string, fields []interface{}, ok bool) {
	data, ok := resp.(*DataResp)
	if !ok || len(data.Fields) == 0 {
		return
	}

	// Some responses (namely EXISTS and RECENT) are formatted like so:
	//   [num] [name] [...]
	// Which is fucking stupid. But we handle that here by checking if the
	// response name is a number and then rearranging it.
	if len(data.Fields) > 1 {
		name, ok := data.Fields[1].(string)
		if ok {
			if _, err := ParseNumber(data.Fields[0]); err == nil {
				fields := []interface{}{data.Fields[0]}
				fields = append(fields, data.Fields[2:]...)
				return strings.ToUpper(name), fields, true
			}
		}
	}

	// IMAP commands are formatted like this:
	//   [name] [...]
	name, ok = data.Fields[0].(string)
	if !ok {
		return
	}
	return strings.ToUpper(name), data.Fields[1:], true
}
