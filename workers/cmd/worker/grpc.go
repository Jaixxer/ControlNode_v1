package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"jaiveer/ControlPlane/pkg/cluster"
	deploypkg "jaiveer/ControlPlane/pkg/deploy"
	pb "jaiveer/ControlPlane/pkg/proto"
	workers_docker "jaiveer/ControlPlane/workers/docker"
	workers_pki "jaiveer/ControlPlane/workers/pki"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// heartbeatInterval is how often the worker tells the control plane it is alive.
// It is shared with the control plane so the two cannot disagree about when a
// worker should be considered gone.
const heartbeatInterval = cluster.HeartbeatInterval

// GrpcClient holds the worker's connection state. Every outbound message goes
// through send so that only one goroutine ever writes to the stream, since
// concurrent Send calls on the same gRPC stream are not safe.
type GrpcClient struct {
	// name is this worker's name, used to scope the compose projects it runs.
	name string
	// projectsDir is where projects pushed by the control plane are unpacked.
	projectsDir string
	send        chan *pb.RegisterWorkerRequest

	mu      sync.Mutex
	pending map[string]*pendingDeploy
	// rejected holds deploys we refused (the project already exists); the rest
	// of their chunks are discarded.
	rejected map[string]bool
}

// pendingDeploy is a project arriving in chunks from the control plane.
type pendingDeploy struct {
	project       string
	source        string
	path          string
	next          uint32
	file          *os.File
	force         bool
	hostPort      uint32
	containerPort uint32
}

// reportStatus tells the control plane how a deploy is going. url is only set
// once the project is running.
func (s *GrpcClient) reportStatus(deployID, project, state, message, url string) {
	slog.Info("Reporting deploy status", "project", project, "deploy", deployID, "state", state)
	select {
	case s.send <- &pb.RegisterWorkerRequest{Status: &pb.DeployStatus{
		DeployId: deployID,
		Project:  project,
		State:    state,
		Message:  message,
		Url:      url,
	}}:
	case <-time.After(5 * time.Second):
		slog.Warn("Could not report deploy status", "project", project, "deploy", deployID, "state", state)
	}
}

// InitGrpcClient connects to the control plane, registers this worker and then
// serves deploy commands until ctx is cancelled or the stream closes.
func (s *GrpcClient) InitGrpcClient(ctx context.Context, certManager workers_pki.CertificateManager, configPath string) error {
	s.name = certManager.Name()
	s.projectsDir = filepath.Join(configPath, "projects")
	s.send = make(chan *pb.RegisterWorkerRequest, 8)
	s.pending = make(map[string]*pendingDeploy)
	s.rejected = make(map[string]bool)

	client, err := tls.LoadX509KeyPair(
		filepath.Join(configPath, "worker.crt"),
		filepath.Join(configPath, "worker.key"),
	)
	if err != nil {
		return err
	}
	caCert, err := os.ReadFile(filepath.Join(configPath, "ca.crt"))
	if err != nil {
		return err
	}
	caCertPool := x509.NewCertPool()
	if !caCertPool.AppendCertsFromPEM(caCert) {
		return errors.New("Unable to append the PEM encoded cert to the cert pool")
	}
	tlsConfig := &tls.Config{
		Certificates: []tls.Certificate{client},
		RootCAs:      caCertPool,
	}
	creds := credentials.NewTLS(tlsConfig)

	conn, err := grpc.NewClient("localhost:50051", grpc.WithTransportCredentials(creds))
	if err != nil {
		return fmt.Errorf("failed to connect: %w", err)
	}
	defer conn.Close()

	echoClient := pb.NewEchoServiceClient(conn)
	echoCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	resp, err := echoClient.Echo(echoCtx, &pb.EchoRequest{Message: "mTlS workign?"})
	if err != nil {
		return err
	}
	slog.Info("Echo response", "message", resp.Message)

	return s.register(ctx, conn, certManager.Name())
}

// register opens the long lived bidi stream and handles what the control plane
// pushes down it.
func (s *GrpcClient) register(ctx context.Context, conn *grpc.ClientConn, name string) error {
	stream, err := pb.NewRegisterWorkerClient(conn).Register(ctx)
	if err != nil {
		return err
	}

	// The control plane reads the worker name off the first message.
	if err := stream.Send(&pb.RegisterWorkerRequest{Name: name}); err != nil {
		return err
	}
	slog.Info("Registered with the control plane", "name", name)

	recvErr := make(chan error, 1)
	go func() {
		for {
			msg, err := stream.Recv()
			if err != nil {
				recvErr <- err
				return
			}
			if cmd := msg.GetDeploy(); cmd != nil {
				s.acceptChunk(ctx, cmd)
				continue
			}
			slog.Info("Message from control plane", "task", msg.GetTask())
		}
	}()

	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	heartbeat := 1
	for {
		select {
		case err := <-recvErr:
			if err == io.EOF {
				slog.Info("Control plane closed the stream")
				return nil
			}
			return err

		case out := <-s.send:
			if err := stream.Send(out); err != nil {
				return err
			}

		case <-ticker.C:
			if err := stream.Send(&pb.RegisterWorkerRequest{Name: name, Heartbeat: int32(heartbeat)}); err != nil {
				return err
			}
			heartbeat++

		case <-ctx.Done():
			_ = stream.CloseSend()
			return ctx.Err()
		}
	}
}

// projectDir is where a named project lives on this worker.
func (s *GrpcClient) projectDir(project string) string {
	return filepath.Join(s.projectsDir, project)
}

// alreadyDeployed reports the error to send back when a project is being
// deployed over an existing copy without --force.
func (s *GrpcClient) alreadyDeployed(project string, force bool) error {
	if force {
		return nil
	}
	if _, err := os.Stat(s.projectDir(project)); err == nil {
		return fmt.Errorf("project %q is already deployed on worker %q; re-run with --force to rebuild it",
			project, s.name)
	}
	return nil
}

// acceptChunk appends one chunk of an incoming project to its temp file. The
// final chunk hands the assembled archive to buildProject.
func (s *GrpcClient) acceptChunk(ctx context.Context, cmd *pb.DeployCommand) {
	id := cmd.GetDeployId()

	s.mu.Lock()

	// A deploy we already refused: discard the rest of its chunks.
	if s.rejected[id] {
		if cmd.GetLast() {
			delete(s.rejected, id)
		}
		s.mu.Unlock()
		return
	}

	p, ok := s.pending[id]
	if !ok {
		// First chunk. This is the earliest point the project name is known, so
		// it is where the "already deployed" check belongs: refusing here avoids
		// building, and the control plane stops streaming to us as soon as it
		// sees the status.
		if err := s.alreadyDeployed(cmd.GetProject(), cmd.GetForce()); err != nil {
			s.rejected[id] = true
			s.mu.Unlock()
			slog.Warn("Refusing deploy", "project", cmd.GetProject(), "reason", err)
			s.reportStatus(id, cmd.GetProject(), "exists", err.Error(), "")
			return
		}

		path := filepath.Join(os.TempDir(), "cplane-"+id+".tar.gz")
		f, err := os.Create(path)
		if err != nil {
			s.mu.Unlock()
			slog.Error("Could not create a file for the incoming project", "error", err)
			return
		}
		p = &pendingDeploy{
			project:       cmd.GetProject(),
			source:        cmd.GetSource(),
			path:          path,
			file:          f,
			force:         cmd.GetForce(),
			hostPort:      cmd.GetHostPort(),
			containerPort: cmd.GetContainerPort(),
		}
		s.pending[id] = p
	}
	if cmd.GetChunkIndex() != p.next {
		s.mu.Unlock()
		slog.Warn("Deploy chunk arrived out of order",
			"project", p.project, "expected", p.next, "got", cmd.GetChunkIndex())
		return
	}
	p.next++

	if len(cmd.GetChunk()) > 0 {
		if _, err := p.file.Write(cmd.GetChunk()); err != nil {
			s.mu.Unlock()
			slog.Error("Could not write a deploy chunk", "project", p.project, "error", err)
			return
		}
	}
	if !cmd.GetLast() {
		s.mu.Unlock()
		return
	}

	delete(s.pending, id)
	s.mu.Unlock()

	if err := p.file.Close(); err != nil {
		slog.Error("Could not finish the incoming project file", "project", p.project, "error", err)
	}

	s.reportStatus(id, p.project, "received", p.source, "")

	// Building takes a while, so keep the receive loop free.
	go func() {
		if err := s.buildProject(ctx, id, p); err != nil {
			slog.Error("Deploy failed", "project", p.project, "error", err)
			s.reportStatus(id, p.project, "failed", err.Error(), "")
			return
		}
	}()

	return
}

// buildProject unpacks an assembled archive and brings it up with docker
// compose, reporting progress and the resulting URL as it goes.
func (s *GrpcClient) buildProject(ctx context.Context, deployID string, p *pendingDeploy) error {
	slog.Info("Received project", "project", p.project, "source", p.source, "file", p.path)

	// Everything the build prints is forwarded to the control plane so the
	// operator can see it. Detached from the stream so a burst of output cannot
	// slow the build down.
	pump := newLogPump(s.send, deployID, p.project)
	defer pump.Close()

	fmt.Fprintf(pump, "received %s (%s)\n", p.project, p.source)

	f, err := os.Open(p.path)
	if err != nil {
		return err
	}
	defer func() {
		f.Close()
		os.Remove(p.path)
	}()

	// Replace any previous copy so the deployment matches what was sent.
	dir := s.projectDir(p.project)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	if err := deploypkg.UnpackTarGz(f, dir, false); err != nil {
		return fmt.Errorf("unpacking project: %w", err)
	}

	// A GitHub tarball arrives wrapped in a "owner-repo-sha" directory, so find
	// the directory that actually holds the project.
	root, err := deploypkg.ResolveProjectRoot(dir)
	if err != nil {
		return err
	}
	slog.Info("Project staged", "project", p.project, "dir", root)

	// The control plane normally generates these, but a project can also be
	// pushed by hand, so make sure the worker can still build it.
	prepared, err := deploypkg.EnsureBuildFiles(root, p.project, int(p.containerPort), int(p.hostPort))
	if err != nil {
		return err
	}
	if len(prepared.Created) > 0 {
		fmt.Fprintf(pump, "generated %s\n", strings.Join(prepared.Created, ", "))
	}
	for _, note := range prepared.Notes {
		fmt.Fprintf(pump, "note: %s\n", note)
	}
	if _, err := os.Stat(filepath.Join(root, deploypkg.ComposeFileName)); err != nil {
		return fmt.Errorf("project %s has no %s: %w", p.project, deploypkg.ComposeFileName, err)
	}

	s.reportStatus(deployID, p.project, "building", "building and starting containers", "")

	buildCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	composeProject := deploypkg.SanitizeName(p.project + "-" + s.name)
	slog.Info("Starting project with docker compose", "project", p.project, "compose", composeProject)
	fmt.Fprintf(pump, "docker compose -p %s up --build -d\n", composeProject)

	ports, err := workers_docker.ComposeUp(buildCtx, root, composeProject, pump)
	if err != nil {
		return err
	}

	// Detached mode reports success as soon as containers start, so check that
	// they are actually still up. Without this, an app that crashes on startup
	// looks like a successful deploy.
	if err := s.checkContainersRunning(buildCtx, root, composeProject, pump); err != nil {
		return err
	}

	urls := s.urlsFor(ports)
	if len(urls) == 0 {
		fmt.Fprintf(pump, "project is running but publishes no ports, so there is no URL to open\n")
	} else {
		for _, u := range urls {
			fmt.Fprintf(pump, "listening on %s\n", u)
		}
	}

	// Close exactly here so the drop count is final before it is reported; the
	// deferred Close is then a no-op.
	dropped := pump.Close()
	message := "deployed"
	if dropped > 0 {
		message = fmt.Sprintf("deployed (%d log lines dropped)", dropped)
	}
	s.reportStatus(deployID, p.project, "done", message, strings.Join(urls, " "))
	return nil
}

// startupSettle is how long to watch new containers before deciding the deploy
// worked. A compose restart policy will keep a crashing app "running", so the
// only reliable signal is whether it stays up, which takes a moment to show.
const startupSettle = 3 * time.Second

// checkContainersRunning fails the deploy when the app does not stay up,
// attaching the container logs so the reason is visible.
func (s *GrpcClient) checkContainersRunning(ctx context.Context, dir, composeProject string, pump *logPump) error {
	statuses, err := workers_docker.ContainerStatuses(ctx, dir, composeProject)
	if err != nil {
		// Not fatal: the containers may well be fine, we just could not confirm.
		fmt.Fprintf(pump, "could not confirm container state: %v\n", err)
		return nil
	}
	if len(statuses) == 0 {
		return errors.New("no containers were created for the project")
	}

	names := make([]string, 0, len(statuses))
	for _, st := range statuses {
		names = append(names, st.Name)
	}

	// Give a crash-looping app time to reveal itself.
	select {
	case <-time.After(startupSettle):
	case <-ctx.Done():
		return ctx.Err()
	}

	health, err := workers_docker.InspectContainers(ctx, names)
	if err != nil {
		fmt.Fprintf(pump, "could not inspect containers: %v\n", err)
		return nil
	}

	var broken []string
	healthy := 0
	for _, h := range health {
		switch {
		case h.RestartCount > 0:
			broken = append(broken, fmt.Sprintf("%s (restarted %d times, exit %d)", h.Name, h.RestartCount, h.ExitCode))
		case !h.Running:
			broken = append(broken, fmt.Sprintf("%s (not running, exit %d)", h.Name, h.ExitCode))
		default:
			healthy++
		}
	}

	if healthy > 0 && len(broken) == 0 {
		return nil
	}
	if healthy > 0 {
		fmt.Fprintf(pump, "warning: these containers are not healthy: %s\n", strings.Join(broken, ", "))
		return nil
	}

	// Nothing is up: the app's own output usually explains why.
	if logs, err := workers_docker.ContainerLogs(ctx, dir, composeProject, 30); err == nil && strings.TrimSpace(logs) != "" {
		fmt.Fprintf(pump, "container logs:\n%s\n", logs)
	}
	return fmt.Errorf("the app did not stay up: %s", strings.Join(broken, ", "))
}

// urlsFor turns the ports Docker actually published into URLs, using this
// worker's address.
func (s *GrpcClient) urlsFor(ports []workers_docker.PublishedPort) []string {
	host := localIP()
	var urls []string
	seen := map[int]bool{}
	for _, p := range ports {
		// UDP has no URL, and two containers on one host port is a clash.
		if p.Protocol != "tcp" || p.HostPort == 0 || seen[p.HostPort] {
			continue
		}
		seen[p.HostPort] = true
		urls = append(urls, fmt.Sprintf("http://%s:%d", host, p.HostPort))
	}
	return urls
}

// localIP is the address this worker is reachable on. A non-loopback IPv4 is
// preferred so the URL works from another machine; localhost is the fallback
// when the host has no external interface.
func localIP() string {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "127.0.0.1"
	}
	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() {
			continue
		}
		if ip4 := ipNet.IP.To4(); ip4 != nil {
			return ip4.String()
		}
	}
	return "127.0.0.1"
}
