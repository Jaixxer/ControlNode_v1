package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	deploypkg "jaiveer/ControlPlane/pkg/deploy"

	"github.com/spf13/cobra"
)

func main() {
	version := "1.0.0"
	var rootCmd = &cobra.Command{
		Use:   "ctl",
		Short: "This project is yet to be given a proper description",
	}
	var versionCmd = &cobra.Command{
		Use:   "version",
		Short: "Prints the current version of controlPlane",
		Run: func(cmd *cobra.Command, args []string) {
			fmt.Println("version: " + version)
		},
	}
	var runCmd = &cobra.Command{
		Use:   "run",
		Short: "Used to run the controlPlane daemon with the config provided",
		Run: func(cmd *cobra.Command, args []string) {
			configPath, err := cmd.Flags().GetString("config")
			if err != nil {
				slog.Error("Failed to get config path", "error", err)
				os.Exit(1)
			}
			slog.Info("Config path", "path", configPath)
			params := url.Values{}
			params.Set("config", configPath)
			urlAdd := GetUrl("/run", params)
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, urlAdd.String(), nil)
			if err != nil {
				slog.Error("Error creating the request: ", "error", err)
				os.Exit(1)
			}
			client := DaemonClient()
			resp, err := client.Do(req)
			if err != nil {
				slog.Error("Error running the daemon: ", "error", err)
				os.Exit(1)
			}
			defer resp.Body.Close()

			scanner := bufio.NewScanner(resp.Body)
			for scanner.Scan() {
				fmt.Println(scanner.Text())
			}

		},
	}
	runCmd.Flags().String("config", "none", "Config file path")
	var addWorkerCmd = &cobra.Command{
		Use:   "add-worker",
		Short: "To add worker node",
		Run: func(cmd *cobra.Command, args []string) {
			name, err := cmd.Flags().GetString("name")
			if err != nil {
				slog.Error("Error parsing the name", "error", err)
				os.Exit(1)
			}
			parms := url.Values{}
			parms.Set("name", name)
			urlAdd := GetUrl("/add-worker", parms)
			req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, urlAdd.String(), nil)
			if err != nil {
				slog.Error("Error creating the request: ", "error", err)
				os.Exit(1)
			}
			client := DaemonClient()
			resp, err := client.Do(req)
			if err != nil {
				slog.Error("Error adding the worker ", "error", err)
				os.Exit(1)
			}
			defer resp.Body.Close()

			scanner := bufio.NewScanner(resp.Body)
			for scanner.Scan() {
				fmt.Println(scanner.Text())
			}

		},
	}
	addWorkerCmd.Flags().String("name", "", "Worker name")
	var authGithubCmd = &cobra.Command{
		Use:   "github",
		Short: "Link a GitHub account using the device flow",
		Run: func(cmd *cobra.Command, args []string) {
			var login struct {
				Status          string `json:"status"`
				UserCode        string `json:"user_code"`
				VerificationURI string `json:"verification_uri"`
			}
			if err := GetJSON("/github/login", nil, &login); err != nil {
				slog.Error("Failed to start GitHub login", "error", err)
				os.Exit(1)
			}
			fmt.Println("Visit:", login.VerificationURI)
			fmt.Println("and enter code:", login.UserCode)
			fmt.Println("Waiting for authorisation...")

			// The daemon polls GitHub in the background, so we poll the daemon.
			for {
				time.Sleep(2 * time.Second)
				var status struct {
					Status string `json:"status"`
					Login  string `json:"login"`
					Error  string `json:"error"`
				}
				if err := GetJSON("/github/status", nil, &status); err != nil {
					slog.Error("Failed to read GitHub status", "error", err)
					os.Exit(1)
				}
				switch status.Status {
				case "authorized":
					fmt.Println("GitHub account linked:", status.Login)
					return
				case "error":
					fmt.Println("GitHub login failed:", status.Error)
					os.Exit(1)
				}
			}
		},
	}
	var authCmd = &cobra.Command{
		Use:   "auth",
		Short: "Authentication commands",
	}
	authCmd.AddCommand(authGithubCmd)
	var githubReposCmd = &cobra.Command{
		Use:   "repos",
		Short: "List GitHub repositories for the linked account",
		Run: func(cmd *cobra.Command, args []string) {
			var repos []struct {
				FullName string `json:"full_name"`
			}
			if err := GetJSON("/github/repos", nil, &repos); err != nil {
				slog.Error("Failed to list GitHub repositories", "error", err)
				os.Exit(1)
			}
			for _, repo := range repos {
				fmt.Println(repo.FullName)
			}
		},
	}
	var githubWatchCmd = &cobra.Command{
		Use:   "watch",
		Short: "Set up a push webhook on a GitHub repository",
		Long: "Registers a push webhook and remembers which workers the repository\n" +
			"should be deployed to. Pushes are then streamed to those workers.",
		Run: func(cmd *cobra.Command, args []string) {
			repo, err := cmd.Flags().GetString("repo")
			if err != nil {
				slog.Error("Error parsing the repo", "error", err)
				os.Exit(1)
			}
			workers, err := cmd.Flags().GetString("workers")
			if err != nil {
				slog.Error("Error parsing the workers", "error", err)
				os.Exit(1)
			}
			project, err := cmd.Flags().GetString("project")
			if err != nil {
				slog.Error("Error parsing the project", "error", err)
				os.Exit(1)
			}
			branch, err := cmd.Flags().GetString("branch")
			if err != nil {
				slog.Error("Error parsing the branch", "error", err)
				os.Exit(1)
			}

			params := url.Values{}
			params.Set("repo", repo)
			if workers != "" {
				params.Set("workers", workers)
			}
			if project != "" {
				params.Set("project", project)
			}
			if branch != "" {
				params.Set("branch", branch)
			}

			var res struct {
				Status  string   `json:"status"`
				Repo    string   `json:"repo"`
				Project string   `json:"project"`
				Workers []string `json:"workers"`
			}
			if err := GetJSON("/github/watch", params, &res); err != nil {
				slog.Error("Failed to set up the webhook", "error", err)
				os.Exit(1)
			}
			fmt.Printf("%s for %s -> project %q, workers: %s\n",
				res.Status, res.Repo, res.Project, strings.Join(res.Workers, ", "))
		},
	}
	githubWatchCmd.Flags().String("repo", "", "Repository in owner/name form")
	githubWatchCmd.Flags().String("workers", "", "Comma separated worker names (defaults to all connected workers)")
	githubWatchCmd.Flags().String("project", "", "Project name to deploy as (defaults to the repository name)")
	githubWatchCmd.Flags().String("branch", "", "Branch to deploy (defaults to the repository's default branch)")
	var githubCmd = &cobra.Command{
		Use:   "github",
		Short: "GitHub integration commands",
	}
	githubCmd.AddCommand(githubReposCmd)
	githubCmd.AddCommand(githubWatchCmd)
	var deployCmd = &cobra.Command{
		Use:   "deploy <dir_path>",
		Short: "Deploy a local node/js project to a worker",
		Long: "Packages a project directory, strips node_modules, generates a Dockerfile\n" +
			"if the project does not ship one, and sends it to a worker to run.",
		Args: cobra.ExactArgs(1),
		Run: func(cmd *cobra.Command, args []string) {
			absDir, err := filepath.Abs(args[0])
			if err != nil {
				fmt.Fprintln(os.Stderr, "Error:", err)
				os.Exit(1)
			}

			// Check locally first so the user gets an immediate answer instead
			// of a round trip to the daemon.
			ok, err := deploypkg.IsNodeProject(absDir)
			if err != nil {
				fmt.Fprintln(os.Stderr, "Error:", err)
				os.Exit(1)
			}
			if !ok {
				fmt.Fprintln(os.Stderr, "Error:", deploypkg.UnsupportedMsg)
				os.Exit(1)
			}

			project, err := cmd.Flags().GetString("project")
			if err != nil {
				slog.Error("Error parsing the project", "error", err)
				os.Exit(1)
			}
			worker, err := cmd.Flags().GetString("workers")
			if err != nil {
				slog.Error("Error parsing the workers", "error", err)
				os.Exit(1)
			}

			params := url.Values{}
			params.Set("path", absDir)
			if project != "" {
				params.Set("project", project)
			}
			if worker != "" {
				params.Set("workers", worker)
			}

			var res struct {
				Status    string   `json:"status"`
				Project   string   `json:"project"`
				Workers   []string `json:"workers"`
				Bytes     int      `json:"bytes"`
				Generated []string `json:"generated"`
			}
			if err := GetJSON("/deploy", params, &res); err != nil {
				slog.Error("Deploy failed", "error", err)
				os.Exit(1)
			}

			if len(res.Generated) > 0 {
				fmt.Println("Generated missing files:", strings.Join(res.Generated, ", "))
			}
			targets := res.Workers
			if len(targets) == 0 {
				targets = []string{"all connected workers"}
			}
			fmt.Printf("Deploying %s to %s (%d bytes)\n", res.Project, strings.Join(targets, ", "), res.Bytes)
		},
	}
	deployCmd.Flags().String("project", "", "Override the project name (defaults to the directory name)")
	deployCmd.Flags().String("workers", "", "Comma separated worker names (defaults to all connected workers)")
	var workersCmd = &cobra.Command{
		Use:   "workers",
		Short: "List worker nodes and their liveness",
		Run: func(cmd *cobra.Command, args []string) {
			var rows []struct {
				Name      string  `json:"name"`
				Status    string  `json:"status"`
				LastSeen  string  `json:"last_seen"`
				AgeSec    float64 `json:"seconds_since_seen"`
				Connected bool    `json:"stream_connected"`
			}
			if err := GetJSON("/workers", nil, &rows); err != nil {
				slog.Error("Failed to list workers", "error", err)
				os.Exit(1)
			}
			if len(rows) == 0 {
				fmt.Println("No workers have ever connected.")
				return
			}
			fmt.Printf("%-16s %-8s %-9s %-10s %s\n", "NAME", "STATUS", "STREAM", "LAST SEEN", "AGE")
			for _, row := range rows {
				stream := "closed"
				if row.Connected {
					stream = "open"
				}
				fmt.Printf("%-16s %-8s %-9s %-10s %.0fs\n",
					row.Name, row.Status, stream, row.LastSeen, row.AgeSec)
			}
		},
	}
	rootCmd.AddCommand(versionCmd)
	rootCmd.AddCommand(addWorkerCmd)
	rootCmd.AddCommand(runCmd)
	rootCmd.AddCommand(authCmd)
	rootCmd.AddCommand(githubCmd)
	rootCmd.AddCommand(deployCmd)
	rootCmd.AddCommand(workersCmd)
	if err := rootCmd.Execute(); err != nil {
		fmt.Println("Error Occured: ", err)
		os.Exit(1)
	}
}
func GetUrl(path string, params url.Values) *url.URL {
	urlAdd := url.URL{Scheme: "http", Host: "local", Path: path, RawQuery: params.Encode()}
	return &urlAdd
}
func DaemonClient() *http.Client {
	dialer := &net.Dialer{}

	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", "/tmp/cplane.sock")

		},
	}
	client := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	return client

}

// GetJSON calls a daemon endpoint over the unix socket and decodes the JSON
// response into out.
func GetJSON(path string, params url.Values, out any) error {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, GetUrl(path, params).String(), nil)
	if err != nil {
		return err
	}
	resp, err := DaemonClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var apiErr struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(body, &apiErr) == nil && apiErr.Error != "" {
			return errors.New(apiErr.Error)
		}
		return fmt.Errorf("daemon returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, out)
}
