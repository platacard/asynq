package asynq

import (
	"sync"
	"time"

	"github.com/platacard/asynq/internal/base"
)

type ondemandProcessor struct {
	*processor
}

func newOndemandProcessor(p *processor) *ondemandProcessor {
	return &ondemandProcessor{p}
}

// start is a no-op for ondemandProcessor since we don't want to start an internal Processor worker
// which constantly reads from the queue
func (p *ondemandProcessor) start(wg *sync.WaitGroup) {}

// stop is a copy of processor.stop but without closing of done channel as ondemandProcessor does not read from it
func (p *ondemandProcessor) stop() {
	p.once.Do(func() {
		p.logger.Debug("OndemandProcessor shutting down...")
		// Unblock if processor is waiting for sema token.
		close(p.quit)
	})
}

// shutdown is a copy of processor.shutdown calling the ondemandProcessor.stop
func (p *ondemandProcessor) shutdown() {
	p.stop()

	time.AfterFunc(p.shutdownTimeout, func() { close(p.abort) })

	p.logger.Info("Waiting for all workers to finish...")
	// block until all workers have released the token
	for i := 0; i < cap(p.sema); i++ {
		p.sema <- struct{}{}
	}
	p.logger.Info("All workers have finished")
}

func (p *ondemandProcessor) setBroker(broker base.Broker) {
	p.broker = broker
}
