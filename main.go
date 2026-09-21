package main

import (
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	addr := flag.String("addr", "", "listen address, overrides the config file")
	configPath := flag.String("config", "config.json", "path to the config file")
	verbose := flag.Bool("v", false, "log every request and which key served it")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	if *addr != "" {
		cfg.Listen = *addr
	}
	if cfg.ClientToken == "" {
		log.Print("warning: no client_token set, anyone who can reach this port can spend your keys")
	}
	if info, err := os.Stat(*configPath); err == nil && info.Mode().Perm()&0o077 != 0 {
		log.Printf("warning: %s is readable by other users (%#o) and holds API keys",
			*configPath, info.Mode().Perm())
	}

	base, err := url.Parse(cfg.Upstream)
	if err != nil {
		log.Fatalf("upstream %q: %v", cfg.Upstream, err)
	}

	p := &proxy{
		base:   base,
		client: newHTTPClient(),
		pool: newPool(cfg.Keys,
			time.Duration(cfg.Cooldown), time.Duration(cfg.CooldownMax)),
		maxAttempts: cfg.MaxAttempts,
		clientToken: cfg.ClientToken,
		verbose:     *verbose,
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           p,
		ReadHeaderTimeout: 15 * time.Second,
		// No WriteTimeout: an answer can take minutes.
	}

	// Model names come out of the request body, so the cooldown table needs
	// reclaiming. Nothing depends on the exact interval: an entry only becomes
	// reclaimable max after its cooldown has ended, so anything well under that
	// is early enough.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go p.pool.sweepLoop(ctx, time.Minute)

	log.Printf("%d keys -> %s, listening on %s (429 cooldown %s, doubling to %s, %d attempt(s) max)",
		len(cfg.Keys), cfg.Upstream, cfg.Listen, cfg.Cooldown, cfg.CooldownMax, cfg.MaxAttempts)

	errc := make(chan error, 1)
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
		}
	}()

	select {
	case err := <-errc:
		log.Fatalf("server error: %v", err)
	case <-ctx.Done():
		log.Print("shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}
}

func newHTTPClient() *http.Client {
	// No Client.Timeout: answers legitimately take minutes.
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   20,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 5 * time.Minute,
			DisableCompression:    true, // keep SSE frames intact
		},
	}
}
