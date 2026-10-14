package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"jaiveer/ControlPlane/pkg/deploy"
)

// newDeployID returns a short random id so a worker's status updates can be tied
// back to the deploy that caused them.
func newDeployID() string {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "deploy"
	}
	return hex.EncodeToString(b)
}

// parseWorkers turns the comma separated ?workers= parameter into a list. An
// empty list means every connected worker, which is the default.
func parseWorkers(raw string) []string {
	var out []string
	for _, name := range strings.Split(raw, ",") {
		if name = strings.TrimSpace(name); name != "" {
			out = append(out, name)
		}
	}
	return out
}

// streamToWorkers pushes an archive to the given workers (all when names is
// empty) and logs the outcome. It returns the deploy id so the caller can report
// progress against it.
func (s *ControlPlane) streamToWorkers(ctx context.Context, deployID string, opts DeployOptions, r io.Reader, names []string) (*DeployResult, error) {
	targets := names
	if len(targets) == 0 {
		targets = s.Workers.names()
	}
	slog.Info("Streaming project to workers",
		"project", opts.Project, "source", opts.Source, "deploy", deployID,
		"workers", targets, "force", opts.Force)

	result, err := s.Grpc.DeployStream(deployID, opts, r, names)
	if err != nil {
		return result, err
	}
	for worker, ferr := range result.Failed {
		if worker == "_requested" {
			continue
		}
		slog.Warn("Worker did not receive the deploy", "worker", worker, "error", ferr)
	}
	for worker, reason := range result.Rejected {
		slog.Warn("Worker refused the deploy", "worker", worker, "reason", reason)
	}
	return result, nil
}

// deployQuery holds the parameters of a deploy request.
type deployQuery struct {
	Project       string
	Source        string
	Workers       []string
	Force         bool
	HostPort      uint16
	ContainerPort int
}

// parseDeployQuery reads and validates the shared deploy parameters. defaultProject
// is used when the request does not name one.
func parseDeployQuery(r *http.Request, defaultProject, source string) (*deployQuery, error) {
	q := &deployQuery{
		Project:       strings.TrimSpace(r.URL.Query().Get("project")),
		Source:        source,
		Workers:       parseWorkers(r.URL.Query().Get("workers")),
		Force:         r.URL.Query().Get("force") == "true",
		ContainerPort: deploy.DefaultContainerPort,
	}
	if q.Project == "" {
		q.Project = defaultProject
	}
	if q.Project == "" {
		return nil, errors.New("project is required")
	}

	if raw := r.URL.Query().Get("containerPort"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > 65535 {
			return nil, fmt.Errorf("invalid containerPort %q", raw)
		}
		q.ContainerPort = n
	}

	// A fixed host port only makes sense for a single worker: two workers
	// sharing a Docker host cannot both bind the same port.
	if raw := r.URL.Query().Get("port"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n <= 0 || n > 65535 {
			return nil, fmt.Errorf("invalid port %q", raw)
		}
		if len(q.Workers) > 1 {
			return nil, fmt.Errorf("--port cannot be used when deploying to more than one worker (%v): leave it unset so each worker publishes its own port", q.Workers)
		}
		q.HostPort = uint16(n)
	}

	return q, nil
}

// deploy stages a project directory stored on this machine and streams it to the
// configured workers. The path is resolved here rather than in the CLI because
// the daemon and the CLI share a filesystem (the CLI talks to the daemon over a
// unix socket).
//
// It returns as soon as the archive has been handed to the workers. The CLI then
// follows progress through /deploy/status, since building can take minutes.
func (s *ControlPlane) deploy(w http.ResponseWriter, r *http.Request) {
	dir := r.URL.Query().Get("path")
	if dir == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path is required"})
		return
	}

	absDir, err := filepath.Abs(dir)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	// Same check the CLI does, repeated here because the daemon is the
	// authority on what it will accept.
	ok, err := deploy.IsNodeProject(absDir)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": deploy.UnsupportedMsg})
		return
	}

	project := r.URL.Query().Get("project")
	if project == "" {
		project = deploy.ProjectName(absDir)
	}

	q, err := parseDeployQuery(r, project, absDir)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	project = q.Project

	if len(s.Workers.names()) == 0 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error":   ErrNoWorkers.Error(),
			"workers": "start a worker with ./worker --join-address localhost:50051 --token <token>",
		})
		return
	}

	// Copy into a scratch directory first so node_modules is stripped and any
	// Dockerfile we generate never touches the user's real project.
	stageDir, err := os.MkdirTemp("", "cplane-deploy-"+project+"-")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	defer os.RemoveAll(stageDir)

	if err := deploy.Stage(absDir, stageDir); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	prepared, err := deploy.EnsureBuildFiles(stageDir, project, q.ContainerPort, int(q.HostPort))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if len(prepared.Created) > 0 {
		slog.Info("Generated missing files for the project", "project", project, "files", prepared.Created)
	}
	for _, note := range prepared.Notes {
		slog.Warn("Deploy note", "project", project, "note", note)
	}

	archive, err := deploy.Archive(stageDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	deployID := newDeployID()
	opts := DeployOptions{
		Project:       project,
		Source:        absDir,
		Force:         q.Force,
		HostPort:      uint32(q.HostPort),
		ContainerPort: uint32(q.ContainerPort),
	}

	result, err := s.streamToWorkers(r.Context(), deployID, opts, bytes.NewReader(archive), q.Workers)
	if err != nil {
		status := http.StatusBadGateway
		if errors.Is(err, ErrNoWorkers) {
			status = http.StatusServiceUnavailable
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"status":    "sent",
		"deploy_id": deployID,
		"project":   project,
		"workers":   result.Sent,
		"rejected":  result.Rejected,
		"bytes":     len(archive),
		"generated": prepared.Created,
		"notes":     prepared.Notes,
		"sentAt":    time.Now().Format(time.RFC3339),
	})
}

// deployStatus reports how a deploy is going, including output produced so far.
// The CLI polls this with ?since=<next> to print only new lines.
func (s *ControlPlane) deployStatus(w http.ResponseWriter, r *http.Request) {
	deployID := r.URL.Query().Get("deploy_id")
	if deployID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "deploy_id is required"})
		return
	}

	since := 0
	if raw := r.URL.Query().Get("since"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("invalid since %q", raw)})
			return
		}
		since = n
	}

	view, ok := s.Grpc.Deploys.view(deployID, since)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "unknown deploy id " + deployID})
		return
	}
	writeJSON(w, http.StatusOK, view)
}

// deployRepo streams a GitHub repository's source straight to the workers. The
// tarball is piped from GitHub through the daemon into the worker stream, so the
// daemon never writes the project to disk.
func (s *ControlPlane) deployRepo(ctx context.Context, opts DeployOptions, repoFullName, ref string, workers []string) (*DeployResult, error) {
	cred, err := s.githubCredential()
	if err != nil {
		return nil, err
	}
	token := cred.AccessToken

	// A GitHub App should act with an installation token rather than the
	// user-to-server token from the device flow, so prefer that when the app is
	// configured. Fall back to the user token if it is not available.
	if s.Config.Github.AppID != 0 && s.Config.Github.PrivateKeyPath != "" {
		if installationToken, err := s.installationToken(ctx); err == nil {
			token = installationToken
		} else {
			slog.Warn("Falling back to the stored user token for the repo fetch", "error", err)
		}
	}

	body, err := s.githubService().RepoTarball(ctx, repoFullName, ref, token)
	if err != nil {
		return nil, err
	}
	defer body.Close()

	opts.Source = repoFullName + "@" + ref
	return s.streamToWorkers(ctx, newDeployID(), opts, body, workers)
}
