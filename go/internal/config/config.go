// Package config reads each process's configuration from environment variables, with the same
// names and defaults as the Node implementation (node/src/shared/config.ts), so either
// implementation runs from the same compose file.
//
// Every problem is collected and reported together at start-up, as Node's Zod parse does,
// rather than stopping at the first. A process builds its own struct from the shared parts:
//
//	p := config.NewParser(os.LookupEnv)
//	cfg := depositWorkerConfig{Shared: config.ParseShared(p), NATS: config.ParseNATS(p), ...}
//	if err := p.Err(); err != nil { ... }
package config

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
)

// LogLevels are the accepted values of LOG_LEVEL: pino's level names, as in Node.
var LogLevels = []string{"fatal", "error", "warn", "info", "debug", "trace", "silent"}

// Shared is the configuration of every long-running process that uses the database (the API,
// the relay, the deposit-worker and the submitter).
type Shared struct {
	DatabaseURL string
	LogLevel    string
}

// NATS is the configuration of every process that connects to NATS (the relay, the
// deposit-worker and the submitter).
type NATS struct {
	URL string
}

// ParseShared reads DATABASE_URL (required) and LOG_LEVEL (default info).
func ParseShared(p *Parser) Shared {
	return Shared{
		DatabaseURL: p.URL("DATABASE_URL"),
		LogLevel:    p.OneOf("LOG_LEVEL", "info", LogLevels),
	}
}

// ParseNATS reads NATS_URL (required).
func ParseNATS(p *Parser) NATS {
	return NATS{URL: p.URL("NATS_URL")}
}

// Lookup reads one environment variable, reporting whether it is set at all: os.LookupEnv in
// a process, a map's lookup in tests. Set-but-empty is not the same as unset: as in Node, a
// default applies only to an unset variable, and an empty value is checked like any other.
type Lookup func(name string) (value string, ok bool)

// Parser reads variables one at a time and remembers every problem; Err reports them all.
// Each method returns the zero value for a variable it rejects, so parsing carries on.
type Parser struct {
	lookup Lookup
	errs   []error
}

// NewParser returns a Parser reading from lookup (usually os.LookupEnv).
func NewParser(lookup Lookup) *Parser {
	return &Parser{lookup: lookup}
}

// URL reads a required absolute URL (it must have a scheme, e.g. postgres:// or nats://).
//
// Node's z.url() parses with the WHATWG URL parser, Go's net/url is more lenient; both accept
// every URL compose and the docs use. The value is never echoed in the error: a database URL
// carries a password.
func (p *Parser) URL(name string) string {
	value, ok := p.lookup(name)
	if !ok {
		p.fail(name, "is required")
		return ""
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme == "" {
		p.fail(name, "must be an absolute URL")
		return ""
	}
	return value
}

// OneOf reads a variable that must be one of allowed, or def when it is unset.
func (p *Parser) OneOf(name, def string, allowed []string) string {
	value, ok := p.lookup(name)
	if !ok {
		return def
	}
	if !slices.Contains(allowed, value) {
		p.fail(name, "must be one of "+strings.Join(allowed, ", "))
		return ""
	}
	return value
}

func (p *Parser) fail(name, problem string) {
	p.errs = append(p.errs, fmt.Errorf("%s %s", name, problem))
}

// Err returns nil if every variable read so far was valid, or one error listing every problem,
// one per line.
func (p *Parser) Err() error {
	if len(p.errs) == 0 {
		return nil
	}
	return fmt.Errorf("invalid configuration:\n%w", errors.Join(p.errs...))
}
