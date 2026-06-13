package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
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

func Start(ctx context.Context, cfg SandboxConfig) (*SandboxHandle, error) {
	containerName := fmt.Sprintf("sandbox-%s", cfg.SubmissionID[:8])

	// Removed the custom seccomp profile. Docker's default seccomp profile 
	// is automatically applied and works perfectly alongside cap-drop.
	args := []string{
		"run", "-d",
		"--name", containerName,
		"--memory", "512m",
		"--memory-swap", "512m",
		"--cpus", "2",
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges:true",
		"--network", "sandbox_isolated",
		"--tmpfs", "/tmp:rw,noexec,nosuid,size=64m",
		"-p", fmt.Sprintf("0:%s", cfg.ExposedPort),
		"-e", fmt.Sprintf("SUBMISSION_ID=%s", cfg.SubmissionID),
		cfg.ImageName,
	}

	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("docker run failed: %w\noutput: %s", err, string(out))
	}

	containerID := strings.TrimSpace(string(out))
	
	// Give the server a moment to boot before resolving ports
	time.Sleep(2 * time.Second)

	hostPort, err := resolveHostPort(ctx, containerID, cfg.ExposedPort)
	if err != nil {
		_ = Stop(context.Background(), containerID)
		return nil, fmt.Errorf("resolve port: %w", err)
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
