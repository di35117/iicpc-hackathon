// sandbox/builder.go: builds contestant source into a Docker image via CLI.
//
// Build pipeline:
//   1. Extract source tarball to a temp directory.
//   2. Detect language (Go, Rust, C++) based on file extensions and build files.
//   3. Inject the corresponding multi-stage Dockerfile (builder + minimal runtime stage).
//   4. Run docker build with --network=none (no internet during compilation).
//   5. Tag image as "iicpc-submission-{submission_id}:latest".
//
// Multi-stage rationale:
//   Builder stage installs the language toolchain and compiles the submission.
//   Runtime stage copies only the static binary into distroless/static — no shell,
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

	// Detect language to inject the correct Dockerfile
	lang := detectLanguage(buildDir)
	var dockerfileContent string
	
	switch lang {
	case "rust":
		dockerfileContent = rustDockerfile()
	case "cpp":
		dockerfileContent = cppDockerfile()
	default:
		dockerfileContent = goDockerfile()
	}

	// Inject the Dockerfile — contestants do not provide their own.
	dockerfilePath := filepath.Join(buildDir, "Dockerfile")
	if err := os.WriteFile(dockerfilePath, []byte(dockerfileContent), 0600); err != nil {
		return nil, fmt.Errorf("write dockerfile: %w", err)
	}

	// Run docker build.
	// --network=none: prevents the build from downloading external dependencies.
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

// detectLanguage walks the extracted files and returns "go", "rust", or "cpp".
func detectLanguage(buildDir string) string {
	lang := "go" // default
	
	filepath.Walk(buildDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		
		ext := strings.ToLower(filepath.Ext(info.Name()))
		name := strings.ToLower(info.Name())
		
		// Priority checks
		if name == "cargo.toml" || ext == ".rs" {
			lang = "rust"
			return io.EOF // Stop walking
		} else if name == "cmakelists.txt" || ext == ".cpp" || ext == ".cc" {
			lang = "cpp"
			return io.EOF
		} else if name == "go.mod" || ext == ".go" {
			lang = "go"
			return io.EOF
		}
		return nil
	})
	
	return lang
}

// goDockerfile returns the multi-stage Dockerfile for Go submissions.
func goDockerfile() string {
	return strings.TrimSpace(`
FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /server ./...

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /server /server
EXPOSE 8080
ENTRYPOINT ["/server"]
`)
}

// rustDockerfile returns the multi-stage Dockerfile for Rust submissions.
func rustDockerfile() string {
	return strings.TrimSpace(`
FROM rust:1.77-alpine AS builder
RUN apk add --no-cache musl-dev
WORKDIR /src
COPY . .
# Build offline. Finds the output executable and moves it to /server.
RUN if [ -f "Cargo.toml" ]; then \
        cargo build --release --offline && \
        find target/release -maxdepth 1 -type f -perm -111 -exec cp {} /server \; ; \
    else \
        rustc -C target-feature=+crt-static -O *.rs -o /server ; \
    fi

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /server /server
EXPOSE 8080
ENTRYPOINT ["/server"]
`)
}

// cppDockerfile returns the multi-stage Dockerfile for C++ submissions.
func cppDockerfile() string {
	return strings.TrimSpace(`
FROM alpine:3.19 AS builder
RUN apk add --no-cache g++ cmake make
WORKDIR /src
COPY . .
# Supports CMake, Make, or raw g++ static compilation
RUN if [ -f "CMakeLists.txt" ]; then \
        mkdir build && cd build && cmake .. && make && \
        find . -maxdepth 1 -type f -perm -111 -exec cp {} /server \; ; \
    elif [ -f "Makefile" ]; then \
        make && \
        find . -maxdepth 1 -type f -perm -111 -exec cp {} /server \; ; \
    else \
        g++ -O3 -static *.cpp -o /server ; \
    fi

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /server /server
EXPOSE 8080
ENTRYPOINT ["/server"]
`)
}