package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type decodeTarget struct {
	Email  string `json:"email"`
	Count  int    `json:"count"`
	Note   string `json:"-"`
	secret string
}

func decode(t *testing.T, body string) (*httptest.ResponseRecorder, decodeTarget, bool) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	var dst decodeTarget
	return w, dst, decodeJSON(w, r, &dst)
}

func errorCodeOf(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var envelope errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &envelope); err != nil {
		t.Fatalf("response is not an error envelope: %v\n%s", err, w.Body.String())
	}
	return envelope.Error.Code
}

// A field the endpoint has no place for is refused, which turns a client's typo
// into a visible error rather than a setting that was quietly ignored.
//
// What this really pins is that the refusal is this package's own judgement. It
// used to be a substring match on an encoding/json sentence, and the sentence
// itself was handed back to the caller -- the one place an internal error
// message reached a response.
func TestDecodeJSONRefusesAnUnknownField(t *testing.T) {
	w, _, ok := decode(t, `{"email":"a@b.com","admin":true}`)
	if ok {
		t.Fatal("decodeJSON accepted a body with an unknown field")
	}
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
	if code := errorCodeOf(t, w); code != "unknown_field" {
		t.Errorf("error code = %q, want unknown_field", code)
	}
	if body := w.Body.String(); !strings.Contains(body, `\"admin\"`) {
		t.Errorf("the response does not name the offending field:\n%s", body)
	}
	// Nothing from the standard library's wording is quoted back.
	if body := w.Body.String(); strings.Contains(body, "json:") {
		t.Errorf("the response quotes an internal error message:\n%s", body)
	}
}

// A field encoding/json cannot see is not a field the endpoint accepts, so
// naming it is refused rather than dropped in silence. That covers the Go name
// of a field tagged json:"-", and the literal key "-" that such a tag would
// otherwise register as legitimate.
func TestDecodeJSONRefusesAFieldJSONCannotSee(t *testing.T) {
	for _, body := range []string{
		`{"email":"a@b.com","Note":"x"}`,
		`{"email":"a@b.com","-":"x"}`,
		`{"email":"a@b.com","secret":"x"}`,
	} {
		t.Run(body, func(t *testing.T) {
			w, _, ok := decode(t, body)
			if ok {
				t.Fatalf("decodeJSON accepted %s", body)
			}
			if code := errorCodeOf(t, w); code != "unknown_field" {
				t.Errorf("error code = %q, want unknown_field", code)
			}
		})
	}
}

// encoding/json matches a field name case-insensitively, and this check
// replaces how an unknown field is reported, not which names are known. A
// client already sending {"Email": ...} must keep working.
func TestDecodeJSONKeepsCaseInsensitiveFieldMatching(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/auth/login",
		strings.NewReader(`{"EMAIL":"a@b.com","Count":3}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	var dst decodeTarget
	if !decodeJSON(w, r, &dst) {
		t.Fatalf("decodeJSON refused a body spelling known fields differently: %s", w.Body.String())
	}
	if dst.Email != "a@b.com" || dst.Count != 3 {
		t.Errorf("decoded %+v, want {a@b.com 3}", dst)
	}
}

// Several unknown fields name the first in sorted order, so one body always
// gets one answer. Go's map iteration order would otherwise make the same
// request answer differently from call to call.
func TestDecodeJSONNamesTheSameFieldEveryTime(t *testing.T) {
	for range 20 {
		w, _, ok := decode(t, `{"zebra":1,"admin":2,"mango":3}`)
		if ok {
			t.Fatal("decodeJSON accepted a body with unknown fields")
		}
		if body := w.Body.String(); !strings.Contains(body, `\"admin\"`) {
			t.Fatalf("the response does not name the first field in sorted order:\n%s", body)
		}
	}
}

func TestDecodeJSONAcceptsAKnownShape(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/auth/login",
		strings.NewReader(`{"email":"a@b.com","count":3}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	var dst decodeTarget
	if !decodeJSON(w, r, &dst) {
		t.Fatalf("decodeJSON refused a valid body: %s", w.Body.String())
	}
	if dst.Email != "a@b.com" || dst.Count != 3 {
		t.Errorf("decoded %+v, want {a@b.com 3}", dst)
	}
}

func TestDecodeJSONClassifiesBadBodies(t *testing.T) {
	cases := []struct {
		name string
		body string
		code string
	}{
		{"empty", "", "missing_body"},
		{"whitespace only", "   \n", "missing_body"},
		{"truncated", `{"email":`, "invalid_json"},
		{"wrong type for the field", `{"count":"three"}`, "invalid_json"},
		{"an array, not an object", `[{"email":"a@b.com"}]`, "invalid_json"},
		// Two documents in one body is not one request.
		{"two documents", `{"email":"a@b.com"} {"email":"c@d.com"}`, "invalid_json"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, _, ok := decode(t, tc.body)
			if ok {
				t.Fatalf("decodeJSON accepted %q", tc.body)
			}
			if code := errorCodeOf(t, w); code != tc.code {
				t.Errorf("error code = %q, want %q", code, tc.code)
			}
		})
	}
}

// A body over the cap is answered as oversized rather than as malformed, which
// is the difference between a client that must send less and a client that must
// send something else.
func TestDecodeJSONRefusesAnOversizedBody(t *testing.T) {
	w, _, ok := decode(t, `{"email":"`+strings.Repeat("x", maxBodyBytes)+`"}`)
	if ok {
		t.Fatal("decodeJSON accepted a body over the cap")
	}
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413", w.Code)
	}
	if code := errorCodeOf(t, w); code != "body_too_large" {
		t.Errorf("error code = %q, want body_too_large", code)
	}
}

// A target that is not a pointer to a struct has no known fields, so every key
// is unknown. A wiring mistake inside a validator must not fail open.
func TestUnknownFieldsFailsClosedForANonStructTarget(t *testing.T) {
	fields := map[string]json.RawMessage{"email": json.RawMessage(`"a@b.com"`)}
	var notAStruct map[string]any
	if got := unknownFields(&notAStruct, fields); len(got) != 1 || got[0] != "email" {
		t.Errorf("unknownFields = %v, want every key reported", got)
	}
	if got := unknownFields(nil, fields); len(got) != 1 {
		t.Errorf("unknownFields(nil) = %v, want every key reported", got)
	}
}
