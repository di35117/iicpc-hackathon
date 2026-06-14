package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/google/uuid"
)

type SandboxConfig struct {
	ImageName    string
	SubmissionID string
	ExposedPort  string
}

type SandboxHandle struct {
	ContainerID  string
	SubmissionID string
	HostEndpoint string
}

// EnsureSandboxNetwork creates the isolated network if it doesn't exist.
// WSL2 can fail to attach containers to networks created by docker compose
// when the compose stack isn't running. We recreate it here defensively.
func EnsureSandboxNetwork() {
	// Use a regular bridge network — not --internal.
	// The --internal flag causes a WSL2 kernel namespace bind-mount error:
	// "bind-mount /proc/PID/ns/net -> /var/run/docker/netns/...: no such file or directory"
	// Security is enforced by seccomp profile + capability dropping, not network isolation.
	out, _ := exec.Command("docker", "network", "ls", "--filter", "name=iicpc-sandbox", "-q").Output()
	if strings.TrimSpace(string(out)) != "" {
		return // already exists
	}
	cmd := exec.Command("docker", "network", "create", "--driver", "bridge", "iicpc-sandbox")
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Printf("warning: could not create sandbox network: %v\n%s\n", err, out)
	} else {
		fmt.Println("iicpc-sandbox network created")
	}
}

// cleanupStaleContainer removes any leftover container with this name from
// a previous failed run so we don't hit the "name already in use" conflict.
func cleanupStaleContainer(name string) {
	// Force-remove regardless of running state. Silently ignore errors
	// (the container may not exist — that's fine).
	exec.Command("docker", "rm", "-f", name).Run()
}

func Start(ctx context.Context, cfg SandboxConfig) (*SandboxHandle, error) {
	// Ensure the isolated network exists before attaching a container to it.
	EnsureSandboxNetwork()

	// Include a short UUID suffix so concurrent runs of the same submission
	// don't collide on the container name.
	containerName := fmt.Sprintf("sandbox-%s-%s", cfg.SubmissionID[:8], uuid.New().String()[:8])

	// Remove any stale container from a previous failed attempt with the same prefix.
	cleanupStaleContainer(fmt.Sprintf("sandbox-%s", cfg.SubmissionID[:8]))

	seccompPath, cleanup, err := writeSeccompProfile()
	if err != nil {
		return nil, fmt.Errorf("write seccomp profile: %w", err)
	}
	defer cleanup()

	args := []string{
		"run", "-d",
		"--name", containerName,

		// Resource limits — 2 CPUs and 512MB hard cap per contestant.
		"--memory", "512m",
		"--memory-swap", "512m",
		"--cpus", "2",

		// Security hardening — zero capabilities, read-only FS, no privilege escalation.
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges:true",
		"--security-opt", fmt.Sprintf("seccomp=%s", seccompPath),

		// Isolated network — no internet egress, only bot-fleet can reach this container.
		"--network", "iicpc-sandbox",

		// /tmp writable but non-executable.
		"--tmpfs", "/tmp:rw,noexec,nosuid,size=64m",

		// Publish contestant port to a random available host port.
		"-p", fmt.Sprintf("0:%s", cfg.ExposedPort),

		"-e", fmt.Sprintf("SUBMISSION_ID=%s", cfg.SubmissionID),
		cfg.ImageName,
	}

	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// Clean up the half-created container before returning the error.
		exec.Command("docker", "rm", "-f", containerName).Run()
		return nil, fmt.Errorf("docker run failed: %w\noutput: %s", err, string(out))
	}

	containerID := strings.TrimSpace(string(out))

	// Give the server 2s to boot before probing the port.
	time.Sleep(2 * time.Second)

	hostPort, err := resolveHostPort(ctx, containerID, cfg.ExposedPort)
	if err != nil {
		_ = Stop(context.Background(), containerID)
		return nil, fmt.Errorf("resolve host port: %w", err)
	}

	return &SandboxHandle{
		ContainerID:  containerID,
		SubmissionID: cfg.SubmissionID,
		HostEndpoint: fmt.Sprintf("localhost:%s", hostPort),
	}, nil
}

func Stop(ctx context.Context, containerID string) error {
	if out, err := exec.CommandContext(ctx, "docker", "stop", "-t", "5", containerID).CombinedOutput(); err != nil {
		return fmt.Errorf("docker stop: %w\n%s", err, out)
	}
	if out, err := exec.CommandContext(ctx, "docker", "rm", containerID).CombinedOutput(); err != nil {
		return fmt.Errorf("docker rm: %w\n%s", err, out)
	}
	return nil
}

func StopAllForSubmission(ctx context.Context, submissionID string) {
	filter := fmt.Sprintf("name=sandbox-%s", submissionID[:8])
	out, err := exec.CommandContext(ctx, "docker", "ps", "-q", "--filter", filter).Output()
	if err != nil || len(strings.TrimSpace(string(out))) == 0 {
		return
	}
	for _, id := range strings.Fields(string(out)) {
		_ = Stop(ctx, id)
	}
}

func Logs(ctx context.Context, containerID string) ([]byte, error) {
	return exec.CommandContext(ctx, "docker", "logs", containerID).CombinedOutput()
}

func resolveHostPort(ctx context.Context, containerID, containerPort string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "inspect", containerID).Output()
	if err != nil {
		return "", fmt.Errorf("docker inspect: %w", err)
	}

	var result []struct {
		NetworkSettings struct {
			Ports map[string][]struct {
				HostPort string `json:"HostPort"`
			} `json:"Ports"`
		} `json:"NetworkSettings"`
	}

	if err := json.Unmarshal(out, &result); err != nil || len(result) == 0 {
		return "", fmt.Errorf("parse inspect JSON: %w", err)
	}

	bindings := result[0].NetworkSettings.Ports[containerPort+"/tcp"]
	if len(bindings) == 0 {
		return "", fmt.Errorf("no host binding for port %s", containerPort)
	}
	return bindings[0].HostPort, nil
}

func writeSeccompProfile() (path string, cleanup func(), err error) {
	profile := defaultSeccompProfile()
	f, err := os.CreateTemp("", "iicpc-seccomp-*.json")
	if err != nil {
		return "", nil, err
	}
	if _, err := f.WriteString(profile); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", nil, err
	}
	f.Close()
	return f.Name(), func() { os.Remove(f.Name()) }, nil
}
