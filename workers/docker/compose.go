package docker

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// ComposeFile is the compose file the worker looks for inside a project
// directory.
const ComposeFile = "docker-compose.yml"

// ComposeUp brings the compose project in dir up with `docker compose up
// --build`, streaming the build output and container logs to stdout/stderr.
//
// projectName scopes the compose project, which keeps container and network
// names from colliding when two workers share a Docker host. It is ignored when
// empty, letting compose fall back to the directory name.
//
// Unlike the rest of this package, which talks to the daemon over the Docker
// SDK, compose is a Docker CLI plugin so this shells out to the docker binary.
// The call blocks until the containers exit or ctx is cancelled.
func ComposeUp(ctx context.Context, dir, projectName string) error {
	composeFile := filepath.Join(dir, ComposeFile)
	if _, err := os.Stat(composeFile); err != nil {
		return fmt.Errorf("looking for %s in %s: %w", ComposeFile, dir, err)
	}

	args := []string{"compose"}
	if projectName != "" {
		args = append(args, "-p", projectName)
	}
	args = append(args, "-f", composeFile, "up", "--build")

	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return fmt.Errorf("docker compose up in %s: %w", dir, err)
	}
	return nil
}
