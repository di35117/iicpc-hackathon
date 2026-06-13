// sandbox/builder.go: builds contestant source code into a runnable Docker image.
//
// Build pipeline:
//   1. Download source tarball from MinIO.
//   2. Inject a multi-stage Dockerfile (builder stage + minimal runtime stage).
//   3. Run docker build inside an isolated build container.
//   4. Tag the output image as "iicpc-submission-{submission_id}:latest".
//
// Why multi-stage builds?
//   The builder stage installs compilers and runs untrusted code (the build itself).
//   The runtime stage copies only the compiled binary into a scratch/distroless image.
//   This means the final image has no compiler, no shell, and minimal attack surface.

package sandbox

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
)

// Builder compiles contestant source code into Docker images.
type Builder struct {
	docker *client.Client
}

// NewBuilder creates a Builder connected to the local Docker daemon.
func NewBuilder() (*Builder, error) {
	cli, err := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("docker client init: %w", err)
	}
	return &Builder{docker: cli}, nil
}

// BuildResult contains the outcome of a build operation.
type BuildResult struct {
	ImageName    string
	SubmissionID string
	Success      bool
	Logs         string
}

// Build takes a source tarball reader and compiles it into a Docker image.
// The tarball must contain a valid Go, C++, or Rust project at its root.
// Returns the image name to pass to Runner.Start().
func (b *Builder) Build(ctx context.Context, submissionID string, source io.Reader) (*BuildResult, error) {
	imageName := fmt.Sprintf("iicpc-submission-%s:latest", submissionID)

	// Read the source tarball into memory so we can append the Dockerfile.
	sourceBytes, err := io.ReadAll(source)
	if err != nil {
		return nil, fmt.Errorf("read source: %w", err)
	}

	// Inject our Dockerfile into the build context.
	// Contestants do not provide their own Dockerfile — we control the build.
	buildContext, err := injectDockerfile(sourceBytes, goDockerfile())
	if err != nil {
		return nil, fmt.Errorf("inject dockerfile: %w", err)
	}

	buildResp, err := b.docker.ImageBuild(ctx, buildContext, types.ImageBuildOptions{
		Tags:        []string{imageName},
		Dockerfile:  "Dockerfile",
		Remove:      true,  // remove intermediate containers after build
		ForceRemove: true,
		// No network access during build — contestants cannot fetch external deps.
		NetworkMode: "none",
	})
	if err != nil {
		return nil, fmt.Errorf("image build for submission %s: %w", submissionID, err)
	}
	defer buildResp.Body.Close()

	// Read the full build log. Docker streams JSON-encoded log lines.
	logBytes, err := io.ReadAll(buildResp.Body)
	if err != nil {
		return nil, fmt.Errorf("read build logs: %w", err)
	}
	logs := string(logBytes)

	// A failed build shows "error" in the output stream.
	success := !strings.Contains(logs, `"error"`)

	return &BuildResult{
		ImageName:    imageName,
		SubmissionID: submissionID,
		Success:      success,
		Logs:         logs,
	}, nil
}

// goDockerfile returns the multi-stage Dockerfile for Go submissions.
// Stage 1 (builder): compiles the source with the Go toolchain.
// Stage 2 (runtime): copies the binary into a minimal distroless image.
// Network is disabled during build so contestants cannot pull external packages.
func goDockerfile() string {
	return `
# Stage 1: compile
FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY . .
# Disable CGO for a fully static binary — no libc dependency in the runtime image.
RUN CGO_ENABLED=0 go build -o /server ./...

# Stage 2: minimal runtime
# distroless/static has no shell, no package manager, no compilers.
# Attack surface is the binary and nothing else.
FROM gcr.io/distroless/static:nonroot
COPY --from=builder /server /server
EXPOSE 8080
ENTRYPOINT ["/server"]
`
}

// injectDockerfile takes an existing tar archive (source) and appends a Dockerfile
// entry to it, returning the modified archive as a reader.
// This lets us control the build process without requiring contestants to provide
// their own Dockerfile.
func injectDockerfile(sourceTar []byte, dockerfile string) (io.Reader, error) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)

	// Copy all existing entries from the source tarball.
	tr := tar.NewReader(bytes.NewReader(sourceTar))
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read source tar: %w", err)
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return nil, err
		}
		if _, err := io.Copy(tw, tr); err != nil {
			return nil, err
		}
	}

	// Append the Dockerfile.
	dfBytes := []byte(dockerfile)
	if err := tw.WriteHeader(&tar.Header{
		Name: "Dockerfile",
		Mode: 0600,
		Size: int64(len(dfBytes)),
	}); err != nil {
		return nil, fmt.Errorf("write dockerfile header: %w", err)
	}
	if _, err := tw.Write(dfBytes); err != nil {
		return nil, fmt.Errorf("write dockerfile content: %w", err)
	}

	if err := tw.Close(); err != nil {
		return nil, err
	}

	return &buf, nil
}
