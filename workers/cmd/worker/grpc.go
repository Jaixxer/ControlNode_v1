package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
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
}

// pendingDeploy is a project arriving in chunks from the control plane.
type pendingDeploy struct {
	project string
	source  string
	path    string
	next    uint32
	file    *os.File
}

// reportStatus tells the control plane how a deploy is going.
func (s *GrpcClient) reportStatus(deployID, project, state, message string) {
	slog.Info("Reporting deploy status", "project", project, "deploy", deployID, "state", state)
	select {
	case s.send <- &pb.RegisterWorkerRequest{Status: &pb.DeployStatus{
		DeployId: deployID,
		Project:  project,
		State:    state,
		Message:  message,
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

// acceptChunk appends one chunk of an incoming project to its temp file. The
// final chunk hands the assembled archive to buildProject.
func (s *GrpcClient) acceptChunk(ctx context.Context, cmd *pb.DeployCommand) {
	s.mu.Lock()
	p, ok := s.pending[cmd.GetDeployId()]
	if !ok {
		path := filepath.Join(os.TempDir(), "cplane-"+cmd.GetDeployId()+".tar.gz")
		f, err := os.Create(path)
		if err != nil {
			s.mu.Unlock()
			slog.Error("Could not create a file for the incoming project", "error", err)
			return
		}
		p = &pendingDeploy{project: cmd.GetProject(), source: cmd.GetSource(), path: path, file: f}
		s.pending[cmd.GetDeployId()] = p
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

	delete(s.pending, cmd.GetDeployId())
	s.mu.Unlock()

	if err := p.file.Close(); err != nil {
		slog.Error("Could not finish the incoming project file", "project", p.project, "error", err)
	}

	deployID := cmd.GetDeployId()
	s.reportStatus(deployID, p.project, "received", p.source)

	// Building takes a while, so keep the receive loop free.
	go func() {
		if err := s.buildProject(ctx, deployID, p); err != nil {
			slog.Error("Deploy failed", "project", p.project, "error", err)
			s.reportStatus(deployID, p.project, "failed", err.Error())
			return
		}
		s.reportStatus(deployID, p.project, "done", "deployed")
	}()
}

// buildProject unpacks an assembled archive and brings it up with docker
// compose.
func (s *GrpcClient) buildProject(ctx context.Context, deployID string, p *pendingDeploy) error {
	slog.Info("Received project", "project", p.project, "source", p.source, "file", p.path)

	f, err := os.Open(p.path)
	if err != nil {
		return err
	}
	defer func() {
		f.Close()
		os.Remove(p.path)
	}()

	// Replace any previous copy so the deployment matches what was sent.
	dir := filepath.Join(s.projectsDir, p.project)
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
	if _, err := deploypkg.EnsureDockerfile(root, p.project); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(root, deploypkg.ComposeFileName)); err != nil {
		return fmt.Errorf("project %s has no %s: %w", p.project, deploypkg.ComposeFileName, err)
	}

	s.reportStatus(deployID, p.project, "running", "building")

	buildCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	slog.Info("Starting project with docker compose", "project", p.project)
	return workers_docker.ComposeUp(buildCtx, root, p.project+"-"+s.name)
}
