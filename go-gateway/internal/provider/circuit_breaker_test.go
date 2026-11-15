package provider

import (
	"testing"
	"time"
)

func TestCircuitBreakerTransitionsThroughOpenAndHalfOpen(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	breaker := NewCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 2,
		RecoveryTimeout:  10 * time.Second,
		SuccessThreshold: 1,
	})

	if !breaker.AllowAt(start) {
		t.Fatal("closed circuit should allow requests")
	}
	breaker.RecordFailureAt(start)
	if breaker.SnapshotAt(start).State != CircuitClosed {
		t.Fatal("circuit should remain closed before failure threshold")
	}
	breaker.RecordFailureAt(start.Add(time.Second))
	if snapshot := breaker.SnapshotAt(start.Add(time.Second)); snapshot.State != CircuitOpen {
		t.Fatalf("state = %s, want open", snapshot.State)
	}
	if breaker.AllowAt(start.Add(5 * time.Second)) {
		t.Fatal("open circuit should reject requests before recovery timeout")
	}
	if !breaker.AllowAt(start.Add(11 * time.Second)) {
		t.Fatal("circuit should allow one half-open probe after timeout")
	}
	if breaker.AllowAt(start.Add(11 * time.Second)) {
		t.Fatal("only one half-open probe should be in flight")
	}
	breaker.RecordSuccessAt(start.Add(12 * time.Second))
	if snapshot := breaker.SnapshotAt(start.Add(12 * time.Second)); snapshot.State != CircuitClosed {
		t.Fatalf("state = %s, want closed after successful probe", snapshot.State)
	}
}

func TestCircuitBreakerHalfOpenFailureReopensCircuit(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	breaker := NewCircuitBreaker(CircuitBreakerConfig{
		FailureThreshold: 1,
		RecoveryTimeout:  5 * time.Second,
		SuccessThreshold: 1,
	})

	breaker.RecordFailureAt(start)
	if !breaker.AllowAt(start.Add(5 * time.Second)) {
		t.Fatal("expected half-open probe to be allowed")
	}
	breaker.RecordFailureAt(start.Add(5 * time.Second))
	if snapshot := breaker.SnapshotAt(start.Add(5 * time.Second)); snapshot.State != CircuitOpen {
		t.Fatalf("state = %s, want open after failed probe", snapshot.State)
	}
}
