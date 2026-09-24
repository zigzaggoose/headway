package ingest

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Pipeline is the change filter, the bounded channel and the writer, wired
// together with the shutdown ordering §9.4 requires.
//
// The channel is the only backpressure point in Transit Late Again, and it drops rather
// than blocks: blocking would back-pressure into the poller, which would miss
// polls and lose data permanently, whereas a dropped observation is re-sent
// fifteen seconds later.
type Pipeline struct {
	ch     chan Observation
	filter *Filter
	writer *Writer
	log    *slog.Logger

	dropped atomic.Uint64

	wg           sync.WaitGroup
	cancelWriter context.CancelFunc
	closeOnce    sync.Once
}

// PipelineConfig carries the tunables from §8.
type PipelineConfig struct {
	QueueSize        int
	BatchSize        int
	FlushInterval    time.Duration
	WriteTimeout     time.Duration
	FilterMinDeltaS  int32
	FilterMaxEntries int
}

// NewPipeline builds the pipeline. Start must be called before Submit.
func NewPipeline(pool *pgxpool.Pool, cfg PipelineConfig, log *slog.Logger) *Pipeline {
	ch := make(chan Observation, cfg.QueueSize)
	return &Pipeline{
		ch:     ch,
		filter: NewFilter(cfg.FilterMinDeltaS, cfg.FilterMaxEntries),
		writer: NewWriter(pool, ch, WriterConfig{
			BatchSize:     cfg.BatchSize,
			FlushInterval: cfg.FlushInterval,
			WriteTimeout:  cfg.WriteTimeout,
		}, log),
		log: log.With("component", "ingest"),
	}
}

// Start launches the writer.
//
// Its context comes from context.Background() and not from the context that
// shutdown cancels. The writer has to outlive that cancellation to flush what
// it has already accepted; deriving it from the shutdown context is the bug
// §9.4 calls out by name.
func (p *Pipeline) Start() {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancelWriter = cancel

	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		if err := p.writer.Run(ctx); err != nil {
			p.log.Error("writer exited", "err", err.Error())
		}
	}()
}

// Submit offers one observation. It returns whether the observation was
// queued; false means it was either suppressed as unchanged or dropped
// because the queue was full, which the counters tell apart.
//
// It never blocks.
func (p *Pipeline) Submit(o Observation) bool {
	if !p.filter.Admit(o) {
		return false
	}

	select {
	case p.ch <- o:
		return true
	default:
		// The filter has already recorded this value as written. If we leave
		// that record in place, the next identical observation is suppressed
		// as unchanged and the value is lost for good rather than for one
		// poll. Forgetting the key costs one redundant write and bounds the
		// damage of a full queue to the observation actually dropped.
		p.filter.Forget(o.Key())
		p.dropped.Add(1)
		return false
	}
}

// Close performs steps 4 and 5 of the shutdown sequence: close the channel,
// then wait for the writer to drain it and flush the final partial batch.
//
// It must be called only after every producer has stopped. Closing a channel a
// producer still writes to panics, which is why §9.4 fixes the order.
func (p *Pipeline) Close(grace time.Duration) error {
	var err error
	p.closeOnce.Do(func() {
		close(p.ch)

		// Close before Start is a startup that aborted between building the
		// pipeline and launching the writer. There is nothing to wait for, and
		// waiting would block forever on a WaitGroup nobody added to.
		if p.cancelWriter == nil {
			return
		}

		done := make(chan struct{})
		go func() {
			p.wg.Wait()
			close(done)
		}()

		timer := time.NewTimer(grace)
		defer timer.Stop()

		select {
		case <-done:
		case <-timer.C:
			// The writer is stuck on a database that is not answering. Cancel
			// it so it stops waiting, and say plainly that a batch may have
			// been lost rather than reporting a clean shutdown.
			p.cancelWriter()
			<-done
			err = fmt.Errorf("writer did not finish within %s; the final batch may be incomplete", grace)
		}
	})
	return err
}

// Stats is a snapshot for /v1/admin/stats and the metrics collectors.
type Stats struct {
	QueueLen   int
	QueueCap   int
	Dropped    uint64
	Suppressed uint64
	Admitted   uint64
	Written    uint64
	Failed     uint64
	Batches    uint64
	Conflicts  uint64
	FilterSize int
}

// Stats reports what the pipeline has done. It is safe to call while the
// writer is running, which is the only time anyone wants it.
func (p *Pipeline) Stats() Stats {
	f := p.filter.Stats()
	w := p.writer.Stats()

	return Stats{
		QueueLen:   len(p.ch),
		QueueCap:   cap(p.ch),
		Dropped:    p.dropped.Load(),
		Suppressed: f.Suppressed,
		Admitted:   f.Admitted,
		Written:    w.Written,
		Failed:     w.Failed,
		Batches:    w.Batches,
		Conflicts:  w.Conflicts,
		FilterSize: f.Entries,
	}
}

// ExpireFilterBefore drops filter entries for service dates before the given
// date. The maintenance job calls this on every tick (§9.3 case 1).
func (p *Pipeline) ExpireFilterBefore(serviceDate time.Time) int {
	return p.filter.ExpireBefore(serviceDate)
}
