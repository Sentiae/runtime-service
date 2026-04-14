package usecase

import (
	"context"
	"log"
	"time"
)

// GraphPendingProcessor is an interface for processing pending graph executions.
// This avoids a circular dependency between the processor and the engine.
type GraphPendingProcessor interface {
	ProcessPendingGraphs(ctx context.Context, limit int) (int, error)
}

// PendingProcessor runs a background loop that processes pending single
// executions and pending graph executions on a configurable interval.
type PendingProcessor struct {
	executionUC ExecutionUseCase
	graphProc   GraphPendingProcessor
	interval    time.Duration
	batchSize   int
	stopCh      chan struct{}
}

// NewPendingProcessor creates a new PendingProcessor
func NewPendingProcessor(
	executionUC ExecutionUseCase,
	graphProc GraphPendingProcessor,
	interval time.Duration,
	batchSize int,
) *PendingProcessor {
	return &PendingProcessor{
		executionUC: executionUC,
		graphProc:   graphProc,
		interval:    interval,
		batchSize:   batchSize,
		stopCh:      make(chan struct{}),
	}
}

// Start begins the background processing loop
func (p *PendingProcessor) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(p.interval)
		defer ticker.Stop()

		log.Printf("Pending processor started (interval=%s, batch=%d)", p.interval, p.batchSize)

		for {
			select {
			case <-ctx.Done():
				log.Println("Pending processor stopped (context cancelled)")
				return
			case <-p.stopCh:
				log.Println("Pending processor stopped")
				return
			case <-ticker.C:
				p.processBatch(ctx)
			}
		}
	}()
}

// Stop signals the processor to stop
func (p *PendingProcessor) Stop() {
	select {
	case <-p.stopCh:
	default:
		close(p.stopCh)
	}
}

func (p *PendingProcessor) processBatch(ctx context.Context) {
	// Process pending single executions
	if n, err := p.executionUC.ProcessPending(ctx, p.batchSize); err != nil {
		log.Printf("Warning: pending execution processing error: %v", err)
	} else if n > 0 {
		log.Printf("Processed %d pending executions", n)
	}

	// Process pending graph executions
	if p.graphProc != nil {
		if n, err := p.graphProc.ProcessPendingGraphs(ctx, p.batchSize); err != nil {
			log.Printf("Warning: pending graph execution processing error: %v", err)
		} else if n > 0 {
			log.Printf("Processed %d pending graph executions", n)
		}
	}
}
