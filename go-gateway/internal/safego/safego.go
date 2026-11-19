// Package safego runs background goroutines with panic containment.
//
// A panic inside a bare `go func()` is never reached by the HTTP recovery
// middleware: the runtime prints the stack and terminates the process. Every
// long-lived goroutine in the gateway (stream pumps, the archive writer, probe
// workers, the cleanup ticker) therefore goes through this package, so one
// malformed upstream response degrades a single request instead of dropping
// every in-flight client.
//
// Containment is never silent: each recovered panic is logged with its stack
// under a caller-supplied label, which is what makes the failure diagnosable
// after the fact.
package safego

import (
	"fmt"
	"log"
	"runtime/debug"
)

// Go starts fn on a new goroutine and contains a panic raised inside it.
func Go(logger *log.Logger, label string, fn func()) {
	go Run(logger, label, fn)
}

// Run executes fn on the calling goroutine and contains a panic raised inside
// it. Use it for the body of a worker that already owns its goroutine, so a
// single bad item does not kill the whole loop.
func Run(logger *log.Logger, label string, fn func()) {
	defer func() {
		if recovered := recover(); recovered != nil {
			LogPanic(logger, label, recovered)
		}
	}()
	fn()
}

// GoErr starts fn on a new goroutine and returns a buffered channel that
// receives exactly one value: fn's error, or the panic converted into an
// error. Callers that block on that channel therefore cannot deadlock when fn
// panics — a requirement for stream pumps, where the request goroutine waits
// on the result while draining the pipe fn writes into.
func GoErr(logger *log.Logger, label string, fn func() error) <-chan error {
	results := make(chan error, 1)
	go func() {
		var err error
		defer func() {
			if recovered := recover(); recovered != nil {
				err = panicError(logger, label, recovered)
			}
			results <- err
		}()
		err = fn()
	}()
	return results
}

// LogPanic records a recovered panic together with the stack that produced it.
// Workers that must publish a terminal state before unwinding (a failed install
// job, a login waiter that has to close its channel) recover locally and call
// this instead of relying on Go/Run.
func LogPanic(logger *log.Logger, label string, recovered any) {
	if logger == nil {
		// Containment must never be silent. Callers that own no logger (the
		// metrics collector, for instance) still get the stack on stderr.
		logger = log.Default()
	}
	logger.Printf("panic contained in %s: %v\n%s", label, recovered, debug.Stack())
}

// panicError logs the panic and converts it into an error for GoErr callers.
func panicError(logger *log.Logger, label string, recovered any) error {
	LogPanic(logger, label, recovered)
	return fmt.Errorf("%s panicked: %v", label, recovered)
}
