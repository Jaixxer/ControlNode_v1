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

// maxLogLines caps how much deploy output is retained per deploy. A noisy build
// should not be able to grow the daemon's memory without bound; the oldest lines
// are dropped once the cap is reached.
const maxLogLines = 2000

// maxTrackedDeploys caps how many deploys are remembered, so the tracker cannot
// leak. Old attempts are pruned when a new deploy starts.
const maxTrackedDeploys = 200

// workerDeployState is how one worker is getting on with a deploy.
type workerDeployState struct {
	State   string
	Message string
	URL     string
	Updated time.Time
}

// deployLogLine is one line of output attributed to the worker that produced it.
type deployLogLine struct {
	Worker string
	Line   string
}

// deployAttempt is one deploy, fanned out across a set of workers.
//
// State is tracked per worker rather than per deploy: with a fan-out to several
// workers one can fail while another succeeds, and reporting only the last
// status to arrive would hide that.
type deployAttempt struct {
	DeployID string
	Project  string
	Source   string
	Targets  []string
	States   map[string]*workerDeployState
	Logs     []deployLogLine
	Created  time.Time
}

// terminalStates are the states that mean a worker is finished with a deploy.
var terminalStates = map[string]bool{
	"done":   true,
	"failed": true,
	"exists": true,
}

// deployTracker records the progress of deploys so the CLI can follow them.
type deployTracker struct {
	mu       sync.Mutex
	attempts map[string]*deployAttempt
}

func newDeployTracker() *deployTracker {
	return &deployTracker{attempts: make(map[string]*deployAttempt)}
}

// create registers a new deploy and prunes old attempts.
func (t *deployTracker) create(deployID, project, source string, targets []string) {
	t.mu.Lock()
	defer t.mu.Unlock()

	t.pruneLocked()

	states := make(map[string]*workerDeployState, len(targets))
	for _, name := range targets {
		states[name] = &workerDeployState{State: "pending", Updated: time.Now()}
	}
	t.attempts[deployID] = &deployAttempt{
		DeployID: deployID,
		Project:  project,
		Source:   source,
		Targets:  targets,
		States:   states,
		Created:  time.Now(),
	}
}

// pruneLocked drops attempts older than an hour, and the oldest ones beyond the
// cap. It assumes the lock is held.
func (t *deployTracker) pruneLocked() {
	cutoff := time.Now().Add(-time.Hour)
	for id, a := range t.attempts {
		if a.Created.Before(cutoff) {
			delete(t.attempts, id)
		}
	}
	for len(t.attempts) >= maxTrackedDeploys {
		var oldestID string
		var oldest time.Time
		for id, a := range t.attempts {
			if oldestID == "" || a.Created.Before(oldest) {
				oldestID, oldest = id, a.Created
			}
		}
		delete(t.attempts, oldestID)
	}
}

// setStatus records a worker's state for its deploy.
func (t *deployTracker) setStatus(worker string, s *pb.DeployStatus) {
	t.mu.Lock()
	defer t.mu.Unlock()

	a, ok := t.attempts[s.GetDeployId()]
	if !ok {
		return
	}
	st, ok := a.States[worker]
	if !ok {
		st = &workerDeployState{}
		a.States[worker] = st
	}
	st.State = s.GetState()
	st.Message = s.GetMessage()
	if s.GetUrl() != "" {
		st.URL = s.GetUrl()
	}
	st.Updated = time.Now()
}

// addLog appends a line of deploy output.
func (t *deployTracker) addLog(worker string, l *pb.DeployLog) {
	t.mu.Lock()
	defer t.mu.Unlock()

	a, ok := t.attempts[l.GetDeployId()]
	if !ok {
		return
	}
	a.Logs = append(a.Logs, deployLogLine{Worker: worker, Line: l.GetLine()})
	if len(a.Logs) > maxLogLines {
		a.Logs = a.Logs[len(a.Logs)-maxLogLines:]
	}
}

// failedTargets returns the workers that will never accept the archive being
// streamed, so the fan-out can stop wasting bandwidth on them.
func (t *deployTracker) failedTargets(deployID string) map[string]bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	a, ok := t.attempts[deployID]
	if !ok {
		return nil
	}
	out := map[string]bool{}
	for name, st := range a.States {
		if st.State == "exists" || st.State == "failed" {
			out[name] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// rejectedWorkers returns the workers that refused this deploy, with the message
// each gave. It is what the API reports back so the operator learns they need
// --force.
func (t *deployTracker) rejectedWorkers(deployID string) map[string]string {
	t.mu.Lock()
	defer t.mu.Unlock()

	a, ok := t.attempts[deployID]
	if !ok {
		return nil
	}
	out := map[string]string{}
	for name, st := range a.States {
		if st.State == "exists" {
			out[name] = st.Message
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// deployWorkerView is one worker's state, as reported to the CLI.
type deployWorkerView struct {
	Name    string `json:"name"`
	State   string `json:"state"`
	Message string `json:"message,omitempty"`
	URL     string `json:"url,omitempty"`
}

// deployLogView is one line of output, as reported to the CLI.
type deployLogView struct {
	Worker string `json:"worker"`
	Line   string `json:"line"`
}

// deployView is the snapshot the CLI polls.
type deployView struct {
	DeployID string             `json:"deploy_id"`
	Project  string             `json:"project"`
	Source   string             `json:"source"`
	Done     bool               `json:"done"`
	Workers  []deployWorkerView `json:"workers"`
	Logs     []deployLogView    `json:"logs"`
	// Next is the index to pass as ?since= to get only newer log lines.
	Next int `json:"next"`
}

// view returns a snapshot of a deploy, including log lines from index since on.
func (t *deployTracker) view(deployID string, since int) (*deployView, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()

	a, ok := t.attempts[deployID]
	if !ok {
		return nil, false
	}

	if since < 0 {
		since = 0
	}
	// Clamp before sizing the slice: a caller asking for an offset past the end
	// must not produce a negative capacity.
	start := min(since, len(a.Logs))
	logs := make([]deployLogView, 0, len(a.Logs)-start)
	for _, l := range a.Logs[start:] {
		logs = append(logs, deployLogView{Worker: l.Worker, Line: l.Line})
	}

	workers := make([]deployWorkerView, 0, len(a.Targets))
	done := len(a.Targets) > 0
	for _, name := range a.Targets {
		st, ok := a.States[name]
		if !ok {
			workers = append(workers, deployWorkerView{Name: name, State: "pending"})
			done = false
			continue
		}
		workers = append(workers, deployWorkerView{
			Name:    name,
			State:   st.State,
			Message: st.Message,
			URL:     st.URL,
		})
		if !terminalStates[st.State] {
			done = false
		}
	}

	return &deployView{
		DeployID: a.DeployID,
		Project:  a.Project,
		Source:   a.Source,
		Done:     done,
		Workers:  workers,
		Logs:     logs,
		Next:     len(a.Logs),
	}, true
}

// DeployResult reports which workers accepted a deploy and which did not.
type DeployResult struct {
	Sent   []string
	Failed map[string]error
	// Rejected names workers that refused the deploy because the project already
	// exists there and --force was not given.
	Rejected map[string]string
}

// DeployOptions describes what is being deployed and how.
type DeployOptions struct {
	Project string
	Source  string
	// Force rebuilds a project that is already deployed on the worker.
	Force bool
	// HostPort pins the published port; 0 lets Docker allocate one per worker.
	HostPort uint32
	// ContainerPort is the port the app listens on inside the container.
	ContainerPort uint32
}

// DeployStream streams a tar.gz from r down to the given workers, or to every
// connected worker when names is empty.
//
// The archive is read once and fanned out chunk by chunk, so the control plane
// never buffers the whole project and never needs it on disk. A worker that
// cannot keep up, or that refuses the deploy because the project is already
// there, is dropped from the fan-out rather than stalling the others.
func (s *GrpcServer) DeployStream(deployID string, opts DeployOptions, r io.Reader, names []string) (*DeployResult, error) {
	targets, err := s.Workers.targets(names)
	if err != nil && len(targets) == 0 {
		return nil, err
	}

	result := &DeployResult{Failed: map[string]error{}, Rejected: map[string]string{}}
	if err != nil {
		// Some requested workers are not connected; carry on with the ones that
		// are, but surface the problem.
		slog.Warn("Deploying to a partial set of workers", "error", err)
		result.Failed["_requested"] = err
	}

	// Record the attempt up front so status and log updates that arrive during
	// the stream have somewhere to land.
	targetNames := make([]string, 0, len(targets))
	for _, conn := range targets {
		targetNames = append(targetNames, conn.name)
	}
	sort.Strings(targetNames)
	s.Deploys.create(deployID, opts.Project, opts.Source, targetNames)

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
				DeployId:      deployID,
				Project:       opts.Project,
				Source:        opts.Source,
				Chunk:         chunk,
				ChunkIndex:    index,
				Force:         opts.Force,
				HostPort:      opts.HostPort,
				ContainerPort: opts.ContainerPort,
			})
			index++

			// Stop wasting bandwidth on workers that already refused this
			// deploy: the "exists" check happens on the first chunk, so this
			// bites within a chunk or two for a large archive.
			if rejected := s.Deploys.failedTargets(deployID); len(rejected) > 0 {
				alive = dropConnections(alive, rejected, result)
			}
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
		DeployId:      deployID,
		Project:       opts.Project,
		Source:        opts.Source,
		ChunkIndex:    index,
		Last:          true,
		Force:         opts.Force,
		HostPort:      opts.HostPort,
		ContainerPort: opts.ContainerPort,
	})

	for _, conn := range targets {
		if _, failed := result.Failed[conn.name]; !failed {
			result.Sent = append(result.Sent, conn.name)
		}
	}
	sort.Strings(result.Sent)

	// Surface the refusals with the worker's own explanation.
	for name, reason := range s.Deploys.rejectedWorkers(deployID) {
		result.Rejected[name] = reason
	}

	if len(result.Sent) == 0 && len(result.Rejected) == 0 {
		return result, fmt.Errorf("no worker accepted the deploy")
	}
	return result, nil
}

// dropConnections removes workers from the fan-out and records why.
func dropConnections(conns []*workerConn, drop map[string]bool, result *DeployResult) []*workerConn {
	remaining := conns[:0]
	for _, conn := range conns {
		if drop[conn.name] {
			slog.Warn("Dropping worker from the fan-out", "worker", conn.name)
			result.Failed[conn.name] = errors.New("worker refused the deploy")
			continue
		}
		remaining = append(remaining, conn)
	}
	return remaining
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
				s.Deploys.setStatus(conn.name, status)
				continue
			}
			// Deploy output is buffered for the CLI to read; logging every line
			// here as well would bury the daemon's own logs.
			if line := req.GetLog(); line != nil {
				s.Deploys.addLog(conn.name, line)
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
