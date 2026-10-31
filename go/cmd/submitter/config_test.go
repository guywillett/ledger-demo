package main

import (
	"strings"
	"testing"
	"time"

	"github.com/guywillett/ledger-demo/go/internal/config"
)

func parse(vars map[string]string) (submitterConfig, error) {
	p := config.NewParser(func(name string) (string, bool) {
		v, ok := vars[name]
		return v, ok
	})
	cfg := loadConfig(p)
	return cfg, p.Err()
}

var env = map[string]string{
	"DATABASE_URL": "postgres://u:p@localhost:5432/db",
	"NATS_URL":     "nats://localhost:4222",
	"PROVIDER_URL": "http://provider-sim:4000",
}

// Node's submitter config cases (test/shared/config.test.ts).
func TestRequiresProviderURLAndDefaultsTheTimeoutTo5s(t *testing.T) {
	noProvider := map[string]string{
		"DATABASE_URL": env["DATABASE_URL"], "NATS_URL": env["NATS_URL"],
	}
	if _, err := parse(noProvider); err == nil || !strings.Contains(err.Error(), "PROVIDER_URL") {
		t.Errorf("without PROVIDER_URL: %v", err)
	}
	cfg, err := parse(env)
	if err != nil || cfg.ProviderTimeout != 5*time.Second || cfg.Concurrency != 10 {
		t.Errorf("defaults: %+v %v", cfg, err)
	}
	for _, bad := range []string{"99", "120001"} {
		vars := map[string]string{"PROVIDER_TIMEOUT_MS": bad}
		for k, v := range env {
			vars[k] = v
		}
		_, err := parse(vars)
		if err == nil || !strings.Contains(err.Error(), "PROVIDER_TIMEOUT_MS") {
			t.Errorf("PROVIDER_TIMEOUT_MS=%s: %v", bad, err)
		}
	}
}

// Against the consumer's real ack_wait, read from NATS at startup.
func TestAcceptsATimeoutUpToTwoThirdsOfTheAckWaitAndRefusesMoreSayingWhy(t *testing.T) {
	if err := checkTimeoutFitsAckWait(20_000*time.Millisecond, 30*time.Second); err != nil {
		t.Errorf("20000 of 30000: %v", err)
	}
	err := checkTimeoutFitsAckWait(20_001*time.Millisecond, 30*time.Second)
	if err == nil || !strings.Contains(err.Error(),
		"at most 20000 ms, two thirds of the submitter consumer's ack_wait (30000 ms)") {
		t.Errorf("20001 of 30000: %v", err)
	}
	if err := checkTimeoutFitsAckWait(5000*time.Millisecond, 6*time.Second); err == nil ||
		!strings.Contains(err.Error(), "at most 4000 ms") {
		t.Errorf("5000 of 6000: %v", err)
	}
}
