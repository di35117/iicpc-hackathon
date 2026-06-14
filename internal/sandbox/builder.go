package sandbox

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type BuildResult struct {
	ImageName    string
	SubmissionID string
	Success      bool
	Logs         string
}

// Build compiles contestant source into a Docker image.
// Network access during build is allowed for C++ and Rust (need apk/apt to install
// compilers), but blocked for Go (uses local module cache — no downloads needed).
// Network isolation is enforced at RUNTIME by the sandbox runner, not at build time.
func Build(submissionID string, filename string, source io.Reader) (*BuildResult, error) {
	imageName := fmt.Sprintf("iicpc-submission-%s:latest", submissionID)

	buildDir, err := os.MkdirTemp("", fmt.Sprintf("iicpc-build-%s-*", submissionID[:8]))
	if err != nil {
		return nil, fmt.Errorf("create build dir: %w", err)
	}
	defer os.RemoveAll(buildDir)

	// Handle both tarballs and raw source files.
	isTarball := strings.HasSuffix(strings.ToLower(filename), ".tar.gz") ||
		strings.HasSuffix(strings.ToLower(filename), ".zip") ||
		strings.HasSuffix(strings.ToLower(filename), ".tar")

	if isTarball {
		tarCmd := exec.Command("tar", "-xf", "-", "-C", buildDir)
		tarCmd.Stdin = source
		if out, err := tarCmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("extract source: %w\n%s", err, out)
		}
	} else {
		destPath := filepath.Join(buildDir, filename)
		outFile, err := os.Create(destPath)
		if err != nil {
			return nil, fmt.Errorf("save raw file: %w", err)
		}
		_, _ = io.Copy(outFile, source)
		outFile.Close()
	}

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

	dockerfilePath := filepath.Join(buildDir, "Dockerfile")
	if err := os.WriteFile(dockerfilePath, []byte(dockerfileContent), 0600); err != nil {
		return nil, fmt.Errorf("write dockerfile: %w", err)
	}

	// Go: --network=none because go build uses the local module cache.
	// C++/Rust: network allowed so apk/cargo can install compilers and deps.
	// The container is network-isolated at RUNTIME by the sandbox runner.
	networkFlag := "--network=none"
	if lang == "cpp" || lang == "rust" {
		networkFlag = "--network=default"
	}

	cmd := exec.Command(
		"docker", "build",
		networkFlag,
		"--tag", imageName,
		"--file", dockerfilePath,
		buildDir,
	)

	out, err := cmd.CombinedOutput()
	return &BuildResult{
		ImageName:    imageName,
		SubmissionID: submissionID,
		Success:      err == nil,
		Logs:         string(out),
	}, nil
}

func detectLanguage(buildDir string) string {
	lang := "go"
	_ = filepath.Walk(buildDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		name := strings.ToLower(info.Name())
		if name == "cargo.toml" || strings.HasSuffix(name, ".rs") {
			lang = "rust"
			return io.EOF
		}
		if name == "cmakelists.txt" || strings.HasSuffix(name, ".cpp") ||
			strings.HasSuffix(name, ".cc") || strings.HasSuffix(name, ".c") {
			lang = "cpp"
			return io.EOF
		}
		return nil
	})
	return lang
}

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

func rustDockerfile() string {
	return strings.TrimSpace(`
FROM rust:alpine AS builder
RUN apk add --no-cache musl-dev
WORKDIR /src
COPY . .
RUN if [ -f "Cargo.toml" ]; then \
      cargo build --release && \
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

func cppDockerfile() string {
	return strings.TrimSpace(`
FROM alpine:latest AS builder
RUN apk add --no-cache g++ cmake make
WORKDIR /src
COPY . .
RUN if [ -f "CMakeLists.txt" ]; then \
      mkdir build && cd build && cmake .. && make && \
      find . -maxdepth 1 -type f -perm -111 -exec cp {} /server \; ; \
    elif [ -f "Makefile" ]; then \
      make && find . -maxdepth 1 -type f -perm -111 -exec cp {} /server \; ; \
    else \
      g++ -O3 -static *.cpp *.cc *.c -o /server 2>/dev/null || g++ -O3 *.cpp -o /server ; \
    fi

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /server /server
EXPOSE 8080
ENTRYPOINT ["/server"]
`)
}
