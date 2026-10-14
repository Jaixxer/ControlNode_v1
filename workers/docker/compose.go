package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	deploypkg "jaiveer/ControlPlane/pkg/deploy"
)

// Compose file names are owned by pkg/deploy, which generates them. They are
// aliased here so the two cannot drift.
const (
	ComposeFileName  = deploypkg.ComposeFileName
	OverrideFileName = deploypkg.OverrideFileName
)

// ComposeFile is the compose file the worker looks for inside a project
// directory.
const ComposeFile = deploypkg.ComposeFileName

// PublishedPort is a port a started project is reachable on.
type PublishedPort struct {
	Service       string
	ContainerPort int
	HostPort      int
	Protocol      string
}

// ComposeUp builds and starts the compose project in dir, detached, streaming
// the build output to out.
//
// Detached rather than foreground is deliberate: a deploy should finish and
// report where the app is, instead of blocking until the containers exit. The
// containers keep running afterwards and Docker restarts them if they stop.
//
// projectName scopes the compose project, which keeps container and network
// names from colliding when two workers share a Docker host. It is ignored when
// empty, letting compose fall back to the directory name.
//
// It returns the ports the project is published on, as Docker actually resolved
// them, so callers can report a real URL rather than one they guessed.
//
// Unlike the rest of this package, which talks to the daemon over the Docker
// SDK, compose is a Docker CLI plugin so this shells out to the docker binary.
func ComposeUp(ctx context.Context, dir, projectName string, out io.Writer) ([]PublishedPort, error) {
	composeFile := filepath.Join(dir, ComposeFile)
	if _, err := os.Stat(composeFile); err != nil {
		return nil, fmt.Errorf("looking for %s in %s: %w", ComposeFile, dir, err)
	}

	cmd := exec.CommandContext(ctx, "docker", composeArgs(projectName, composeFile, "up", "--build", "-d")...)
	cmd.Dir = dir
	cmd.Stdout = out
	cmd.Stderr = out

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker compose up in %s: %w", dir, err)
	}

	ports, err := PublishedPorts(ctx, dir, projectName)
	if err != nil {
		// The project is up; failing to read the ports only costs us the URL.
		return nil, err
	}
	return ports, nil
}

// ContainerStatus is the state of one container in a compose project.
type ContainerStatus struct {
	Service  string
	Name     string
	State    string
	ExitCode int
}

// ContainerStatuses reports the state of each container in the project.
//
// Detached mode reports success as soon as containers are started, so a project
// whose app dies on startup would otherwise look like a successful deploy. This
// is how that is caught.
func (c *Client) ContainerStatuses(ctx context.Context, dir, projectName string) ([]ContainerStatus, error) {
	composeFile := filepath.Join(dir, ComposeFile)
	cmd := exec.CommandContext(ctx, "docker", composeArgs(projectName, composeFile, "ps", "--all", "--format", "json")...)
	cmd.Dir = dir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker compose ps: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	entries, err := parseComposePSEntries(stdout.Bytes())
	if err != nil {
		return nil, err
	}

	out := make([]ContainerStatus, 0, len(entries))
	for _, e := range entries {
		out = append(out, ContainerStatus{
			Service:  e.Service,
			Name:     e.Name,
			State:    e.State,
			ExitCode: e.ExitCode,
		})
	}
	return out, nil
}

// ContainerLogs returns the tail of the project's container logs, which is what
// explains a container that came up and died.
func (c *Client) ContainerLogs(ctx context.Context, dir, projectName string, tail int) (string, error) {
	composeFile := filepath.Join(dir, ComposeFile)
	cmd := exec.CommandContext(ctx, "docker",
		composeArgs(projectName, composeFile, "logs", "--no-color", "--tail", strconv.Itoa(tail))...)
	cmd.Dir = dir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Compose returns non-zero when a container has exited, which is exactly
	// when we most want the logs, so only fail if we got nothing at all.
	if err := cmd.Run(); err != nil && stdout.Len() == 0 && stderr.Len() == 0 {
		return "", fmt.Errorf("docker compose logs: %w", err)
	}
	return stdout.String() + stderr.String(), nil
}

// PublishedPorts asks Docker which ports the project is actually published on.
// This is the only way to learn the host port when the compose file lets Docker
// allocate one.
func (c *Client) PublishedPorts(ctx context.Context, dir, projectName string) ([]PublishedPort, error) {
	composeFile := filepath.Join(dir, ComposeFile)
	cmd := exec.CommandContext(ctx, "docker", composeArgs(projectName, composeFile, "ps", "--format", "json")...)
	cmd.Dir = dir

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("docker compose ps: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return parseComposePS(stdout.Bytes())
}

// PublishedPorts is the package level form, for callers that do not hold a
// Client of their own.
func PublishedPorts(ctx context.Context, dir, projectName string) ([]PublishedPort, error) {
	c, err := New()
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.PublishedPorts(ctx, dir, projectName)
}

// ContainerStatuses is the package level form of (*Client).ContainerStatuses.
func ContainerStatuses(ctx context.Context, dir, projectName string) ([]ContainerStatus, error) {
	c, err := New()
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.ContainerStatuses(ctx, dir, projectName)
}

// ContainerLogs is the package level form of (*Client).ContainerLogs.
func ContainerLogs(ctx context.Context, dir, projectName string, tail int) (string, error) {
	c, err := New()
	if err != nil {
		return "", err
	}
	defer c.Close()
	return c.ContainerLogs(ctx, dir, projectName, tail)
}

// ComposeDown stops and removes a project's containers.
func ComposeDown(ctx context.Context, dir, projectName string) error {
	composeFile := filepath.Join(dir, ComposeFile)
	if _, err := os.Stat(composeFile); err != nil {
		return fmt.Errorf("looking for %s in %s: %w", ComposeFile, dir, err)
	}

	cmd := exec.CommandContext(ctx, "docker", composeArgs(projectName, composeFile, "down")...)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker compose down: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// composeArgs builds the docker compose argument list.
//
// Every file is passed with an explicit -f because compose only merges an
// override automatically when it discovers the files itself; with -f it uses
// exactly what it is given. The override is therefore passed explicitly, and
// after the base file so it wins.
func composeArgs(projectName, composeFile string, rest ...string) []string {
	args := []string{"compose"}
	if projectName != "" {
		args = append(args, "-p", projectName)
	}
	args = append(args, "-f", composeFile)

	override := filepath.Join(filepath.Dir(composeFile), OverrideFileName)
	if _, err := os.Stat(override); err == nil {
		args = append(args, "-f", override)
	}

	return append(args, rest...)
}

// composePSEntry is the subset of `docker compose ps --format json` we need.
type composePSEntry struct {
	Service    string `json:"Service"`
	Name       string `json:"Name"`
	State      string `json:"State"`
	ExitCode   int    `json:"ExitCode"`
	Publishers []struct {
		TargetPort    int    `json:"TargetPort"`
		PublishedPort int    `json:"PublishedPort"`
		Protocol      string `json:"Protocol"`
	} `json:"Publishers"`
}

// parseComposePSEntries copes with both shapes of output: newer docker emits a
// JSON array, other builds emit one JSON object per line.
func parseComposePSEntries(raw []byte) ([]composePSEntry, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return nil, nil
	}

	if trimmed[0] == '[' {
		var entries []composePSEntry
		if err := json.Unmarshal(trimmed, &entries); err != nil {
			return nil, fmt.Errorf("parsing compose ps output: %w", err)
		}
		return entries, nil
	}

	var entries []composePSEntry
	scanner := bufio.NewScanner(bytes.NewReader(trimmed))
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var entry composePSEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			return nil, fmt.Errorf("parsing compose ps line: %w", err)
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

// parseComposePS flattens the published ports from `docker compose ps`.
func parseComposePS(raw []byte) ([]PublishedPort, error) {
	entries, err := parseComposePSEntries(raw)
	if err != nil {
		return nil, err
	}

	var ports []PublishedPort
	for _, entry := range entries {
		for _, pub := range entry.Publishers {
			if pub.PublishedPort == 0 {
				continue
			}
			protocol := pub.Protocol
			if protocol == "" {
				protocol = "tcp"
			}
			ports = append(ports, PublishedPort{
				Service:       entry.Service,
				ContainerPort: pub.TargetPort,
				HostPort:      pub.PublishedPort,
				Protocol:      protocol,
			})
		}
	}
	return ports, nil
}

// ParsePortMapping parses "3000" or "8080:3000" into host and container ports.
// With a single value the container port is returned and host is 0, meaning
// "let Docker choose".
func ParsePortMapping(spec string) (host, container int, err error) {
	parts := strings.Split(spec, ":")
	switch len(parts) {
	case 1:
		container, err = strconv.Atoi(parts[0])
		return 0, container, err
	case 2:
		if host, err = strconv.Atoi(parts[0]); err != nil {
			return 0, 0, err
		}
		container, err = strconv.Atoi(parts[1])
		return host, container, err
	default:
		return 0, 0, fmt.Errorf("unsupported port mapping %q, want \"container\" or \"host:container\"", spec)
	}
}
