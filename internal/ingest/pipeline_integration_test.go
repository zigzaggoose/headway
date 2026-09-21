//go:build integration

package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"
)

// Stats is read by /v1/admin/stats and the metrics collectors while the writer
// goroutine is running. If the counters are not synchronised, that is a data
// race in the one place an operator looks when something is wrong. It was one:
// every other test read Stats either before Start or after Close, so nothing
// caught it until a test deliberately read it mid-write.
func TestPipeline_StatsWhileWriting_IsRaceFree(t *testing.T) {
	pool := testPool(t)
	ts := time.Date(2026, 9, 21, 1, 4, 3, 0, time.UTC)

	p := newTestPipeline(t, pool, PipelineConfig{
		QueueSize: 4096, BatchSize: 50, FlushInterval: 5 * time.Millisecond,
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range 2000 {
			p.Submit(sample("trip-1", fmt.Sprintf("stop-%d", i), int32(i), ts))
		}
	}()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-done:
			_ = p.Stats()
			if err := p.Close(10 * time.Second); err != nil {
				t.Fatalf("close: %v", err)
			}
			return
		case <-deadline:
			t.Fatal("submitters did not finish")
		default:
			_ = p.Stats() // concurrent with the writer goroutine
		}
	}
}

// Close before Start is a startup that aborted between building the pipeline
// and launching the writer. It must not panic on a nil cancel func, and must
// not wait forever on a WaitGroup nobody added to.
func TestPipeline_CloseWithoutStart_ReturnsImmediately(t *testing.T) {
	pool := testPool(t)
	p := NewPipeline(pool, PipelineConfig{QueueSize: 4, BatchSize: 1, FlushInterval: time.Second}, slog.New(slog.DiscardHandler))

	done := make(chan error, 1)
	go func() { done <- p.Close(5 * time.Second) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("close: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked on a pipeline that was never started")
	}
}

// A WriterConfig built by hand with no flush interval used to panic inside
// time.NewTicker the moment the writer started.
func TestNewWriter_ZeroConfig_DoesNotPanic(t *testing.T) {
	pool := testPool(t)
	ch := make(chan Observation)
	w := NewWriter(pool, ch, WriterConfig{}, slog.New(slog.DiscardHandler))

	done := make(chan error, 1)
	go func() { done <- w.Run(context.Background()) }()

	close(ch)
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("writer did not stop")
	}
}
