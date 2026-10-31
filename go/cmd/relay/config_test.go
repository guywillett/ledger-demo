package main

import (
	"strings"
	"testing"
	"time"
)

// Node's loadRelayConfig cases (test/shared/config.test.ts).
func env(vars map[string]string) func(string) (string, bool) {
	all := map[string]string{
		"DATABASE_URL": "postgres://ledger:ledger@localhost:5432/ledger",
		"NATS_URL":     "nats://localhost:4222",
	}
	for k, v := range vars {
		all[k] = v
	}
	return func(name string) (string, bool) {
		v, ok := all[name]
		return v, ok
	}
}

func TestDefaultsToBatchesOf10UnderA30sLease(t *testing.T) {
	cfg, err := loadConfig(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.NATS.URL != "nats://localhost:4222" || cfg.BatchSize != 10 || cfg.Lease != 30*time.Second {
		t.Errorf("got %+v", cfg)
	}
}

// Beyond 60 s, a crashed relay's re-publish could fall outside the stream's 120 s duplicate window.
func TestRefusesALeaseLongerThan60s(t *testing.T) {
	if _, err := loadConfig(env(map[string]string{"RELAY_LEASE_MS": "60001"})); err == nil ||
		!strings.Contains(err.Error(), "RELAY_LEASE_MS") {
		t.Errorf("60001: err = %v", err)
	}
	if cfg, err := loadConfig(env(map[string]string{"RELAY_LEASE_MS": "60000"})); err != nil || cfg.Lease != time.Minute {
		t.Errorf("60000: %v %v", cfg.Lease, err)
	}
}

// 20 × 2000 ms publish timeout = 40 s of worst-case publishing, + 5 s for database work.
func TestRefusesALeaseShorterThanABatchOfTimedOutPublishesPlusTheMargin(t *testing.T) {
	_, err := loadConfig(env(map[string]string{"RELAY_BATCH_SIZE": "20", "RELAY_LEASE_MS": "44999"}))
	want := "RELAY_LEASE_MS must be at least RELAY_BATCH_SIZE × 2000 ms (the publish timeout) + 5000 ms " +
		"(database work): a batch whose publishes all time out would otherwise outlast its lease, and " +
		"another relay would claim and publish the same events while this one is still on them"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v", err)
	}
	cfg, err := loadConfig(env(map[string]string{"RELAY_BATCH_SIZE": "20", "RELAY_LEASE_MS": "45000"}))
	if err != nil || cfg.Lease != 45*time.Second {
		t.Errorf("45000: %v %v", cfg.Lease, err)
	}
}

// The rule across variables runs only once each has passed, as Zod's refine: a batch size out of
// range is reported alone.
func TestTheLeaseRuleWaitsForValidValues(t *testing.T) {
	_, err := loadConfig(env(map[string]string{"RELAY_BATCH_SIZE": "1001", "RELAY_LEASE_MS": "1000"}))
	if err == nil || strings.Contains(err.Error(), "must be at least") ||
		!strings.Contains(err.Error(), "RELAY_BATCH_SIZE must be a whole number from 1 to 1000") {
		t.Errorf("err = %v", err)
	}
}
