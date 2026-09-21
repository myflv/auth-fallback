// auth-fallback puts a pool of Cline API keys behind one OpenAI-shaped
// endpoint. A model keeps using one key until upstream answers 429, which parks
// that key for that model and moves the request on to the next key.
//
// Only /v1/chat/completions is routed; the request body is forwarded byte for
// byte and the response is streamed back untouched.
package main

import (
	"bytes"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	clientType = "cline-cli"
	// maxBody is plenty for a chat request, images and all, and stops a rogue
	// caller from making the proxy hold gigabytes.
	maxBody = 64 << 20
	// maxErrBody caps how much of an upstream error we keep to hand back.
	maxErrBody = 64 << 10
)

var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

type proxy struct {
	base        *url.URL
	client      *http.Client
	pool        *pool
	maxAttempts int
	clientToken string
	verbose     bool
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/healthz":
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	case r.URL.Path == "/status":
		writeJSON(w, http.StatusOK, p.pool.snapshot(time.Now()))
	case r.URL.Path == "/v1/chat/completions":
		p.chat(w, r)
	default:
		writeError(w, http.StatusNotFound, "only /v1/chat/completions is served")
	}
}

func (p *proxy) chat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "only POST is supported")
		return
	}
	if !p.authorized(r) {
		writeError(w, http.StatusUnauthorized, "wrong or missing proxy token")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "unreadable request body: "+err.Error())
		return
	}
	model := peekModel(body)

	order := p.pool.order(model, time.Now())
	if len(order) == 0 {
		writeError(w, http.StatusServiceUnavailable, "no API keys configured")
		return
	}
	tries := min(len(order), p.maxAttempts)

	// Retries are deliberately narrow: only a 429 moves on to the next key. It
	// arrives immediately, so a retry never multiplies the caller's latency,
	// and it is the one answer that says something about the key rather than
	// about the request.
	for _, index := range order[:tries] {
		if r.Context().Err() != nil {
			return // caller hung up; nothing left to answer
		}

		resp, err := p.send(r, body, index)
		if err != nil {
			writeError(w, http.StatusBadGateway, err.Error())
			return
		}

		if resp.StatusCode == http.StatusOK {
			p.pool.succeeded(index, model)
			p.logf("model=%s key=%d ok", model, index)
			p.relay(w, resp)
			return
		}

		if resp.StatusCode == http.StatusTooManyRequests {
			drain(resp)
			until := p.pool.rateLimited(index, model, time.Now())
			log.Printf("model=%s key=%d 429, parked for %s",
				model, index, time.Until(until).Round(time.Second))
			continue
		}

		// Anything else would look the same on the next key -- a bad request,
		// a rejected key, an upstream outage -- so it goes straight back
		// rather than costing the caller seven more round trips.
		p.relay(w, resp)
		return
	}

	// Every key we were willing to try is rate limited.
	if secs := p.pool.retryAfter(model, time.Now()); secs > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(secs))
	}
	writeError(w, http.StatusTooManyRequests, "every key is rate limited for "+model)
}

// authorized checks the caller's token. An empty client token means the proxy
// is open, which is only sane on a loopback or otherwise private address.
func (p *proxy) authorized(r *http.Request) bool {
	if p.clientToken == "" {
		return true
	}
	got := r.Header.Get("Authorization")
	if len(got) > 7 && strings.EqualFold(got[:7], "bearer ") {
		got = got[7:]
	}
	got = strings.TrimSpace(got)
	return subtle.ConstantTimeCompare([]byte(got), []byte(p.clientToken)) == 1
}

// send forwards the request with one key substituted in. Everything else,
// headers included, travels unchanged.
func (p *proxy) send(r *http.Request, body []byte, index int) (*http.Response, error) {
	target := *p.base
	target.Path = strings.TrimSuffix(target.Path, "/") + "/chat/completions"
	target.RawQuery = ""

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header = r.Header.Clone()
	for _, h := range hopHeaders {
		req.Header.Del(h)
	}
	// Keep SSE frames intact; Go would otherwise transparently gunzip them.
	req.Header.Del("Accept-Encoding")

	// The caller's own Authorization is dropped: the pool decides the key.
	req.Header.Set("Authorization", "Bearer "+p.pool.key(index))
	req.Header.Set("Content-Type", "application/json")
	if req.Header.Get("x-client-type") == "" {
		req.Header.Set("x-client-type", clientType)
	}
	req.ContentLength = int64(len(body))

	return p.client.Do(req)
}

// relay streams a successful upstream reply back, flushing as it goes so SSE
// frames reach the caller the moment they arrive.
func (p *proxy) relay(w http.ResponseWriter, resp *http.Response) {
	defer resp.Body.Close()

	if ct := resp.Header.Get("Content-Type"); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(resp.StatusCode)

	if f, ok := w.(http.Flusher); ok {
		_, _ = io.Copy(&flushWriter{w: w, f: f}, resp.Body)
		return
	}
	_, _ = io.Copy(w, resp.Body)
}

// drain reads and closes a response we are not going to use, so its connection
// can go back to the pool instead of being torn down.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrBody))
	resp.Body.Close()
}

func (p *proxy) logf(format string, args ...any) {
	if p.verbose {
		log.Printf(format, args...)
	}
}

// peekModel pulls the model out of the body, which is what cooldowns are scoped
// by. A body we cannot parse gets the empty model, sharing one cooldown slot;
// upstream will reject it anyway.
func peekModel(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	if json.Unmarshal(body, &probe) != nil {
		return ""
	}
	return probe.Model
}

type flushWriter struct {
	w io.Writer
	f http.Flusher
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	fw.f.Flush()
	return n, err
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{
		"error": map[string]any{"message": msg, "type": "proxy_error", "code": status},
	})
}
