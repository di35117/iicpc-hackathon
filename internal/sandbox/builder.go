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

func Build(submissionID string, filename string, source io.Reader) (*BuildResult, error) {
	imageName := fmt.Sprintf("iicpc-submission-%s:latest", submissionID)

	buildDir, err := os.MkdirTemp("", fmt.Sprintf("iicpc-build-%s-*", submissionID[:8]))
	if err != nil {
		return nil, fmt.Errorf("create build dir: %w", err)
	}
	defer os.RemoveAll(buildDir)

	// --- UNIFIED FILE HANDLING: Tarballs vs Raw Source ---
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
		// Save raw file directly into build context
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
	case "rust":  dockerfileContent = rustDockerfile()
	case "cpp":   dockerfileContent = cppDockerfile()
	default:      dockerfileContent = goDockerfile()
	}

	dockerfilePath := filepath.Join(buildDir, "Dockerfile")
	_ = os.WriteFile(dockerfilePath, []byte(dockerfileContent), 0600)

	cmd := exec.Command("docker", "build", "--network=none", "--tag", imageName, "--file", dockerfilePath, buildDir)
	out, err := cmd.CombinedOutput()
	return &BuildResult{ImageName: imageName, SubmissionID: submissionID, Success: err == nil, Logs: string(out)}, nil
}

func detectLanguage(buildDir string) string {
	lang := "go"
	_ = filepath.Walk(buildDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() { return nil }
		name := strings.ToLower(info.Name())
		if name == "cargo.toml" || strings.HasSuffix(name, ".rs") { lang = "rust"; return io.EOF }
		if name == "cmakelists.txt" || strings.HasSuffix(name, ".cpp") || strings.HasSuffix(name, ".cc") { lang = "cpp"; return io.EOF }
		return nil
	})
	return lang
}

func goDockerfile() string { return "FROM golang:1.22-alpine AS builder\nWORKDIR /src\nCOPY . .\nRUN CGO_ENABLED=0 go build -o /server ./...\nFROM gcr.io/distroless/static:nonroot\nCOPY --from=builder /server /server\nEXPOSE 8080\nENTRYPOINT [\"/server\"]" }
func rustDockerfile() string { return "FROM rust:1.77-alpine AS builder\nRUN apk add --no-cache musl-dev\nWORKDIR /src\nCOPY . .\nRUN if [ -f \"Cargo.toml\" ]; then cargo build --release --offline && find target/release -maxdepth 1 -type f -perm -111 -exec cp {} /server \\; ; else rustc -C target-feature=+crt-static -O *.rs -o /server ; fi\nFROM gcr.io/distroless/static:nonroot\nCOPY --from=builder /server /server\nEXPOSE 8080\nENTRYPOINT [\"/server\"]" }
func cppDockerfile() string { return "FROM alpine:3.19 AS builder\nRUN apk add --no-cache g++ cmake make\nWORKDIR /src\nCOPY . .\nRUN if [ -f \"CMakeLists.txt\" ]; then mkdir build && cd build && cmake .. && make && find . -maxdepth 1 -type f -perm -111 -exec cp {} /server \\; ; elif [ -f \"Makefile\" ]; then make && find . -maxdepth 1 -type f -perm -111 -exec cp {} /server \\; ; else g++ -O3 -static *.cpp -o /server ; fi\nFROM gcr.io/distroless/static:nonroot\nCOPY --from=builder /server /server\nEXPOSE 8080\nENTRYPOINT [\"/server\"]" }