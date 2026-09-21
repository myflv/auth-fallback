package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// The Cline gateway keeps its /api prefix; /v1/chat/completions is appended to
// this base.
const defaultUpstream = "https://api.cline.bot/api/v1"

const (
	defaultCooldown    = 60 * time.Second
	defaultCooldownMax = 30 * time.Minute
	defaultMaxAttempts = 3
)

type config struct {
	Listen string `json:"listen"`
	// Upstream is the API base, without /chat/completions.
	Upstream string `json:"upstream"`
	// ClientToken, when set, is the token callers must present. Without it the
	// proxy is an open door to every key in the pool.
	ClientToken string `json:"client_token"`
	// Cooldown is how long a key is parked after a 429, doubling on each
	// consecutive 429 up to CooldownMax.
	Cooldown    duration `json:"cooldown"`
	CooldownMax duration `json:"cooldown_max"`
	// MaxAttempts caps how many keys one request may burn through. Set it to 1
	// to pick a key and live with whatever that key answers.
	MaxAttempts int      `json:"max_attempts"`
	Keys        []string `json:"keys"`
}

func loadConfig(path string) (*config, error) {
	var c config
	body, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(body, &c); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	case errors.Is(err, os.ErrNotExist) && os.Getenv("CLINE_KEYS") != "":
		// Container mode: keys come from the environment, everything else is
		// defaults.
	default:
		return nil, err
	}

	if v := os.Getenv("CLINE_KEYS"); v != "" {
		c.Keys = strings.Split(v, ",")
	}
	if v := os.Getenv("CLIENT_TOKEN"); v != "" {
		c.ClientToken = v
	}
	return &c, c.withDefaults()
}

func (c *config) withDefaults() error {
	if c.Listen == "" {
		c.Listen = defaultListen()
	}
	if c.Upstream == "" {
		c.Upstream = defaultUpstream
	}
	if c.Cooldown <= 0 {
		c.Cooldown = duration(defaultCooldown)
	}
	if c.CooldownMax <= 0 {
		c.CooldownMax = duration(defaultCooldownMax)
	}
	if c.CooldownMax < c.Cooldown {
		c.CooldownMax = c.Cooldown
	}
	if c.MaxAttempts <= 0 {
		c.MaxAttempts = defaultMaxAttempts
	}

	keys := c.Keys[:0]
	for _, k := range c.Keys {
		if k = strings.TrimSpace(k); k != "" {
			keys = append(keys, k)
		}
	}
	c.Keys = keys
	if len(c.Keys) == 0 {
		return errors.New("no API keys: set keys[] in the config file or CLINE_KEYS in the environment")
	}
	return nil
}

func defaultListen() string {
	if port := os.Getenv("PORT"); port != "" {
		return ":" + port
	}
	return ":8787"
}

// duration accepts both "90s" and a bare number of seconds.
type duration time.Duration

func (d duration) String() string { return time.Duration(d).String() }

func (d *duration) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		return nil
	}
	if n, err := strconv.ParseFloat(s, 64); err == nil {
		*d = duration(time.Duration(n * float64(time.Second)))
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("bad duration %s", b)
	}
	*d = duration(v)
	return nil
}
