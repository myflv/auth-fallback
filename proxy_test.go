package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	deepseek = "deepseek/deepseek-chat"
	claude   = "anthropic/claude-sonnet-4-6"
)

type call struct{ key, model string }

type rec struct {
	mu   sync.Mutex
	seen []call
}

func (r *rec) add(key, model string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, call{key, model})
}

func (r *rec) calls() []call {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]call(nil), r.seen...)
}

func (r *rec) keys() []string {
	var out []string
	for _, c := range r.calls() {
		out = append(out, c.key)
	}
	return out
}

func upstream(t *testing.T, r *rec, h func(w http.ResponseWriter, key, model string)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		key := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		body, _ := io.ReadAll(req.Body)
		var probe struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &probe)
		r.add(key, probe.Model)
		h(w, key, probe.Model)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func ok(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"data":{"id":"gen_1","object":"chat.completion"},"success":true}`)
}

func limited(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusTooManyRequests)
	_, _ = io.WriteString(w, `{"error":"rate limited","success":false}`)
}

func newProxy(t *testing.T, up *httptest.Server, keys ...string) *httptest.Server {
	t.Helper()
	srv, _ := newProxyWith(t, up, 3, keys...)
	return srv
}

func newProxyWith(t *testing.T, up *httptest.Server, maxAttempts int, keys ...string) (*httptest.Server, *proxy) {
	t.Helper()
	base, err := url.Parse(up.URL + "/api/v1")
	if err != nil {
		t.Fatal(err)
	}
	p := &proxy{
		base:        base,
		client:      newHTTPClient(),
		pool:        newPool(keys, time.Minute, 30*time.Minute),
		maxAttempts: maxAttempts,
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	return srv, p
}

func chatBody(model string, stream bool) string {
	b, _ := json.Marshal(map[string]any{
		"model":    model,
		"messages": []map[string]string{{"role": "user", "content": "hi"}},
		"stream":   stream,
	})
	return string(b)
}

func post(t *testing.T, url, token, body string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, out
}

func TestRetriesOnTheNextKeyAfter429(t *testing.T) {
	r := &rec{}
	up := upstream(t, r, func(w http.ResponseWriter, key, _ string) {
		if key == "k0" {
			limited(w)
			return
		}
		ok(w)
	})
	p := newProxy(t, up, "k0", "k1")

	resp, out := post(t, p.URL+"/v1/chat/completions", "", chatBody(deepseek, false))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200: the caller should never see the 429\n%s", resp.StatusCode, out)
	}
	if got := r.keys(); !slices.Equal(got, []string{"k0", "k1"}) {
		t.Errorf("upstream saw %v, want [k0 k1]", got)
	}
}

// A 429 arrives before any bytes are written, so a streaming request can still
// move to the next key. Getting this wrong would mean emitting a truncated SSE
// body to the caller.
func TestStreamingRequestsAreRetriedToo(t *testing.T) {
	r := &rec{}
	up := upstream(t, r, func(w http.ResponseWriter, key, _ string) {
		if key == "k0" {
			limited(w)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})
	p := newProxy(t, up, "k0", "k1")

	resp, out := post(t, p.URL+"/v1/chat/completions", "", chatBody(deepseek, true))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", resp.StatusCode, out)
	}
	if !strings.Contains(string(out), "[DONE]") {
		t.Errorf("stream = %q", out)
	}
}

// The headline behaviour: deepseek running out must not touch anything else.
func TestExhaustingOneModelLeavesTheOthersAlone(t *testing.T) {
	r := &rec{}
	up := upstream(t, r, func(w http.ResponseWriter, _, model string) {
		if model == deepseek {
			limited(w)
			return
		}
		ok(w)
	})
	p := newProxy(t, up, "k0", "k1")

	resp, _ := post(t, p.URL+"/v1/chat/completions", "", chatBody(deepseek, false))
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 once the whole pool is spent", resp.StatusCode)
	}

	before := len(r.calls())
	resp, out := post(t, p.URL+"/v1/chat/completions", "", chatBody(claude, false))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the other model got %d: %s", resp.StatusCode, out)
	}
	if got := len(r.calls()) - before; got != 1 {
		t.Errorf("the other model took %d attempts, want 1: deepseek's cooldown leaked", got)
	}
}

func TestAllKeysLimitedGivesUpWithRetryAfter(t *testing.T) {
	r := &rec{}
	up := upstream(t, r, func(w http.ResponseWriter, _, _ string) { limited(w) })
	p := newProxy(t, up, "k0", "k1", "k2")

	resp, _ := post(t, p.URL+"/v1/chat/completions", "", chatBody(deepseek, false))
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", resp.StatusCode)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("no Retry-After: the caller has no idea when to come back")
	}
	if got := len(r.keys()); got != 3 {
		t.Errorf("tried %d keys, want 3", got)
	}
}

func TestMaxAttemptsOneTurnsRetryingOff(t *testing.T) {
	r := &rec{}
	up := upstream(t, r, func(w http.ResponseWriter, key, _ string) {
		if key == "k0" {
			limited(w)
			return
		}
		ok(w)
	})
	p, _ := newProxyWith(t, up, 1, "k0", "k1")

	resp, _ := post(t, p.URL+"/v1/chat/completions", "", chatBody(deepseek, false))
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the upstream 429 straight through", resp.StatusCode)
	}
	if got := r.keys(); !slices.Equal(got, []string{"k0"}) {
		t.Errorf("upstream saw %v, want exactly one attempt", got)
	}
}

func TestTheSameKeyIsReusedUntilItIsRateLimited(t *testing.T) {
	r := &rec{}
	up := upstream(t, r, func(w http.ResponseWriter, _, _ string) { ok(w) })
	p := newProxy(t, up, "k0", "k1")

	for range 4 {
		post(t, p.URL+"/v1/chat/completions", "", chatBody(deepseek, false))
	}
	if got := r.keys(); !slices.Equal(got, []string{"k0", "k0", "k0", "k0"}) {
		t.Errorf("keys served %v, want key 0 throughout", got)
	}
}

func TestTheCallersTokenIsReplacedByAPoolKey(t *testing.T) {
	r := &rec{}
	up := upstream(t, r, func(w http.ResponseWriter, _, _ string) { ok(w) })
	p := newProxy(t, up, "k0")

	post(t, p.URL+"/v1/chat/completions", "sk-the-callers-own-key", chatBody(deepseek, false))
	if got := r.keys(); !slices.Equal(got, []string{"k0"}) {
		t.Errorf("upstream saw %v, want the pool key: the caller's token must not travel", got)
	}
}

// A bad request would be bad on all eight keys, so there is nothing to gain
// from trying them.
func TestClientErrorsAreNotRetried(t *testing.T) {
	r := &rec{}
	up := upstream(t, r, func(w http.ResponseWriter, _, _ string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"unknown model","success":false}`)
	})
	p := newProxy(t, up, "k0", "k1", "k2")

	resp, out := post(t, p.URL+"/v1/chat/completions", "", chatBody("nope", false))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if !strings.Contains(string(out), "unknown model") {
		t.Errorf("upstream's explanation was lost: %s", out)
	}
	if got := len(r.calls()); got != 1 {
		t.Errorf("made %d attempts, want 1", got)
	}
}

// Every key points at the same host, so an upstream outage is not something a
// different key fixes; retrying would only triple the caller's wait.
func TestUpstreamErrorsAreNotRetried(t *testing.T) {
	r := &rec{}
	up := upstream(t, r, func(w http.ResponseWriter, _, _ string) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	p := newProxy(t, up, "k0", "k1", "k2")

	resp, _ := post(t, p.URL+"/v1/chat/completions", "", chatBody(deepseek, false))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", resp.StatusCode)
	}
	if got := len(r.calls()); got != 1 {
		t.Errorf("made %d attempts, want 1", got)
	}
}

func TestUnreachableUpstream(t *testing.T) {
	base, _ := url.Parse("http://127.0.0.1:1/api/v1")
	p := httptest.NewServer(&proxy{
		base:        base,
		client:      newHTTPClient(),
		pool:        newPool([]string{"k0"}, time.Minute, time.Hour),
		maxAttempts: 3,
	})
	defer p.Close()

	resp, out := post(t, p.URL+"/v1/chat/completions", "", chatBody(deepseek, false))
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	var got struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(out, &got) != nil || got.Error.Message == "" {
		t.Errorf("expected an OpenAI-shaped error, got %s", out)
	}
}

func TestProxyTokenIsEnforced(t *testing.T) {
	r := &rec{}
	up := upstream(t, r, func(w http.ResponseWriter, _, _ string) { ok(w) })
	p, pr := newProxyWith(t, up, 3, "k0")
	pr.clientToken = "s3cret"

	if resp, _ := post(t, p.URL+"/v1/chat/completions", "wrong", chatBody(deepseek, false)); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("wrong token got %d, want 401", resp.StatusCode)
	}
	if resp, _ := post(t, p.URL+"/v1/chat/completions", "", chatBody(deepseek, false)); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("no token got %d, want 401", resp.StatusCode)
	}
	if len(r.calls()) != 0 {
		t.Errorf("upstream was called %d times before the token checked out", len(r.calls()))
	}

	if resp, _ := post(t, p.URL+"/v1/chat/completions", "s3cret", chatBody(deepseek, false)); resp.StatusCode != http.StatusOK {
		t.Errorf("right token got %d, want 200", resp.StatusCode)
	}
}

func TestOnlyChatCompletionsIsServed(t *testing.T) {
	r := &rec{}
	up := upstream(t, r, func(w http.ResponseWriter, _, _ string) { ok(w) })
	p := newProxy(t, up, "k0")

	resp, err := http.Get(p.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}

	if resp, _ := post(t, p.URL+"/v1/chat/completions", "", chatBody(deepseek, false)); resp.StatusCode != http.StatusOK {
		t.Errorf("chat completions stopped working")
	}
	if path := r.calls(); len(path) != 1 {
		t.Errorf("upstream saw %v", path)
	}
}
