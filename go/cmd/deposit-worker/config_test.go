package main

import (
	"strings"
	"testing"
	"time"

	"github.com/guywillett/ledger-demo/go/internal/config"
)

func parse(vars map[string]string) (workerConfig, error) {
	all := map[string]string{
		"DATABASE_URL": "postgres://ledger:ledger@localhost:5432/ledger",
		"NATS_URL":     "nats://localhost:4222",
	}
	for k, v := range vars {
		all[k] = v
	}
	p := config.NewParser(func(name string) (string, bool) {
		v, ok := all[name]
		return v, ok
	})
	cfg := loadConfig(p)
	return cfg, p.Err()
}

// Node's defaults: a 2 s review, 80% approved, 10 at once; NATS_URL required.
func TestDefaults(t *testing.T) {
	cfg, err := parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ReviewDelay != 2*time.Second || cfg.ApproveRate != 0.8 || cfg.Concurrency != 10 ||
		cfg.NATS.URL != "nats://localhost:4222" {
		t.Errorf("got %+v", cfg)
	}
	p := config.NewParser(func(name string) (string, bool) {
		if name == "DATABASE_URL" {
			return "postgres://ledger:ledger@localhost:5432/ledger", true
		}
		return "", false
	})
	loadConfig(p)
	if p.Err() == nil || !strings.Contains(p.Err().Error(), "NATS_URL is required") {
		t.Errorf("without NATS_URL: %v", p.Err())
	}
}

// Node's bounds: REVIEW_DELAY_MS 0 to 60000, APPROVE_RATE 0 to 1, DEPOSIT_WORKER_CONCURRENCY 1
// to 1000.
func TestBounds(t *testing.T) {
	for _, vars := range []map[string]string{
		{"REVIEW_DELAY_MS": "0", "APPROVE_RATE": "0", "DEPOSIT_WORKER_CONCURRENCY": "1"},
		{"REVIEW_DELAY_MS": "60000", "APPROVE_RATE": "1", "DEPOSIT_WORKER_CONCURRENCY": "1000"},
	} {
		if _, err := parse(vars); err != nil {
			t.Errorf("%v: %v", vars, err)
		}
	}
	for name, value := range map[string]string{
		"REVIEW_DELAY_MS":            "60001",
		"APPROVE_RATE":               "1.01",
		"DEPOSIT_WORKER_CONCURRENCY": "0",
	} {
		_, err := parse(map[string]string{name: value})
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s=%s: err = %v", name, value, err)
		}
	}
}
