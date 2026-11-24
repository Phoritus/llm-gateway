package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"llm-gateway/internal/promptlog"
	"llm-gateway/internal/store"
)

type loggingRoutes struct {
	fakeRoutes
	on bool
}

func (r loggingRoutes) ResolveRoute(ctx context.Context, workspaceID, alias string) (*store.ResolvedRoute, error) {
	route, err := r.fakeRoutes.ResolveRoute(ctx, workspaceID, alias)
	if route != nil {
		route.Settings.PromptLogging = r.on
	}
	return route, err
}

type fakePromptLog struct {
	mu   sync.Mutex
	logs []store.RequestLog
}

func (p *fakePromptLog) Record(l store.RequestLog) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.logs = append(p.logs, l)
}

func (p *fakePromptLog) recorded() []store.RequestLog {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]store.RequestLog(nil), p.logs...)
}

func (p *fakePromptLog) waitFor(t *testing.T, n int) []store.RequestLog {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(p.recorded()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("logged %d requests, want %d", len(p.recorded()), n)
		}
		time.Sleep(time.Millisecond)
	}
	return p.recorded()
}

// heldPromptLog takes no log until release is closed.
type heldPromptLog struct {
	release chan struct{}
	fakePromptLog
}

func (p *heldPromptLog) Record(l store.RequestLog) {
	<-p.release
	p.fakePromptLog.Record(l)
}

func TestTheClientHasItsWholeResponseBeforeThePromptIsLogged(t *testing.T) {
	for _, stream := range []bool{false, true} {
		logs := &heldPromptLog{release: make(chan struct{})}
		srv, _ := usageServer(t, func(d *Deps) {
			d.Routes = loggingRoutes{on: true}
			d.PromptLog = logs
		})
		read := make(chan error, 1)
		go func() {
			req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions",
				strings.NewReader(promptBody("mock/echo", stream)))
			req.Header.Set("Authorization", "Bearer "+testKey)
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				_, err = io.ReadAll(resp.Body)
				resp.Body.Close()
			}
			read <- err
		}()
		select {
		case err := <-read:
			if err != nil {
				t.Fatalf("stream=%v: %v", stream, err)
			}
		case <-time.After(5 * time.Second):
			close(logs.release)
			t.Fatalf("stream=%v: the response did not end while its log was held", stream)
		}
		close(logs.release)
		logs.waitFor(t, 1)
	}
}

const promptSecret = "sk-proj-abcdefghijklmnopqrstuvwxyz0123"

func promptBody(model string, stream bool) string {
	b, _ := json.Marshal(map[string]any{
		"model":    model,
		"stream":   stream,
		"messages": []map[string]string{{"role": "user", "content": "my key is " + promptSecret + " say hi"}},
	})
	return string(b)
}

func loggedRequest(t *testing.T, on bool, body string) (store.UsageEvent, []store.RequestLog) {
	t.Helper()
	logs := &fakePromptLog{}
	srv, rec := usageServer(t, func(d *Deps) {
		d.Routes = loggingRoutes{on: on}
		d.PromptLog = logs
	})
	drain(post(t, srv, body))
	e := recordedEvents(t, rec, 1)[0]
	if !on {
		return e, logs.recorded()
	}
	return e, logs.waitFor(t, 1)
}

func TestNoPromptIsLoggedUnlessTheWorkspaceTurnedLoggingOn(t *testing.T) {
	if _, logs := loggedRequest(t, false, promptBody("mock/echo", false)); len(logs) != 0 {
		t.Fatalf("logged %d requests with logging off", len(logs))
	}
}

func TestALoggedRequestKeepsItsBodyRedactedAndItsAnswer(t *testing.T) {
	for _, stream := range []bool{false, true} {
		e, logs := loggedRequest(t, true, promptBody("mock/echo", stream))
		if len(logs) != 1 {
			t.Fatalf("stream=%v: logged %d requests, want 1", stream, len(logs))
		}
		l := logs[0]
		if l.ID == "" || l.ID != e.ID {
			t.Errorf("stream=%v: log id %q, usage event id %q: a log must join its event", stream, l.ID, e.ID)
		}
		if l.WorkspaceID != "ws1" || l.RouteAlias != "mock/echo" || l.Status != 200 || l.Outcome != store.OutcomeOK {
			t.Errorf("stream=%v: log = %+v", stream, l)
		}
		if strings.Contains(l.Request, promptSecret) || !strings.Contains(l.Request, "[REDACTED]") {
			t.Errorf("stream=%v: the secret in the prompt was stored: %s", stream, l.Request)
		}
		if !strings.Contains(l.Request, `"model":"mock/echo"`) {
			t.Errorf("stream=%v: request = %s, want the body as sent", stream, l.Request)
		}
		var answer struct {
			Choices []struct {
				Message struct{ Content string } `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal([]byte(l.Response), &answer); err != nil || len(answer.Choices) != 1 ||
			!strings.Contains(answer.Choices[0].Message.Content, "say hi") {
			t.Errorf("stream=%v: response = %s, want the assembled answer", stream, l.Response)
		}
		if strings.Contains(l.Response, promptSecret) {
			t.Errorf("stream=%v: the echoed secret was stored in the response", stream)
		}
	}
}

func TestALoggedFailureKeepsWhatTheClientWasTold(t *testing.T) {
	_, logs := loggedRequest(t, true, promptBody("mock/fail", false))
	if len(logs) != 1 || logs[0].Status != 502 || !strings.Contains(logs[0].Response, "upstream_error") {
		t.Fatalf("logs = %+v, want one 502 with the error sent", logs)
	}
}

func TestALoggedBrokenStreamKeepsWhatArrived(t *testing.T) {
	_, logs := loggedRequest(t, true, promptBody("mock/break", true))
	if len(logs) != 1 {
		t.Fatalf("logged %d requests, want 1", len(logs))
	}
	l := logs[0]
	if l.Status != 200 || l.Outcome != store.OutcomeStreamError || !strings.Contains(l.Response, `"content"`) {
		t.Fatalf("log = %+v, want a 200 stream_error with the partial answer", l)
	}
}

func TestALongAnswerIsLoggedCut(t *testing.T) {
	long := strings.Repeat("word ", promptlog.MaxBodyBytes/2)
	body, _ := json.Marshal(map[string]any{
		"model":    "mock/echo",
		"messages": []map[string]string{{"role": "user", "content": long}},
	})
	_, logs := loggedRequest(t, true, string(body))
	l := logs[0]
	if !l.ResponseTruncated || len(l.Response) > promptlog.MaxBodyBytes {
		t.Fatalf("response truncated=%v len=%d, want it cut to %d", l.ResponseTruncated, len(l.Response), promptlog.MaxBodyBytes)
	}
}
