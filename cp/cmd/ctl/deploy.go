package main

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// deployPollInterval is how often the CLI asks the daemon for progress.
	deployPollInterval = 400 * time.Millisecond
	// deployFollowTimeout bounds how long the CLI waits. It is longer than the
	// worker's own build timeout so a slow build is reported rather than cut off
	// by the client.
	deployFollowTimeout = 12 * time.Minute
)

// deployWorkerView mirrors the daemon's per-worker deploy state.
type deployWorkerView struct {
	Name    string `json:"name"`
	State   string `json:"state"`
	Message string `json:"message"`
	URL     string `json:"url"`
}

// deployLogView mirrors one line of deploy output.
type deployLogView struct {
	Worker string `json:"worker"`
	Line   string `json:"line"`
}

// deployStatusView mirrors the daemon's /deploy/status response.
type deployStatusView struct {
	DeployID string             `json:"deploy_id"`
	Project  string             `json:"project"`
	Source   string             `json:"source"`
	Done     bool               `json:"done"`
	Workers  []deployWorkerView `json:"workers"`
	Logs     []deployLogView    `json:"logs"`
	Next     int                `json:"next"`
}

// followDeploy polls the daemon until the deploy reaches a terminal state,
// printing the build output as it arrives.
//
// It returns the final view, and an error only if the CLI could not obtain a
// result (a failed deploy is reported in the view, not as an error here).
func followDeploy(deployID string) (*deployStatusView, error) {
	params := url.Values{}
	params.Set("deploy_id", deployID)

	since := 0
	deadline := time.Now().Add(deployFollowTimeout)
	lastState := map[string]string{}

	for {
		params.Set("since", strconv.Itoa(since))

		var view deployStatusView
		if err := GetJSON("/deploy/status", params, &view); err != nil {
			return nil, err
		}

		// Print everything the workers have said since the last poll.
		for _, line := range view.Logs {
			fmt.Printf("[%s] %s\n", line.Worker, strings.TrimRight(line.Line, "\n"))
		}
		since = view.Next

		// And note state changes, so the user can see progress even when a
		// build is quiet.
		for _, w := range view.Workers {
			if lastState[w.Name] == w.State || w.State == "pending" {
				continue
			}
			lastState[w.Name] = w.State
			fmt.Printf("==> %s: %s\n", w.Name, w.State)
		}

		if view.Done {
			return &view, nil
		}
		if time.Now().After(deadline) {
			return &view, fmt.Errorf("timed out after %s waiting for the deploy to finish", deployFollowTimeout)
		}
		time.Sleep(deployPollInterval)
	}
}

// printDeploySummary reports the outcome of each worker and returns an error if
// any of them ended badly, so the CLI can exit non-zero.
func printDeploySummary(view *deployStatusView) error {
	fmt.Println()
	fmt.Printf("Deploy %s:\n", view.Project)

	var problems []string
	var urls []string
	for _, w := range view.Workers {
		switch w.State {
		case "done":
			fmt.Printf("  %-10s done     %s\n", w.Name, w.URL)
			if w.URL != "" {
				for _, u := range strings.Fields(w.URL) {
					urls = append(urls, u)
				}
			}
		case "exists":
			fmt.Printf("  %-10s rejected %s\n", w.Name, w.Message)
			problems = append(problems, fmt.Sprintf("%s: %s", w.Name, w.Message))
		case "failed":
			fmt.Printf("  %-10s FAILED   %s\n", w.Name, w.Message)
			problems = append(problems, fmt.Sprintf("%s: %s", w.Name, w.Message))
		default:
			fmt.Printf("  %-10s %s\n", w.Name, w.State)
		}
	}

	if len(urls) > 0 {
		fmt.Println()
		for _, u := range urls {
			fmt.Println("  " + u)
		}
	}

	if len(problems) > 0 {
		return fmt.Errorf("deploy failed on %d worker(s)", len(problems))
	}
	return nil
}
