// Package docker wraps the official Docker Go SDK so the worker can build
// images from project directories stored locally and run them as containers.
package docker

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/docker/docker/api/types/build"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/stdcopy"
)

// Client is a thin wrapper around the official Docker Go SDK.
type Client struct {
	cli *client.Client
}

// New connects to the Docker daemon. It honours the standard DOCKER_HOST,
// DOCKER_TLS_VERIFY and DOCKER_CERT_PATH environment variables and negotiates
// the API version with whatever daemon it talks to.
func New() (*Client, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("creating docker client: %w", err)
	}
	return &Client{cli: cli}, nil
}

// Close releases the underlying client.
func (c *Client) Close() error {
	return c.cli.Close()
}

// Ping verifies that the Docker daemon is reachable.
func (c *Client) Ping(ctx context.Context) error {
	if _, err := c.cli.Ping(ctx); err != nil {
		return fmt.Errorf("pinging docker daemon: %w", err)
	}
	return nil
}

// BuildImage builds an image from the Dockerfile in dir and tags it as tag.
// The build log is streamed to stdout so progress is visible.
func (c *Client) BuildImage(ctx context.Context, dir, tag string) error {
	buildCtx, err := tarDir(dir)
	if err != nil {
		return fmt.Errorf("packing build context %s: %w", dir, err)
	}
	defer buildCtx.Close()

	resp, err := c.cli.ImageBuild(ctx, buildCtx, build.ImageBuildOptions{
		Tags:       []string{tag},
		Dockerfile: "Dockerfile",
		Remove:     true,
	})
	if err != nil {
		return fmt.Errorf("building image %s: %w", tag, err)
	}
	defer resp.Body.Close()

	// The build only completes once the response body has been fully drained.
	if _, err := io.Copy(os.Stdout, resp.Body); err != nil {
		return fmt.Errorf("reading build output for %s: %w", tag, err)
	}
	return nil
}

// PullImage makes sure the given image reference exists locally.
func (c *Client) PullImage(ctx context.Context, ref string) error {
	reader, err := c.cli.ImagePull(ctx, ref, image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pulling image %s: %w", ref, err)
	}
	defer reader.Close()

	if _, err := io.Copy(io.Discard, reader); err != nil {
		return fmt.Errorf("reading pull output for %s: %w", ref, err)
	}
	return nil
}

// RunContainer creates and starts a container from imageTag using the given
// name, returning the container ID.
func (c *Client) RunContainer(ctx context.Context, imageTag, name string) (string, error) {
	created, err := c.cli.ContainerCreate(ctx,
		&container.Config{
			Image: imageTag,
		},
		&container.HostConfig{},
		nil,
		nil,
		name,
	)
	if err != nil {
		return "", fmt.Errorf("creating container %s: %w", name, err)
	}
	if err := c.cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		return created.ID, fmt.Errorf("starting container %s: %w", name, err)
	}
	return created.ID, nil
}

// RemoveContainer force-removes a container by name or ID. Removing something
// that does not exist is not an error.
func (c *Client) RemoveContainer(ctx context.Context, nameOrID string) error {
	err := c.cli.ContainerRemove(ctx, nameOrID, container.RemoveOptions{Force: true})
	if err != nil && !errdefs.IsNotFound(err) {
		return fmt.Errorf("removing container %s: %w", nameOrID, err)
	}
	return nil
}

// Logs streams a container's stdout and stderr to w until the container exits
// or ctx is cancelled.
func (c *Client) Logs(ctx context.Context, id string, w io.Writer) error {
	reader, err := c.cli.ContainerLogs(ctx, id, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Follow:     true,
	})
	if err != nil {
		return fmt.Errorf("fetching logs for %s: %w", id, err)
	}
	defer reader.Close()

	// Container logs are multiplexed with an 8 byte header per frame unless the
	// container was started with a TTY, so demux them back into two streams.
	if _, err := stdcopy.StdCopy(w, w, reader); err != nil {
		return fmt.Errorf("reading logs for %s: %w", id, err)
	}
	return nil
}

// tarDir packs dir into an in-memory tar stream suitable for use as a Docker
// build context. Paths are relative to dir and .git is skipped.
func tarDir(dir string) (io.ReadCloser, error) {
	info, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}

	pr, pw := io.Pipe()

	go func() {
		tw := tar.NewWriter(pw)

		walkErr := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(dir, path)
			if err != nil {
				return err
			}
			if rel == "." {
				return nil
			}
			rel = filepath.ToSlash(rel)
			if rel == ".git" || strings.HasPrefix(rel, ".git/") {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}

			hdr, err := tar.FileInfoHeader(info, "")
			if err != nil {
				return err
			}
			hdr.Name = rel
			if info.IsDir() {
				hdr.Name += "/"
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return nil
			}

			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()

			_, err = io.Copy(tw, f)
			return err
		})

		if walkErr != nil {
			pw.CloseWithError(walkErr)
			return
		}
		if err := tw.Close(); err != nil {
			pw.CloseWithError(err)
			return
		}
		pw.Close()
	}()

	return pr, nil
}
