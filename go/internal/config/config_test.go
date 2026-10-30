package config_test

import (
	"strings"
	"testing"

	"github.com/guywillett/ledger-demo/go/internal/config"
)

// env turns a map into a Lookup: a variable is set exactly when it is in the map.
func env(vars map[string]string) config.Lookup {
	return func(name string) (string, bool) {
		value, ok := vars[name]
		return value, ok
	}
}

const databaseURL = "postgres://ledger:secret@localhost:5432/ledger?sslmode=disable"

func TestParseShared(t *testing.T) {
	p := config.NewParser(env(map[string]string{"DATABASE_URL": databaseURL, "LOG_LEVEL": "debug"}))
	got := config.ParseShared(p)
	if err := p.Err(); err != nil {
		t.Fatalf("Err() = %v, want nil", err)
	}
	want := config.Shared{DatabaseURL: databaseURL, LogLevel: "debug"}
	if got != want {
		t.Errorf("ParseShared() = %+v, want %+v", got, want)
	}
}

func TestLogLevelDefaultsToInfo(t *testing.T) {
	p := config.NewParser(env(map[string]string{"DATABASE_URL": databaseURL}))
	if got := config.ParseShared(p).LogLevel; got != "info" {
		t.Errorf("LogLevel = %q, want info", got)
	}
	if err := p.Err(); err != nil {
		t.Errorf("Err() = %v, want nil", err)
	}
}

// Every problem is reported at once, one per line, as Node's parse does.
func TestReportsEveryProblem(t *testing.T) {
	p := config.NewParser(env(map[string]string{"LOG_LEVEL": "verbose"}))
	config.ParseShared(p)
	config.ParseNATS(p)
	err := p.Err()
	if err == nil {
		t.Fatal("Err() = nil, want an error")
	}
	want := "invalid configuration:\n" +
		"DATABASE_URL is required\n" +
		"LOG_LEVEL must be one of fatal, error, warn, info, debug, trace, silent\n" +
		"NATS_URL is required"
	if err.Error() != want {
		t.Errorf("Err() =\n%s\nwant\n%s", err, want)
	}
}

func TestRejects(t *testing.T) {
	// A table-driven test: one case per row, each run as a named subtest (t.Run), the Go
	// counterpart of Vitest's it.each.
	tests := []struct {
		name string
		vars map[string]string
		want string
	}{
		{"a URL without a scheme", map[string]string{"DATABASE_URL": "localhost/ledger"}, "DATABASE_URL must be an absolute URL"},
		{"an unparseable URL", map[string]string{"DATABASE_URL": "postgres://%zz"}, "DATABASE_URL must be an absolute URL"},
		// Set but empty is checked, not defaulted (as in Node).
		{"an empty URL", map[string]string{"DATABASE_URL": ""}, "DATABASE_URL must be an absolute URL"},
		{"an empty log level", map[string]string{"DATABASE_URL": databaseURL, "LOG_LEVEL": ""}, "LOG_LEVEL must be one of"},
		{"an upper-case log level", map[string]string{"DATABASE_URL": databaseURL, "LOG_LEVEL": "INFO"}, "LOG_LEVEL must be one of"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := config.NewParser(env(tt.vars))
			config.ParseShared(p)
			err := p.Err()
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Err() = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

// The error names the variable but never repeats its value: a database URL carries a password.
func TestNeverEchoesTheValue(t *testing.T) {
	p := config.NewParser(env(map[string]string{"DATABASE_URL": "secret-password"}))
	config.ParseShared(p)
	if err := p.Err(); err == nil || strings.Contains(err.Error(), "secret") {
		t.Errorf("Err() = %v, want an error without the value", err)
	}
}

func TestParseNATS(t *testing.T) {
	p := config.NewParser(env(map[string]string{"NATS_URL": "nats://localhost:4222"}))
	if got := config.ParseNATS(p).URL; got != "nats://localhost:4222" {
		t.Errorf("URL = %q, want nats://localhost:4222", got)
	}
	if err := p.Err(); err != nil {
		t.Errorf("Err() = %v, want nil", err)
	}
}

func TestInt(t *testing.T) {
	tests := []struct {
		vars map[string]string
		want int
		err  bool
	}{
		{map[string]string{}, 3000, false}, // unset: the default
		{map[string]string{"PORT": "8080"}, 8080, false},
		{map[string]string{"PORT": "1"}, 1, false},
		{map[string]string{"PORT": "65535"}, 65535, false},
		{map[string]string{"PORT": "0"}, 0, true},
		{map[string]string{"PORT": "65536"}, 0, true},
		{map[string]string{"PORT": ""}, 0, true},
		{map[string]string{"PORT": "3e3"}, 0, true}, // Node's Number() would read 3000
		{map[string]string{"PORT": "http"}, 0, true},
	}
	for _, tt := range tests {
		p := config.NewParser(env(tt.vars))
		got := p.Int("PORT", 3000, 1, 65535)
		if err := p.Err(); (err != nil) != tt.err || got != tt.want {
			t.Errorf("Int(%v) = %d, %v; want %d, error %v", tt.vars, got, err, tt.want, tt.err)
		}
	}
}
