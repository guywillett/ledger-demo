package main

import (
	"bytes"
	"errors"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The API as a process: the test binary runs main itself when runMainEnv is set, so a test can
// start the real API as a child process (os.Args[0] is this test binary) and send it signals.
// TestMain runs instead of the tests when a test package defines one.
const runMainEnv = "LEDGER_API_TEST_RUN_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(runMainEnv) == "1" {
		main() // exits the process itself
		return
	}
	os.Exit(m.Run())
}

// syncBuffer collects the child's output; the test reads it while the child writes.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func waitForLine(t *testing.T, out *syncBuffer, text string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(out.String(), text) {
		if time.Now().After(deadline) {
			t.Fatalf("no %q line; output:\n%s", text, out)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// After SIGTERM the API waits up to its grace period (5 s) for requests in flight. A second
// SIGTERM during that wait has its default effect, as Node's process.once handlers allow: the
// process ends at once, killed by the signal, instead of the signal being caught and ignored.
func TestASecondSignalEndsTheShutdownAtOnce(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		databaseURL = "postgres://ledger:ledger@localhost:5432/ledger_go_test?sslmode=disable"
	}
	// A free port: listen on port 0 (the system picks one), note it, close.
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()

	// CommandContext: the child is killed if the test ends first (t.Context is cancelled then).
	cmd := exec.CommandContext(t.Context(), os.Args[0])
	cmd.Env = append(os.Environ(), runMainEnv+"=1", "DATABASE_URL="+databaseURL,
		"PORT="+strconv.Itoa(port), "LOG_LEVEL=info")
	out := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	waitForLine(t, out, "server listening")

	// A request in flight that won't finish: a body that never fully arrives. The handler waits
	// for it (until ReadTimeout, 30 s), so shutdown waits its whole grace period.
	var d net.Dialer
	conn, err := d.DialContext(t.Context(), "tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, err = conn.Write([]byte("POST /provider/incoming-payments HTTP/1.1\r\nHost: test\r\n" +
		"Content-Type: application/json\r\nContent-Length: 100\r\n\r\n{"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond) // into the handler

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitForLine(t, out, "shutting down") // logged once stop() has run
	sent := time.Now()
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-exited:
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("exit: %v", err)
		}
		status := exit.Sys().(syscall.WaitStatus)
		if !status.Signaled() || status.Signal() != syscall.SIGTERM {
			t.Errorf("ended with %v, want killed by SIGTERM; output:\n%s", err, out)
		}
		if elapsed := time.Since(sent); elapsed > 2*time.Second {
			t.Errorf("ended %v after the second signal, want at once", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("still running 10 s after the second signal; output:\n%s", out)
	}
}
