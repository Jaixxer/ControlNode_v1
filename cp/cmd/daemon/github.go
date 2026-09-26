package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	database "jaiveer/ControlPlane/cp/internal/db"
	ghclient "jaiveer/ControlPlane/cp/internal/github"
	"jaiveer/ControlPlane/cp/internal/utils"
	"jaiveer/ControlPlane/pkg/deploy"

	"gorm.io/gorm"
)

// Flow states for the GitHub device flow.
const (
	flowIdle       = "idle"
	flowPending    = "pending"
	flowAuthorized = "authorized"
	flowError      = "error"
)

// githubFlow tracks an in-progress device flow. GitHub makes the CLI wait until
// the user authorises in a browser, which can take minutes, so the daemon runs
// that wait in a goroutine and exposes the outcome here for the CLI to poll.
type githubFlow struct {
	mu        sync.Mutex
	status    string
	userCode  string
	verifyURI string
	login     string
	errMsg    string
}

type githubFlowSnapshot struct {
	Status          string `json:"status"`
	UserCode        string `json:"user_code,omitempty"`
	VerificationURI string `json:"verification_uri,omitempty"`
	Login           string `json:"login,omitempty"`
	Error           string `json:"error,omitempty"`
}

func (f *githubFlow) begin(userCode, verifyURI string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = flowPending
	f.userCode = userCode
	f.verifyURI = verifyURI
	f.login = ""
	f.errMsg = ""
}

func (f *githubFlow) authorize(login string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = flowAuthorized
	f.login = login
}

func (f *githubFlow) fail(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status = flowError
	f.errMsg = err.Error()
}

func (f *githubFlow) snapshot() githubFlowSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	status := f.status
	if status == "" {
		status = flowIdle
	}
	return githubFlowSnapshot{
		Status:          status,
		UserCode:        f.userCode,
		VerificationURI: f.verifyURI,
		Login:           f.login,
		Error:           f.errMsg,
	}
}

// githubService builds the GitHub client from the daemon config.
func (s *ControlPlane) githubService() *ghclient.GithubService {
	return ghclient.NewGithubService(
		s.Config.Github.ClientID,
		s.Config.Github.AppSlug,
		s.Config.Github.WebhookURL,
	)
}

// githubLogin kicks off the device flow and returns the code/URL the user needs.
// The polling for the token runs in a goroutine so this handler can return
// immediately; the CLI then polls /github/status.
func (s *ControlPlane) githubLogin(w http.ResponseWriter, r *http.Request) {
	clientID := s.Config.Github.ClientID
	svc := s.githubService()

	values, err := svc.GetGithubCode()
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	deviceCode := values.Get("device_code")
	interval, _ := strconv.Atoi(values.Get("interval"))
	expiresIn, _ := strconv.Atoi(values.Get("expires_in"))
	if interval == 0 {
		interval = 5
	}
	if expiresIn == 0 {
		expiresIn = 900
	}

	s.Github.begin(values.Get("user_code"), values.Get("verification_uri"))
	writeJSON(w, http.StatusOK, s.Github.snapshot())

	// Polling blocks until the user authorises, so keep it off the request.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(expiresIn)*time.Second)
		defer cancel()

		token, err := svc.PollForToken(ctx, &http.Client{}, clientID, deviceCode, interval, expiresIn)
		if err != nil {
			slog.Error("GitHub device flow failed", "error", err)
			s.Github.fail(err)
			return
		}

		user, err := svc.GetGithubUserInfo(ctx, token.AccessToken)
		if err != nil {
			slog.Error("Failed to fetch GitHub user", "error", err)
			s.Github.fail(err)
			return
		}

		if err := s.saveGithubCredential(user, token.AccessToken); err != nil {
			slog.Error("Failed to store GitHub credential", "error", err)
			s.Github.fail(err)
			return
		}

		slog.Info("GitHub account linked", "login", user.Login, "github_id", user.ID)
		s.Github.authorize(user.Login)
	}()
}

// githubStatus reports the state of the last device flow.
func (s *ControlPlane) githubStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.Github.snapshot())
}

// githubRepos lists the repositories the linked account can see.
func (s *ControlPlane) githubRepos(w http.ResponseWriter, r *http.Request) {
	cred, err := s.githubCredential()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no GitHub account linked yet: " + err.Error()})
		return
	}

	repos, err := s.githubService().ListRepos(r.Context(), cred.AccessToken)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, repos)
}

// githubWatch registers a push webhook on the given "owner/repo" and remembers
// which workers its pushes should be deployed to. Omitting ?workers= means every
// connected worker.
func (s *ControlPlane) githubWatch(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	if !strings.Contains(repo, "/") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "repo must be in owner/name form"})
		return
	}

	cred, err := s.githubCredential()
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no GitHub account linked yet: " + err.Error()})
		return
	}

	owner := strings.SplitN(repo, "/", 2)[0]
	if err := s.githubService().SetupWebhook(r.Context(), owner, cred.AccessToken, repo); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}

	project := r.URL.Query().Get("project")
	if project == "" {
		project = deploy.ProjectName(repo)
	}
	workers := parseWorkers(r.URL.Query().Get("workers"))
	branch := r.URL.Query().Get("branch")

	// Remember the mapping so the webhook knows what to deploy where.
	var watched database.WatchedRepo
	err = s.DB.Where("repo_full_name = ?", repo).First(&watched).Error
	switch {
	case err == nil:
		watched.Project = project
		watched.Workers = strings.Join(workers, ",")
		watched.Branch = branch
		if err := s.DB.Save(&watched).Error; err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	case errors.Is(err, gorm.ErrRecordNotFound):
		watched = database.WatchedRepo{
			RepoFullName: repo,
			Project:      project,
			Workers:      strings.Join(workers, ","),
			Branch:       branch,
		}
		if err := s.DB.Create(&watched).Error; err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	targets := workers
	if len(targets) == 0 {
		targets = []string{"all"}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "webhook created",
		"repo":    repo,
		"project": project,
		"workers": targets,
	})
}

// saveGithubCredential upserts the single GitHub credential row.
func (s *ControlPlane) saveGithubCredential(user *ghclient.GitHubUser, token string) error {
	var cred database.GithubCredential
	err := s.DB.First(&cred).Error
	switch {
	case err == nil:
		cred.GithubID = user.ID
		cred.GithubLogin = user.Login
		cred.AccessToken = token
		return s.DB.Save(&cred).Error
	case errors.Is(err, gorm.ErrRecordNotFound):
		return s.DB.Create(&database.GithubCredential{
			GithubID:    user.ID,
			GithubLogin: user.Login,
			AccessToken: token,
		}).Error
	default:
		return err
	}
}

// githubCredential loads the stored GitHub credential.
func (s *ControlPlane) githubCredential() (*database.GithubCredential, error) {
	var cred database.GithubCredential
	if err := s.DB.First(&cred).Error; err != nil {
		return nil, err
	}
	return &cred, nil
}

// githubWebhook is the callback GitHub delivers App webhooks to. The
// X-Hub-Signature-256 header is verified against the configured secret.
func (s *ControlPlane) githubWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "webhook expects POST"})
		return
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}

	secret := s.Config.Github.WebhookSecret
	if secret == "" {
		slog.Warn("GitHub webhook secret is not configured, skipping signature check")
	} else if !validWebhookSignature(secret, body, r.Header.Get("X-Hub-Signature-256")) {
		slog.Warn("Rejected GitHub webhook with a bad signature")
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid signature"})
		return
	}

	event := r.Header.Get("X-GitHub-Event")
	slog.Info("GitHub webhook received",
		"event", event,
		"delivery", r.Header.Get("X-GitHub-Delivery"),
	)

	if event == "push" {
		var push struct {
			Ref        string `json:"ref"`
			Repository struct {
				FullName      string `json:"full_name"`
				DefaultBranch string `json:"default_branch"`
			} `json:"repository"`
		}
		if err := json.Unmarshal(body, &push); err != nil {
			slog.Error("Failed to decode push payload", "error", err)
		} else {
			slog.Info("Push event", "repo", push.Repository.FullName, "ref", push.Ref)
			s.deployOnPush(push.Repository.FullName, push.Ref, push.Repository.DefaultBranch)
		}
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "received"})
}

// deployOnPush starts a deploy for a repository we watch.
//
// It runs in the background because streaming a repo out to the workers can take
// a while and GitHub expects a prompt response to the webhook.
func (s *ControlPlane) deployOnPush(repoFullName, ref, defaultBranch string) {
	var watched database.WatchedRepo
	if err := s.DB.Where("repo_full_name = ?", repoFullName).First(&watched).Error; err != nil {
		slog.Info("Ignoring push for an unwatched repository", "repo", repoFullName)
		return
	}

	// Only deploy the branch we care about.
	want := watched.Branch
	if want == "" {
		want = defaultBranch
	}
	if want != "" && ref != "refs/heads/"+want {
		slog.Info("Ignoring push to an untracked branch", "repo", repoFullName, "ref", ref, "tracked", want)
		return
	}

	if len(s.Workers.names()) == 0 {
		slog.Warn("Push received but no workers are connected", "repo", repoFullName)
		return
	}

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()

		targets := watched.WorkerList()
		slog.Info("Deploying pushed repo",
			"repo", repoFullName, "project", watched.Project, "ref", ref, "workers", targets)

		result, err := s.deployRepo(ctx, watched.Project, repoFullName, ref, targets)
		if err != nil {
			slog.Error("Push deploy failed", "repo", repoFullName, "error", err)
			return
		}
		slog.Info("Push deploy sent", "repo", repoFullName, "workers", result.Sent)
	}()
}

// validWebhookSignature compares GitHub's HMAC-SHA256 signature with the one we
// compute over the raw request body.
func validWebhookSignature(secret string, body []byte, header string) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(expected), []byte(strings.TrimPrefix(header, prefix)))
}

// installationTokenFor mints a GitHub App installation access token. When
// installationID is empty the app's first installation is used.
func (s *ControlPlane) installationTokenFor(ctx context.Context, installationID string) (string, string, error) {
	appID := strconv.FormatInt(s.Config.Github.AppID, 10)

	if installationID == "" {
		appJWT, err := utils.GenerateAppJWT(appID, s.Config.Github.PrivateKeyPath)
		if err != nil {
			return "", "", err
		}
		installations, err := s.githubService().ListAppInstallations(ctx, appJWT)
		if err != nil {
			return "", "", err
		}
		if len(installations) == 0 {
			return "", "", errors.New("the GitHub App is not installed anywhere")
		}
		installationID = strconv.FormatInt(installations[0].ID, 10)
	}

	token, err := utils.GenerateGithubAppJWT(appID, s.Config.Github.PrivateKeyPath, installationID)
	if err != nil {
		return "", installationID, err
	}
	return token, installationID, nil
}

// installationToken is the common case: a token for whichever installation the
// app has.
func (s *ControlPlane) installationToken(ctx context.Context) (string, error) {
	token, _, err := s.installationTokenFor(ctx, "")
	return token, err
}

// githubInstallationToken mints a GitHub App installation access token using the
// app's private key. The installation comes from ?installation_id=, or the
// app's first installation when omitted.
func (s *ControlPlane) githubInstallationToken(w http.ResponseWriter, r *http.Request) {
	token, installationID, err := s.installationTokenFor(r.Context(), r.URL.Query().Get("installation_id"))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"installation_id": installationID,
		"token":           token,
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("Failed to write JSON response", "error", err)
	}
}
