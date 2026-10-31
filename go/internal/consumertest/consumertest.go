// Package consumertest is what the consumers' handler tests share (the deposit-worker's, later the
// submitter's): a fake message that records its signals, and a logger whose lines a test can read
// back. Node: the fake message and capturingLogger in test/helpers.ts. Imported only by tests,
// like testdb (and the standard library's httptest).
package consumertest

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/guywillett/ledger-demo/go/internal/logging"
)

// Message is a consumer.Message that records its signals, as Node's tests' fake: no NATS, and a
// "redelivery" is handling it again. Safe for the handler's goroutine and the test's.
type Message struct {
	body    string
	msgID   string
	attempt int

	mu         sync.Mutex
	calls      []string
	nakDelay   time.Duration
	workingErr error
}

// FailWorking makes every later Working fail with err (recorded all the same), as on a closed
// connection.
func (m *Message) FailWorking(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.workingErr = err
}

// NewMessage is a message with this body, Nats-Msg-Id ("" for none) and delivery attempt.
func NewMessage(body, msgID string, attempt int) *Message {
	return &Message{body: body, msgID: msgID, attempt: attempt}
}

func (m *Message) Body() []byte         { return []byte(m.body) }
func (m *Message) NATSMsgID() string    { return m.msgID }
func (m *Message) DeliveryAttempt() int { return m.attempt }

func (m *Message) record(signal string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, signal)
	return nil
}

func (m *Message) Ack() error        { return m.record("ack") }
func (m *Message) Term(string) error { return m.record("term") }
func (m *Message) Working() error {
	_ = m.record("working")
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.workingErr
}
func (m *Message) Nak(d time.Duration) error {
	m.mu.Lock()
	m.nakDelay = d
	m.mu.Unlock()
	return m.record("nak")
}

// Calls are the signals received, in order.
func (m *Message) Calls() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

// NakDelay is the delay of the last Nak.
func (m *Message) NakDelay() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.nakDelay
}

// Logs is a logger's output, readable line by line.
type Logs struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *Logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

// NewLogger is a logger (the processes' JSON format, at info) writing to the returned Logs.
func NewLogger(t testing.TB) (*slog.Logger, *Logs) {
	t.Helper()
	logs := &Logs{}
	logger, err := logging.New(logs, "test", "info")
	if err != nil {
		t.Fatal(err)
	}
	return logger, logs
}

// Lines are the log lines, decoded; with level, only those at that level.
func (l *Logs) Lines(level string) []map[string]any {
	l.mu.Lock()
	defer l.mu.Unlock()
	var found []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(l.buf.String()), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil && (level == "" || m["level"] == level) {
			found = append(found, m)
		}
	}
	return found
}

// Line is the first line with this message, nil if there is none.
func (l *Logs) Line(msg string) map[string]any {
	for _, m := range l.Lines("") {
		if m["msg"] == msg {
			return m
		}
	}
	return nil
}
