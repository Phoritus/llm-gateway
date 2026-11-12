// Package sse reads a server-sent event stream, the framing every streaming
// upstream answers in.
package sse

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

// MaxLine bounds one line.
const MaxLine = 1 << 20

// ErrLineTooLong is a line longer than MaxLine.
var ErrLineTooLong = errors.New("sse: line too long")

// Event is one dispatched event. Data is its data lines joined by "\n".
type Event struct {
	Name string
	Data []byte
}

type Reader struct {
	r *bufio.Reader
}

func NewReader(r io.Reader) *Reader {
	return &Reader{r: bufio.NewReaderSize(r, 4096)}
}

// Next returns the next event that carries data. Comments (lines starting
// with a colon, which some upstreams send as keep-alives), id and retry
// fields, and events with no data are skipped. At the end of the stream it
// returns io.EOF; an event the stream ended in the middle of is discarded,
// as the SSE specification says, because its data may be cut short.
func (s *Reader) Next() (Event, error) {
	var ev Event
	hasData := false
	for {
		line, err := s.line()
		if err != nil {
			return Event{}, err
		}
		if len(line) == 0 {
			if hasData {
				return ev, nil
			}
			ev = Event{}
			continue
		}
		if line[0] == ':' {
			continue
		}
		field, value, _ := bytes.Cut(line, []byte(":"))
		value = bytes.TrimPrefix(value, []byte(" "))
		switch string(field) {
		case "data":
			if hasData {
				ev.Data = append(ev.Data, '\n')
			}
			ev.Data = append(ev.Data, value...)
			hasData = true
		case "event":
			ev.Name = string(value)
		}
	}
}

// line reads one line without its terminator, which may be \n, \r\n or a lone
// \r. A last line with no terminator is incomplete and is dropped with io.EOF.
func (s *Reader) line() ([]byte, error) {
	var out []byte
	for {
		b, err := s.r.ReadByte()
		if err != nil {
			return nil, err
		}
		switch b {
		case '\n':
			return out, nil
		case '\r':
			if next, err := s.r.Peek(1); err == nil && next[0] == '\n' {
				_, _ = s.r.ReadByte()
			}
			return out, nil
		}
		if len(out) >= MaxLine {
			return nil, ErrLineTooLong
		}
		out = append(out, b)
	}
}
