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

// rec records the key each upstream request arrived with, in order.
type rec struct {
	mu   sync.Mutex
	seen []string
}

func (r *rec) add(key string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, key)
}

func (r *rec) keys() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

func upstream(t *testing.T, r *rec, h func(w http.ResponseWriter, key, model string)) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		key := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		body, _ := io.ReadAll(req.Body)
		r.add(key)
		h(w, key, peekModel(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func ok(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"data":{"id":"gen_1","object":"chat.completion"},"success":true}`)
}

const limitedBody = `{"error":"rate limited","success":false}`

func limited(w http.ResponseWriter) {
	status(w, http.StatusTooManyRequests, limitedBody)
}

func status(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, body)
}

func newProxy(t *testing.T, up *httptest.Server, keys ...string) *httptest.Server {
	t.Helper()
	srv, _ := newProxyAt(t, up.URL+"/api/v1", 3, keys...)
	return srv
}

func newProxyAt(t *testing.T, baseURL string, maxAttempts int, keys ...string) (*httptest.Server, *proxy) {
	t.Helper()
	base, err := url.Parse(baseURL)
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
	return send(t, http.MethodPost, url, token, body)
}

func get(t *testing.T, url, token string) (*http.Response, []byte) {
	t.Helper()
	return send(t, http.MethodGet, url, token, "")
}

func send(t *testing.T, method, url, token, body string) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
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

	before := len(r.keys())
	resp, out := post(t, p.URL+"/v1/chat/completions", "", chatBody(claude, false))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the other model got %d: %s", resp.StatusCode, out)
	}
	if got := len(r.keys()) - before; got != 1 {
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
	p, _ := newProxyAt(t, up.URL+"/api/v1", 1, "k0", "k1")

	resp, out := post(t, p.URL+"/v1/chat/completions", "", chatBody(deepseek, false))
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want the upstream 429 straight through", resp.StatusCode)
	}
	if got := r.keys(); !slices.Equal(got, []string{"k0"}) {
		t.Errorf("upstream saw %v, want exactly one attempt", got)
	}
	// Upstream's own explanation, not a synthesized one. Telling the caller
	// "every key is rate limited" would be false with k1 sitting free.
	if string(out) != limitedBody {
		t.Errorf("body = %s, want upstream's own %s", out, limitedBody)
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

// Every key points at the same host and carries the same request, so only a 429
// says anything the next key could answer differently.
func TestOnly429IsRetried(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"bad request", http.StatusBadRequest, `{"error":"unknown model","success":false}`},
		{"rejected key", http.StatusUnauthorized, `{"error":"bad key","success":false}`},
		{"upstream outage", http.StatusInternalServerError, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &rec{}
			up := upstream(t, r, func(w http.ResponseWriter, _, _ string) {
				status(w, tc.status, tc.body)
			})
			p := newProxy(t, up, "k0", "k1", "k2")

			resp, out := post(t, p.URL+"/v1/chat/completions", "", chatBody(deepseek, false))
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			if tc.body != "" && string(out) != tc.body {
				t.Errorf("body = %s, want upstream's own %s", out, tc.body)
			}
			if got := len(r.keys()); got != 1 {
				t.Errorf("made %d attempts, want 1", got)
			}
		})
	}
}

func TestUnreachableUpstream(t *testing.T) {
	p, _ := newProxyAt(t, "http://127.0.0.1:1/api/v1", 3, "k0")

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
	p, pr := newProxyAt(t, up.URL+"/api/v1", 3, "k0")
	pr.clientToken = "s3cret"

	for _, tc := range []struct{ name, token string }{
		{"wrong token", "wrong"},
		{"no token", ""},
	} {
		if resp, _ := post(t, p.URL+"/v1/chat/completions", tc.token, chatBody(deepseek, false)); resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: got %d, want 401", tc.name, resp.StatusCode)
		}
	}
	// The check runs before routing, so an unknown path sits behind it as well:
	// the proxy cannot be used to probe which paths exist without a token.
	if resp, _ := get(t, p.URL+"/v1/models", ""); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("an unrouted path without a token got %d, want 401", resp.StatusCode)
	}
	// /healthz stays open: a load balancer has no token to offer.
	if resp, _ := get(t, p.URL+"/healthz", ""); resp.StatusCode != http.StatusOK {
		t.Errorf("/healthz got %d, want 200", resp.StatusCode)
	}
	if len(r.keys()) != 0 {
		t.Errorf("upstream was called %d times before the token checked out", len(r.keys()))
	}

	if resp, _ := post(t, p.URL+"/v1/chat/completions", "s3cret", chatBody(deepseek, false)); resp.StatusCode != http.StatusOK {
		t.Errorf("right token got %d, want 200", resp.StatusCode)
	}
}

func TestOnlyChatCompletionsIsServed(t *testing.T) {
	r := &rec{}
	up := upstream(t, r, func(w http.ResponseWriter, _, _ string) { ok(w) })
	p := newProxy(t, up, "k0")

	if resp, _ := get(t, p.URL+"/v1/models", ""); resp.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404", resp.StatusCode)
	}
	if resp, _ := post(t, p.URL+"/v1/chat/completions", "", chatBody(deepseek, false)); resp.StatusCode != http.StatusOK {
		t.Errorf("chat completions stopped working")
	}
	if got := len(r.keys()); got != 1 {
		t.Errorf("upstream saw %d requests, want only the chat one", got)
	}
}

// The model name becomes a key in the pool's tables and is kept there, so an
// implausible one is refused before it can be carried around.
func TestImplausiblyLongModelNamesAreRefused(t *testing.T) {
	r := &rec{}
	up := upstream(t, r, func(w http.ResponseWriter, _, _ string) { ok(w) })
	p := newProxy(t, up, "k0")

	for _, tc := range []struct {
		name  string
		model string
		want  int
	}{
		{"a real id", deepseek, http.StatusOK},
		{"at the limit", strings.Repeat("m", maxModelLen), http.StatusOK},
		{"past the limit", strings.Repeat("m", maxModelLen+1), http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, _ := post(t, p.URL+"/v1/chat/completions", "", chatBody(tc.model, false))
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
		})
	}
	if got := len(r.keys()); got != 2 {
		t.Errorf("upstream saw %d requests, want only the plausible two", got)
	}
}
