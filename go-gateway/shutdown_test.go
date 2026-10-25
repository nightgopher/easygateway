package main

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nightgopher/easygateway/internal/archive"
	"github.com/nightgopher/easygateway/internal/metrics"
)

func TestGatewayShutdownDrainsAcceptedRequest(t *testing.T) {
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		io.WriteString(w, "complete")
		close(finished)
	})}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serveGateway(ctx, server, func() error { return server.Serve(listener) }, time.Second) }()
	go func() {
		resp, err := http.Get("http://" + listener.Addr().String())
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		t.Fatalf("server returned with request still active: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
	default:
		t.Fatal("handler not finished before server lifecycle ended")
	}
}

func TestGatewayShutdownTimeoutCancelsConnection(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(canceled)
	})}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- serveGateway(ctx, server, func() error { return server.Serve(listener) }, 20*time.Millisecond)
	}()
	go func() {
		resp, err := http.Get("http://" + listener.Addr().String())
		if err == nil {
			resp.Body.Close()
		}
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown err=%v", err)
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("timed out connection was not canceled")
	}
}

func TestBoundedCleanupDoesNotHangExit(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := boundedCleanup(ctx, func() error { <-release; return nil }); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("cleanup err=%v", err)
	}
}

type shutdownRecordStore struct {
	started, release chan struct{}
	closed           atomic.Bool
	saved            atomic.Bool
}

func (s *shutdownRecordStore) SaveRecord(metrics.RequestRecord) error {
	close(s.started)
	<-s.release
	s.saved.Store(true)
	return nil
}
func (s *shutdownRecordStore) GetRecentLogs(int) ([]metrics.RequestRecord, error) { return nil, nil }
func (s *shutdownRecordStore) Close() error                                       { s.closed.Store(true); return nil }

func TestGatewayShutdownPersistsFinalMetricsBeforeStoreClose(t *testing.T) {
	store := &shutdownRecordStore{started: make(chan struct{}), release: make(chan struct{})}
	collector := metrics.NewCollector()
	collector.SetStore(store)
	archiver := archive.NewArchiver(nil, archive.Config{}, nil)
	collector.Record(metrics.RequestRecord{Timestamp: time.Now(), Method: http.MethodPost, Path: "/v1/messages", StatusCode: 200})
	<-store.started
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		closeGatewayPersistence(ctx, collector, archiver, store, log.New(io.Discard, "", 0))
		close(done)
	}()
	select {
	case <-done:
		close(store.release)
		t.Fatal("cleanup returned before final metric persisted")
	case <-time.After(20 * time.Millisecond):
	}
	if store.closed.Load() {
		close(store.release)
		t.Fatal("database closed while metrics write was active")
	}
	close(store.release)
	<-done
	if !store.saved.Load() || !store.closed.Load() {
		t.Fatalf("saved=%t closed=%t", store.saved.Load(), store.closed.Load())
	}
}
