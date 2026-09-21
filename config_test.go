package main

import (
	"encoding/json"
	"testing"
	"time"
)

// config.example.json is what everyone copies to get started, so it has to keep
// carrying the real defaults. A stale example would silently pin new users to
// whatever the numbers used to be.
func TestExampleConfigMatchesTheDefaults(t *testing.T) {
	t.Setenv("CLINE_KEYS", "")
	t.Setenv("CLIENT_TOKEN", "")
	t.Setenv("PORT", "")

	c, err := loadConfig("config.example.json")
	if err != nil {
		t.Fatal(err)
	}

	if c.Upstream != defaultUpstream {
		t.Errorf("upstream = %q, want %q", c.Upstream, defaultUpstream)
	}
	if got := time.Duration(c.Cooldown); got != defaultCooldown {
		t.Errorf("cooldown = %s, want %s", got, defaultCooldown)
	}
	if got := time.Duration(c.CooldownMax); got != defaultCooldownMax {
		t.Errorf("cooldown_max = %s, want %s", got, defaultCooldownMax)
	}
	if c.MaxAttempts != defaultMaxAttempts {
		t.Errorf("max_attempts = %d, want %d", c.MaxAttempts, defaultMaxAttempts)
	}
	if c.Listen != defaultListen() {
		t.Errorf("listen = %q, want %q", c.Listen, defaultListen())
	}
	if len(c.Keys) != 8 {
		t.Errorf("keys = %d, want the 8 placeholders", len(c.Keys))
	}
}

func TestDurationAcceptsUnitsAndBareSeconds(t *testing.T) {
	var c config
	if err := json.Unmarshal([]byte(`{"cooldown":"90s","cooldown_max":120}`), &c); err != nil {
		t.Fatal(err)
	}
	if got := time.Duration(c.Cooldown); got != 90*time.Second {
		t.Errorf("cooldown = %s, want 1m30s", got)
	}
	if got := time.Duration(c.CooldownMax); got != 2*time.Minute {
		t.Errorf("cooldown_max = %s, want 2m", got)
	}
}

// A ceiling under the starting point would make the doubling meaningless.
func TestCooldownMaxIsRaisedToTheBase(t *testing.T) {
	c := config{
		Cooldown:    duration(time.Minute),
		CooldownMax: duration(time.Second),
		Keys:        []string{"k"},
	}
	if err := c.withDefaults(); err != nil {
		t.Fatal(err)
	}
	if c.CooldownMax != c.Cooldown {
		t.Errorf("cooldown_max = %s, want it raised to %s", c.CooldownMax, c.Cooldown)
	}
}

func TestEnvOverridesTheFile(t *testing.T) {
	t.Setenv("CLINE_KEYS", "k0, k1 ,k2")
	t.Setenv("CLIENT_TOKEN", "tok")

	c, err := loadConfig("config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Keys) != 3 {
		t.Fatalf("keys = %v, want the three from the environment", c.Keys)
	}
	if c.ClientToken != "tok" {
		t.Errorf("client_token = %q, want the environment to win over the file", c.ClientToken)
	}
}

func TestNoFileWorksWhenKeysComeFromTheEnvironment(t *testing.T) {
	t.Setenv("CLINE_KEYS", "k0")
	c, err := loadConfig("does-not-exist.json")
	if err != nil {
		t.Fatalf("the container path should need no file: %v", err)
	}
	if c.Upstream != defaultUpstream || len(c.Keys) != 1 {
		t.Errorf("config = %+v", c)
	}
}

// With nothing to fall back on, the missing-file error is the useful one.
func TestNoFileAndNoEnvKeysIsAnError(t *testing.T) {
	t.Setenv("CLINE_KEYS", "")
	if _, err := loadConfig("does-not-exist.json"); err == nil {
		t.Error("want an error naming the missing file")
	}
}
