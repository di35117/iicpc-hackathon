// sandbox/builder.go: builds contestant source into a Docker image via CLI.
//
// Build pipeline:
//   1. Extract source tarball to a temp directory.
//   2. Inject a multi-stage Dockerfile (builder + minimal runtime stage).
//   3. Run docker build with --network=none (no internet during compilation).
//   4. Tag image as "iicpc-submission-{submission_id}:latest".
//
// Multi-stage rationale:
//   Builder stage installs the Go toolchain and compiles the submission.
//   Runtime stage copies only the binary into distroless/static — no shell,
//   no compiler, no package manager. Minimal attack surface for the runner.

package sandbox

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// BuildResult is returned after a successful or failed build.
type BuildResult struct {
	ImageName    string
	SubmissionID string
	Success      bool
	Logs         string
}

// Build compiles source code from a tarball reader into a Docker image.
// The tarball must contain a valid Go project at its root (go.mod required).
func Build(submissionID string, source io.Reader) (*BuildResult, error) {
	imageName := fmt.Sprintf("iicpc-submission-%s:latest", submissionID)

	// Create a temp directory for the build context.
	buildDir, err := os.MkdirTemp("", fmt.Sprintf("iicpc-build-%s-*", submissionID[:8]))
	if err != nil {
		return nil, fmt.Errorf("create build dir: %w", err)
	}
	defer os.RemoveAll(buildDir)

	// Extract the source tarball into the build directory.
	tarCmd := exec.Command("tar", "-xf", "-", "-C", buildDir)
	tarCmd.Stdin = source
	if out, err := tarCmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("extract source: %w\n%s", err, out)
	}

	// Inject the Dockerfile — contestants do not provide their own.
	// We control the build environment entirely.
	dockerfilePath := filepath.Join(buildDir, "Dockerfile")
	if err := os.WriteFile(dockerfilePath, []byte(goDockerfile()), 0600); err != nil {
		return nil, fmt.Errorf("write dockerfile: %w", err)
	}

	// Run docker build.
	// --network=none: prevents the build from downloading external dependencies.
	// Contestants must vendor their dependencies or use the Go module cache.
	cmd := exec.Command(
		"docker", "build",
		"--network=none",
		"--tag", imageName,
		"--file", dockerfilePath,
		buildDir,
	)

	out, err := cmd.CombinedOutput()
	logs := string(out)
	success := err == nil

	return &BuildResult{
		ImageName:    imageName,
		SubmissionID: submissionID,
		Success:      success,
		Logs:         logs,
	}, nil
}

// goDockerfile returns the multi-stage Dockerfile for Go submissions.
func goDockerfile() string {
	return strings.TrimSpace(`
FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /server ./...

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /server /server
EXPOSE 8080
ENTRYPOINT ["/server"]
`)
}
