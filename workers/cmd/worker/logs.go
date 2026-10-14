package main

import (
	"bytes"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	pb "jaiveer/ControlPlane/pkg/proto"
)

// logPump forwards a running deploy's output to the control plane.
//
// It exists to decouple two speeds that do not match: a docker build can emit
// output far faster than the gRPC stream drains. Write never blocks on the
// network, so a slow control plane can never stall a build. When the buffer
// fills up, lines are dropped and counted rather than queued forever.
type logPump struct {
	deployID string
	project  string
	send     chan<- *pb.RegisterWorkerRequest

	ch      chan string
	dropped atomic.Int64

	mu      sync.Mutex
	partial bytes.Buffer

	closeOnce sync.Once
	wg        sync.WaitGroup
}

// sendTimeout bounds how long the pump waits for a slot on the outgoing queue
// before giving up on a line.
const sendTimeout = 2 * time.Second

// logBufferLines is how many lines may be queued before we start dropping.
const logBufferLines = 256

func newLogPump(send chan<- *pb.RegisterWorkerRequest, deployID, project string) *logPump {
	p := &logPump{
		deployID: deployID,
		project:  project,
		send:     send,
		ch:       make(chan string, logBufferLines),
	}
	p.wg.Add(1)
	go p.run()
	return p
}

func (p *logPump) run() {
	defer p.wg.Done()
	for line := range p.ch {
		req := &pb.RegisterWorkerRequest{
			Log: &pb.DeployLog{
				DeployId: p.deployID,
				Project:  p.project,
				Line:     line,
			},
		}
		select {
		case p.send <- req:
		case <-time.After(sendTimeout):
			p.dropped.Add(1)
		}
	}
}

// Write implements io.Writer. Input is buffered and emitted a line at a time so
// the control plane receives readable entries rather than arbitrary fragments.
func (p *logPump) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.partial.Write(b)
	for {
		line, err := p.partial.ReadString('\n')
		if err != nil {
			// No newline yet; keep the remainder for the next call.
			p.partial.Reset()
			p.partial.WriteString(line)
			return len(b), nil
		}
		p.emit(line)
	}
}

// emit queues one line, dropping it if the buffer is full.
func (p *logPump) emit(line string) {
	select {
	case p.ch <- line:
	default:
		p.dropped.Add(1)
	}
}

// Close flushes what is buffered and stops the pump, reporting how many lines
// were dropped. It is safe to call more than once.
func (p *logPump) Close() int64 {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		if p.partial.Len() > 0 {
			p.emit(p.partial.String())
			p.partial.Reset()
		}
		p.mu.Unlock()

		close(p.ch)
		p.wg.Wait()
	})

	if n := p.dropped.Load(); n > 0 {
		slog.Warn("Dropped deploy log lines because the control plane could not keep up",
			"project", p.project, "dropped", n)
	}
	return p.dropped.Load()
}
