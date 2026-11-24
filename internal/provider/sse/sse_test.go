package sse

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"
)

func readAll(t *testing.T, r io.Reader) []Event {
	t.Helper()
	sr := NewReader(r)
	var out []Event
	for {
		ev, err := sr.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		out = append(out, ev)
	}
}

func TestReaderFraming(t *testing.T) {
	stream := ": OPENROUTER PROCESSING\n\n" +
		"data: {\"a\":1}\n\n" +
		"data:{\"b\":2}\r\n\r\n" +
		"event: message_start\ndata: one\ndata: two\nid: 7\nretry: 100\n\n" +
		"data: lone-cr\r\r" +
		"id: 8\n\n" +
		"data: [DONE]\n\n" +
		"data: cut-off-without-blank-line\n"
	want := []Event{
		{Data: []byte(`{"a":1}`)},
		{Data: []byte(`{"b":2}`)},
		{Name: "message_start", Data: []byte("one\ntwo")},
		{Data: []byte("lone-cr")},
		{Data: []byte("[DONE]")},
	}
	for name, r := range map[string]io.Reader{
		"whole":       strings.NewReader(stream),
		"byte-a-time": iotest.OneByteReader(strings.NewReader(stream)),
	} {
		got := readAll(t, r)
		if len(got) != len(want) {
			t.Fatalf("%s: %d events %q, want %d", name, len(got), got, len(want))
		}
		for i := range want {
			if got[i].Name != want[i].Name || string(got[i].Data) != string(want[i].Data) {
				t.Errorf("%s: event %d = {%q %q}, want {%q %q}", name, i, got[i].Name, got[i].Data, want[i].Name, want[i].Data)
			}
		}
	}
}

func TestReaderRefusesAnEndlessLine(t *testing.T) {
	_, err := NewReader(strings.NewReader("data: " + strings.Repeat("x", MaxLine+1))).Next()
	if !errors.Is(err, ErrLineTooLong) {
		t.Fatalf("err = %v, want ErrLineTooLong", err)
	}
}

func TestReaderPassesOnReadErrors(t *testing.T) {
	boom := errors.New("connection reset")
	_, err := NewReader(io.MultiReader(strings.NewReader("data: x\n"), iotest.ErrReader(boom))).Next()
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the read error", err)
	}
}

func TestAnEventThatNeverEndsIsRefusedAtMaxEvent(t *testing.T) {
	line := "data: " + strings.Repeat("x", MaxLine/2) + "\n"
	r := NewReader(strings.NewReader(strings.Repeat(line, 2*MaxEvent/MaxLine+2)))
	if _, err := r.Next(); !errors.Is(err, ErrEventTooLong) {
		t.Fatalf("err = %v, want ErrEventTooLong", err)
	}
}
