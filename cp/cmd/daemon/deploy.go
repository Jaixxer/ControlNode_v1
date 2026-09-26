package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
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
// empty) and logs the outcome.
func (s *ControlPlane) streamToWorkers(ctx context.Context, project, source string, r io.Reader, names []string) (*DeployResult, error) {
	deployID := newDeployID()

	targets := names
	if len(targets) == 0 {
		targets = s.Workers.names()
	}
	slog.Info("Streaming project to workers",
		"project", project, "source", source, "deploy", deployID, "workers", targets)

	result, err := s.Grpc.DeployStream(deployID, project, source, r, names)
	if err != nil {
		return result, err
	}
	for worker, ferr := range result.Failed {
		slog.Warn("Worker did not receive the deploy", "worker", worker, "error", ferr)
	}
	return result, nil
}

// deploy stages a project directory stored on this machine and streams it to the
// configured workers. The path is resolved here rather than in the CLI because
// the daemon and the CLI share a filesystem (the CLI talks to the daemon over a
// unix socket).
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
	workers := parseWorkers(r.URL.Query().Get("workers"))

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

	created, err := deploy.EnsureDockerfile(stageDir, project)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if len(created) > 0 {
		slog.Info("Generated missing files for the project", "project", project, "files", created)
	}

	archive, err := deploy.Archive(stageDir)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	result, err := s.streamToWorkers(r.Context(), project, absDir, bytes.NewReader(archive), workers)
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
		"project":   project,
		"workers":   result.Sent,
		"bytes":     len(archive),
		"generated": created,
		"sentAt":    time.Now().Format(time.RFC3339),
	})
}

// deployRepo streams a GitHub repository's source straight to the workers. The
// tarball is piped from GitHub through the daemon into the worker stream, so the
// daemon never writes the project to disk.
func (s *ControlPlane) deployRepo(ctx context.Context, project, repoFullName, ref string, workers []string) (*DeployResult, error) {
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

	source := repoFullName + "@" + ref
	return s.streamToWorkers(ctx, project, source, body, workers)
}
