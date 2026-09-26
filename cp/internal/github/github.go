// Package ghclient holds the GitHub API client used by the control plane
// daemon: the OAuth device flow, user/repo lookups, GitHub App installation
// checks and webhook setup.
//
// This was ported from an older version of the app. The polling calls
// (PollForToken, PollForInstallation) block until the user acts in the browser,
// so callers must run them in a goroutine rather than inline in an HTTP
// handler.
package ghclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// GithubService talks to the GitHub API on behalf of the control plane.
type GithubService struct {
	clientID   string
	appSlug    string
	webhookURL string
}

// NewGithubService builds a service. clientID and appSlug come from config,
// webhookURL is the public URL GitHub should deliver webhooks to.
func NewGithubService(clientID, appSlug, webhookURL string) *GithubService {
	return &GithubService{
		clientID:   clientID,
		appSlug:    appSlug,
		webhookURL: webhookURL,
	}
}

type GithubWebhookConfig struct {
	URL          string `json:"url"`
	Content_Type string `json:"content_type"`
	InsecureSSL  string `json:"insecure_ssl"`
}

type GithubWebhookInitialise struct {
	Owner  string              `json:"owner"`
	Repo   string              `json:"repo"`
	Name   string              `json:"name"`
	Active bool                `json:"active"`
	Events []string            `json:"events"`
	Config GithubWebhookConfig `json:"config"`
}

type DeviceTokenResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	Scope       string `json:"scope"`

	Error            string `json:"error"`
	ErrorDescription string `json:"error_description"`
	Interval         int    `json:"interval"`
}

type GitHubRepo struct {
	ID              int64     `json:"id"`
	Name            string    `json:"name"`
	FullName        string    `json:"full_name"`
	Private         bool      `json:"private"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
	PushedAt        time.Time `json:"pushed_at"`
	StargazersCount int       `json:"stargazers_count"`
	WatchersCount   int       `json:"watchers_count"`
	Language        string    `json:"language"`
	DefaultBranch   string    `json:"default_branch"`
}

type GitHubUser struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
}

type GitHubInstallation struct {
	ID      int64      `json:"id"`
	AppSlug string     `json:"app_slug"`
	Account GitHubUser `json:"account"`
}

// GetGithubCode starts the device flow and returns GitHub's form encoded
// response (device_code, user_code, verification_uri, interval, expires_in).
func (s *GithubService) GetGithubCode() (url.Values, error) {
	form := url.Values{}
	form.Set("client_id", s.clientID)
	form.Set("scope", "repo admin:repo_hook")

	resp, err := http.Post("https://github.com/login/device/code", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()))
	if err != nil {
		slog.Error("Failed to request GitHub device code", slog.Any("error", err))
		return nil, err
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		slog.Error("Error reading the body of github response", slog.Any("error", err))
		return nil, err
	}
	values, err := url.ParseQuery(string(respBody))
	if err != nil {
		slog.Error("Failed to parse the query", slog.Any("error", err))
		return nil, err
	}
	return values, nil
}

// PollForToken keeps polling GitHub until the user authorises the device, the
// code expires, or ctx is cancelled. This blocks, so run it in a goroutine.
func (s *GithubService) PollForToken(ctx context.Context, client *http.Client, clientID string, deviceCode string, interval int, expiresIn int) (*DeviceTokenResponse, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		form := url.Values{}
		form.Set("device_code", deviceCode)
		form.Set("client_id", clientID)
		form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://github.com/login/oauth/access_token", strings.NewReader(form.Encode()))
		if err != nil {
			slog.Error("Failed to create request for GitHub access token", slog.Any("error", err))
			return nil, err
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Accept", "application/json")

		resp, err := client.Do(req)
		if err != nil {
			slog.Error("Failed to request GitHub access token", slog.Any("error", err))
			return nil, err
		}

		var tr DeviceTokenResponse
		if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
			slog.Error("Failed to decode GitHub access token response", slog.Any("error", err))
			_ = resp.Body.Close()
			return nil, err
		}
		_ = resp.Body.Close()

		if tr.AccessToken != "" {
			return &tr, nil
		}
		switch tr.Error {
		case "authorization_pending":
		case "slow_down":
			if tr.Interval > 0 {
				interval += tr.Interval
			} else {
				interval += 5
			}
		case "expired_token":
			return nil, errors.New("device code expired")
		case "access_denied":
			return nil, errors.New("user denied authorization")
		case "incorrect_device_code":
			return nil, errors.New("invalid device code")
		case "incorrect_client_credentials":
			return nil, errors.New("invalid client credentials")
		case "unsupported_grant_type":
			return nil, errors.New("unsupported grant type")
		case "device_flow_disabled":
			return nil, errors.New("device flow disabled")
		default:
			return nil, fmt.Errorf("unexpected oauth error: %s", tr.Error)
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(interval) * time.Second):
		}
	}
}

func (s *GithubService) GetGithubUserInfo(ctx context.Context, accessToken string) (*GitHubUser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user", nil)
	if err != nil {
		slog.Error("Failed to create request for GitHub user info", slog.Any("error", err))
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Error("Failed to request GitHub user info", slog.Any("error", err))
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		slog.Error("GitHub user API returned an error", slog.Int("status", resp.StatusCode), slog.String("body", string(body)))
		return nil, fmt.Errorf("github user api error: %d", resp.StatusCode)
	}

	var user GitHubUser
	if err = json.NewDecoder(resp.Body).Decode(&user); err != nil {
		slog.Error("Failed to decode GitHub user info response", slog.Any("error", err))
		return nil, err
	}
	if user.ID == 0 || user.Login == "" {
		return nil, errors.New("invalid GitHub user response")
	}
	return &user, nil
}

func (s *GithubService) ListRepos(ctx context.Context, accessToken string) ([]GitHubRepo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user/repos", nil)
	if err != nil {
		slog.Error("Failed to create request for GitHub repositories", slog.Any("error", err))
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Error("Failed to request GitHub repositories", slog.Any("error", err))
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		slog.Error("GitHub API returned an error", slog.Int("status", resp.StatusCode), slog.String("body", string(body)))
		return nil, fmt.Errorf("github api error: %d", resp.StatusCode)
	}

	var repos []GitHubRepo
	if err = json.NewDecoder(resp.Body).Decode(&repos); err != nil {
		slog.Error("Failed to decode GitHub repositories response", slog.Any("error", err))
		return nil, err
	}
	return repos, nil
}

func (s *GithubService) CheckInstallation(ctx context.Context, accessToken string) (*GitHubInstallation, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user/installations", nil)
	if err != nil {
		slog.Error("Failed to create request for GitHub installations", slog.Any("error", err))
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Error("Failed to request GitHub installations", slog.Any("error", err))
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		slog.Error("GitHub installations API returned an error", slog.Int("status", resp.StatusCode), slog.String("body", string(body)))
		return nil, fmt.Errorf("github installations api error: %d", resp.StatusCode)
	}

	var result struct {
		TotalCount    int                  `json:"total_count"`
		Installations []GitHubInstallation `json:"installations"`
	}
	if err = json.NewDecoder(resp.Body).Decode(&result); err != nil {
		slog.Error("Failed to decode GitHub installations response", slog.Any("error", err))
		return nil, err
	}
	if result.TotalCount == 0 || len(result.Installations) == 0 {
		return nil, nil
	}
	return &result.Installations[0], nil
}

// ListAppInstallations lists the installations of the GitHub App itself, using
// an app JWT rather than a user token.
func (s *GithubService) ListAppInstallations(ctx context.Context, appJWT string) ([]GitHubInstallation, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/app/installations", nil)
	if err != nil {
		slog.Error("Failed to create request for app installations", slog.Any("error", err))
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+appJWT)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Error("Failed to request app installations", slog.Any("error", err))
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		slog.Error("GitHub app installations API returned an error", slog.Int("status", resp.StatusCode), slog.String("body", string(body)))
		return nil, fmt.Errorf("github app installations api error: %d", resp.StatusCode)
	}

	var installations []GitHubInstallation
	if err = json.NewDecoder(resp.Body).Decode(&installations); err != nil {
		slog.Error("Failed to decode app installations response", slog.Any("error", err))
		return nil, err
	}
	return installations, nil
}

// PollForInstallation polls until the GitHub App is installed or ctx is
// cancelled. This blocks, so run it in a goroutine.
func (s *GithubService) PollForInstallation(ctx context.Context, accessToken string) (*GitHubInstallation, error) {
	interval := 5
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		installation, err := s.CheckInstallation(ctx, accessToken)
		if err != nil {
			return nil, err
		}
		if installation != nil {
			return installation, nil
		}

		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(interval) * time.Second):
		}
	}
}

func (s *GithubService) GetInstallationURL() string {
	return fmt.Sprintf("https://github.com/apps/%s/installations/new", s.appSlug)
}

// RepoTarball opens a stream of the repository's source at ref as a tar.gz.
//
// The caller owns the returned reader and must close it. Nothing is written to
// disk, so the archive can be piped straight on to a worker.
func (s *GithubService) RepoTarball(ctx context.Context, repoFullName, ref, token string) (io.ReadCloser, error) {
	if ref == "" {
		ref = "HEAD"
	}
	endpoint := fmt.Sprintf("https://api.github.com/repos/%s/tarball/%s", repoFullName, url.PathEscape(ref))

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		slog.Error("Failed to create request for repository tarball", slog.Any("error", err))
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Error("Failed to request repository tarball", slog.Any("error", err))
		return nil, err
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		slog.Error("GitHub tarball API returned an error",
			slog.Int("status", resp.StatusCode), slog.String("body", string(body)))
		return nil, fmt.Errorf("github tarball api error: %d: %s", resp.StatusCode, string(body))
	}
	return resp.Body, nil
}

// SetupWebhook registers a push webhook on the given repository, pointing at
// the configured public webhook URL.
func (s *GithubService) SetupWebhook(ctx context.Context, ownerName string, accessToken string, repoFullName string) error {
	h := GithubWebhookInitialise{
		Owner:  ownerName,
		Repo:   repoFullName,
		Name:   "web",
		Active: true,
		Events: []string{"push"},
		Config: GithubWebhookConfig{
			URL:          s.webhookURL,
			Content_Type: "json",
			InsecureSSL:  "0",
		},
	}
	body, err := json.Marshal(h)
	if err != nil {
		slog.Error("Failed to marshal webhook configuration", slog.Any("error", err))
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("https://api.github.com/repos/%s/hooks", repoFullName), bytes.NewBuffer(body))
	if err != nil {
		slog.Error("Failed to create request for GitHub webhook setup", slog.Any("error", err))
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		slog.Error("Failed to request GitHub webhook setup", slog.Any("error", err))
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		slog.Error("GitHub API returned an error during webhook setup", slog.Int("status", resp.StatusCode), slog.String("body", string(respBody)))
		return fmt.Errorf("github webhook api error: %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}
