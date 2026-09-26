package main

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"sync"
	"time"

	pb "jaiveer/ControlPlane/pkg/proto"

	"google.golang.org/grpc"
)

// ErrNoWorkers is returned when a deploy is requested but no worker is
// connected to receive it.
var ErrNoWorkers = errors.New("no workers are currently connected")

// chunkSize is how much of an archive is pushed per message. gRPC's default max
// message size is 4MiB, so this leaves plenty of headroom.
const chunkSize = 512 * 1024

// sendTimeout bounds how long we wait for a worker to accept one message.
const sendTimeout = 30 * time.Second

// workerConn is one connected worker. Commands are queued on send and written to
// the gRPC stream by the single goroutine that owns it, because concurrent
// Send calls on the same stream are not safe.
type workerConn struct {
	name string
	send chan *pb.RegisterWorkerResponse
	// closed is closed to force the stream shut, e.g. when the sweeper decides
	// the worker has gone silent. shutdown is safe to call more than once.
	closed       chan struct{}
	shutdownOnce sync.Once
}

// shutdown tells registerStream to give up on this worker.
func (c *workerConn) shutdown() {
	c.shutdownOnce.Do(func() { close(c.closed) })
}

// workerRegistry tracks the workers currently attached to the control plane.
type workerRegistry struct {
	mu      sync.RWMutex
	workers map[string]*workerConn
}

func newWorkerRegistry() *workerRegistry {
	return &workerRegistry{workers: make(map[string]*workerConn)}
}

func (r *workerRegistry) add(conn *workerConn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.workers[conn.name] = conn
}

func (r *workerRegistry) remove(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.workers, name)
}

// targets resolves a list of worker names into connections. An empty list means
// every connected worker, which is the default for deploys.
func (r *workerRegistry) targets(names []string) ([]*workerConn, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if len(r.workers) == 0 {
		return nil, ErrNoWorkers
	}

	if len(names) == 0 {
		out := make([]*workerConn, 0, len(r.workers))
		for _, conn := range r.workers {
			out = append(out, conn)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
		return out, nil
	}

	out := make([]*workerConn, 0, len(names))
	var missing []string
	for _, name := range names {
		conn, ok := r.workers[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		out = append(out, conn)
	}
	if len(missing) > 0 {
		return out, fmt.Errorf("workers not connected: %v (connected: %v)", missing, r.namesLocked())
	}
	return out, nil
}

// get returns the connection for a worker if it is currently attached.
func (r *workerRegistry) get(name string) (*workerConn, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	conn, ok := r.workers[name]
	return conn, ok
}

// names lists the connected worker names, for logging and error messages.
func (r *workerRegistry) names() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.namesLocked()
}

func (r *workerRegistry) namesLocked() []string {
	out := make([]string, 0, len(r.workers))
	for name := range r.workers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// deployTracker remembers the latest state each worker reported for a deploy so
// callers can see how it went.
//
// Statuses are held as pointers because protobuf messages embed a mutex and must
// not be copied.
type deployTracker struct {
	mu     sync.Mutex
	states map[string]*pb.DeployStatus
}

func newDeployTracker() *deployTracker {
	return &deployTracker{states: make(map[string]*pb.DeployStatus)}
}

func (t *deployTracker) set(s *pb.DeployStatus) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.states[s.GetDeployId()] = s
}

func (t *deployTracker) get(deployID string) (*pb.DeployStatus, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.states[deployID]
	return s, ok
}

// DeployResult reports which workers accepted a deploy and which did not.
type DeployResult struct {
	Sent   []string
	Failed map[string]error
}

// DeployStream streams a tar.gz from r down to the given workers, or to every
// connected worker when names is empty.
//
// The archive is read once and fanned out chunk by chunk, so the control plane
// never buffers the whole project and never needs it on disk. A worker that
// cannot keep up is dropped from the fan-out rather than stalling the others.
func (s *GrpcServer) DeployStream(deployID, project, source string, r io.Reader, names []string) (*DeployResult, error) {
	targets, err := s.Workers.targets(names)
	if err != nil && len(targets) == 0 {
		return nil, err
	}

	result := &DeployResult{Failed: map[string]error{}}
	if err != nil {
		// Some requested workers are not connected; carry on with the ones that
		// are, but surface the problem.
		slog.Warn("Deploying to a partial set of workers", "error", err)
		result.Failed["_requested"] = err
	}

	alive := make([]*workerConn, len(targets))
	copy(alive, targets)

	buf := make([]byte, chunkSize)
	var index uint32

	for {
		n, readErr := r.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])

			alive = s.fanOutChunk(alive, result, &pb.DeployCommand{
				DeployId:   deployID,
				Project:    project,
				Source:     source,
				Chunk:      chunk,
				ChunkIndex: index,
			})
			index++
		}

		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return result, fmt.Errorf("reading archive: %w", readErr)
		}
	}

	// A final empty chunk marks the end of the deploy.
	s.fanOutChunk(alive, result, &pb.DeployCommand{
		DeployId:   deployID,
		Project:    project,
		Source:     source,
		ChunkIndex: index,
		Last:       true,
	})

	for _, conn := range targets {
		if _, failed := result.Failed[conn.name]; !failed {
			result.Sent = append(result.Sent, conn.name)
		}
	}
	sort.Strings(result.Sent)

	if len(result.Sent) == 0 {
		return result, fmt.Errorf("no worker accepted the deploy")
	}
	return result, nil
}

// fanOutChunk sends one deploy chunk to every live worker, dropping any that
// fail.
func (s *GrpcServer) fanOutChunk(conns []*workerConn, result *DeployResult, cmd *pb.DeployCommand) []*workerConn {
	msg := &pb.RegisterWorkerResponse{Success: true, Deploy: cmd}

	remaining := conns[:0]
	for _, conn := range conns {
		select {
		case conn.send <- msg:
			remaining = append(remaining, conn)
		case <-time.After(sendTimeout):
			slog.Warn("Worker did not accept a deploy chunk", "worker", conn.name)
			result.Failed[conn.name] = fmt.Errorf("worker %q did not accept the deploy in time", conn.name)
		}
	}
	return remaining
}

// registerStream owns a worker's gRPC stream: it reads heartbeats in the
// background and is the only writer on the stream.
func (s *GrpcServer) registerStream(stream grpc.BidiStreamingServer[pb.RegisterWorkerRequest, pb.RegisterWorkerResponse]) error {
	first, err := stream.Recv()
	if err != nil {
		return err
	}

	conn := &workerConn{
		name:   first.Name,
		send:   make(chan *pb.RegisterWorkerResponse, 8),
		closed: make(chan struct{}),
	}
	s.Workers.add(conn)
	defer func() {
		s.Workers.remove(conn.name)
		s.Store.markOffline(conn.name, "disconnected")
	}()
	s.Store.markOnline(conn.name)
	slog.Info("Worker connected", "name", first.Name, "connected", len(s.Workers.names()))

	recvDone := make(chan error, 1)
	go func() {
		for {
			req, err := stream.Recv()
			if err != nil {
				recvDone <- err
				return
			}
			if status := req.GetStatus(); status != nil {
				slog.Info("Deploy status",
					"worker", conn.name,
					"project", status.GetProject(),
					"deploy", status.GetDeployId(),
					"state", status.GetState(),
					"message", status.GetMessage(),
				)
				s.Deploys.set(status)
				continue
			}
			// Heartbeats only prove liveness; log them at debug level so the
			// daemon output stays readable, but always stamp last seen.
			s.Store.touch(conn.name)
			slog.Debug("Worker heartbeat", "name", req.Name, "heartbeat", req.Heartbeat)
		}
	}()

	for {
		select {
		case err := <-recvDone:
			if err == io.EOF {
				slog.Info("Worker disconnected", "name", conn.name)
				return nil
			}
			slog.Warn("Worker stream ended", "name", conn.name, "error", err)
			return err

		case resp := <-conn.send:
			if err := stream.Send(resp); err != nil {
				return err
			}

		case <-conn.closed:
			slog.Warn("Shutting down a worker that stopped reporting in", "name", conn.name)
			return fmt.Errorf("worker %q missed its heartbeats", conn.name)

		case <-stream.Context().Done():
			slog.Info("Worker context done", "name", conn.name)
			return stream.Context().Err()
		}
	}
}
