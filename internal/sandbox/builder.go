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
//
// Performance strategy: all base images are pre-built and cached locally.
//   iicpc-builder-go:latest    — golang:1.25-alpine + git
//   iicpc-builder-cpp:latest   — alpine + g++ cmake make
//   iicpc-builder-rust:latest  — rust:alpine + musl-dev
//
// This means contestant builds never pull packages from the internet.
// A typical build takes 2-4s (just the compile step on cached layers).
//
// Network policy:
//   Go:       --network=none  (go build uses local module cache, no downloads)
//   C++/Rust: --network=none  (compilers already in cached image, no apk needed)
func Build(submissionID string, filename string, source io.Reader) (*BuildResult, error) {
	imageName := fmt.Sprintf("iicpc-submission-%s:latest", submissionID)

	buildDir, err := os.MkdirTemp("", fmt.Sprintf("iicpc-build-%s-*", submissionID[:8]))
	if err != nil {
		return nil, fmt.Errorf("create build dir: %w", err)
	}
	defer os.RemoveAll(buildDir)

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

	// All builds now use --network=none because compilers are in cached base images.
	// No package downloads happen at build time — everything is already local.
	cmd := exec.Command(
		"docker", "build",
		"--network=none",
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
FROM iicpc-builder-go:latest AS builder
WORKDIR /src
COPY . .
RUN if [ ! -f go.mod ]; then go mod init submission; fi
RUN CGO_ENABLED=0 go build -o /server ./...

FROM alpine:3.19
RUN adduser -D nonroot
USER nonroot
# Force ownership to the unprivileged user
COPY --from=builder --chown=nonroot:nonroot /server /server
EXPOSE 8080
ENTRYPOINT ["/server"]
`)
}

func rustDockerfile() string {
	return strings.TrimSpace(`
FROM iicpc-builder-rust:latest AS builder
WORKDIR /src
COPY . .
RUN if [ -f "Cargo.toml" ]; then \
      cargo build --release && \
      find target/release -maxdepth 1 -type f -perm -111 -exec cp {} /server \; ; \
    else \
      rustc -C target-feature=+crt-static -O *.rs -o /server ; \
    fi

FROM alpine:3.19
RUN adduser -D nonroot
USER nonroot
COPY --from=builder /server /server
EXPOSE 8080
ENTRYPOINT ["/server"]
`)
}

func cppDockerfile() string {
	return strings.TrimSpace(`
FROM iicpc-builder-cpp:latest AS builder
WORKDIR /src
COPY . .
RUN if [ -f "CMakeLists.txt" ]; then \
      mkdir build && cd build && cmake .. && make && \
      find . -maxdepth 1 -type f -perm -111 -exec cp {} /server \; ; \
    elif [ -f "Makefile" ]; then \
      make && find . -maxdepth 1 -type f -perm -111 -exec cp {} /server \; ; \
    else \
      g++ -O3 -static *.cpp *.cc *.c -o /server 2>/dev/null || \
      g++ -O3 -static *.cpp -o /server 2>/dev/null || \
      g++ -O3 -static *.c -o /server ; \
    fi

FROM alpine:3.19
# Completely offline stage. No apk add needed because the binary is static.
RUN adduser -D nonroot
USER nonroot
COPY --from=builder /server /server
EXPOSE 8080
ENTRYPOINT ["/server"]
`)
}