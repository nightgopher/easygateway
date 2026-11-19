package safego

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuffer is a goroutine-safe log sink: the goroutines under test write from
// their own stacks while the test reads.
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

func TestRunContainsPanicAndLogsStack(t *testing.T) {
	sink := &syncBuffer{}
	logger := log.New(sink, "", 0)

	returned := false
	Run(logger, "worker", func() {
		panic("boom")
	})
	returned = true

	if !returned {
		t.Fatal("Run did not return after a panic")
	}
	logged := sink.String()
	if !strings.Contains(logged, "panic contained in worker") {
		t.Fatalf("log does not name the label: %q", logged)
	}
	if !strings.Contains(logged, "boom") {
		t.Fatalf("log does not carry the panic value: %q", logged)
	}
	if !strings.Contains(logged, "panicking") && !strings.Contains(logged, "safego") {
		t.Fatalf("log does not carry the stack: %q", logged)
	}
}

func TestGoContainsPanicWithoutKillingTheCaller(t *testing.T) {
	sink := &syncBuffer{}
	logger := log.New(sink, "", 0)

	done := make(chan struct{})
	Go(logger, "background", func() {
		defer close(done)
		panic("goroutine boom")
	})

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("background function never ran")
	}
	waitForLog(t, sink, "panic contained in background")
}

func TestGoErrReturnsTheFunctionError(t *testing.T) {
	expected := errors.New("upstream failed")
	results := GoErr(nil, "pump", func() error { return expected })

	select {
	case got := <-results:
		if !errors.Is(got, expected) {
			t.Fatalf("GoErr returned %v, want %v", got, expected)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("GoErr did not deliver a result")
	}
}

// The panic case is the one that matters for stream pumps: the request
// goroutine blocks on the channel while draining the pipe, so a panic that
// produced no value would hang that request forever.
func TestGoErrConvertsPanicIntoError(t *testing.T) {
	sink := &syncBuffer{}
	logger := log.New(sink, "", 0)

	results := GoErr(logger, "pump", func() error {
		panic("pipe exploded")
	})

	select {
	case got := <-results:
		if got == nil {
			t.Fatal("panic produced a nil error")
		}
		if !strings.Contains(got.Error(), "pipe exploded") {
			t.Fatalf("error does not carry the panic value: %v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("GoErr deadlocked on a panicking function")
	}
	waitForLog(t, sink, "panic contained in pump")
}

func TestLogPanicFallsBackToDefaultLogger(t *testing.T) {
	// A nil logger must not swallow the report; it falls back to log.Default().
	// Redirect the default logger to keep test output clean and inspectable.
	var captured bytes.Buffer
	previous := log.Writer()
	flags := log.Flags()
	log.SetOutput(&captured)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previous)
		log.SetFlags(flags)
	})

	LogPanic(nil, "collector", "write failed")

	if !strings.Contains(captured.String(), "panic contained in collector") {
		t.Fatalf("nil logger silently dropped the report: %q", captured.String())
	}
}

func waitForLog(t *testing.T, sink *syncBuffer, needle string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(sink.String(), needle) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("log never contained %q: %q", needle, sink.String())
}
