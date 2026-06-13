// sandbox/runner.go: spawns and manages contestant containers via Docker CLI.
//
// We call Docker CLI commands via os/exec rather than the Docker Go SDK.
// This avoids the +incompatible module issue with docker/docker in Go 1.25,
// and produces code where every container operation maps to an explicit,
// auditable shell command — easier to reason about and explain.
//
// Security model enforced via CLI flags:
//   --memory / --memory-swap   hard memory cap, swap disabled
//   --cpus                     CPU quota (2 cores max)
//   --read-only                immutable root filesystem
//   --cap-drop ALL             zero Linux capabilities
//   --security-opt             no-new-privileges + seccomp profile
//   --network sandbox_isolated no internet egress

package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// SandboxConfig holds parameters for a single contestant sandbox.
type SandboxConfig struct {
	ImageName    string // "iicpc-submission-{id}:latest"
	SubmissionID string
	ExposedPort  string // port the contestant server listens on, e.g. "8080"
}

// SandboxHandle is returned after a container starts successfully.
type SandboxHandle struct {
	ContainerID  string
	SubmissionID string
	HostEndpoint string // "localhost:PORT" — routable from the bot fleet
}

// Start creates and runs a fully isolated contestant container.
// Returns a handle containing the host endpoint the bot fleet should target.
func Start(ctx context.Context, cfg SandboxConfig) (*SandboxHandle, error) {
	containerName := fmt.Sprintf("sandbox-%s", cfg.SubmissionID[:8])

	// Write the seccomp profile to a temp file Docker can read.
	seccompPath, cleanup, err := writeSeccompProfile()
	if err != nil {
		return nil, fmt.Errorf("write seccomp profile: %w", err)
	}
	defer cleanup()

	// Build the docker run command with all security and resource constraints.
	// Each flag is chosen deliberately — see comments inline.
	args := []string{
		"run", "-d",                  // detached mode
		"--name", containerName,

		// Resource limits
		"--memory", "512m",           // hard memory cap
		"--memory-swap", "512m",      // equal to memory = swap disabled
		"--cpus", "2",                // max 2 CPU cores via cgroup quota

		// Security
		"--read-only",                // immutable root filesystem
		"--cap-drop", "ALL",          // drop every Linux capability
		"--security-opt", "no-new-privileges:true",
		"--security-opt", fmt.Sprintf("seccomp=%s", seccompPath),

		// Network isolation — sandbox_isolated has internal:true in compose,
		// meaning no external routing. Bots reach the container via host port mapping.
		"--network", "sandbox_isolated",

		// Tmpfs for /tmp — contestants can write temp files but cannot execute them.
		"--tmpfs", fmt.Sprintf("/tmp:rw,noexec,nosuid,size=64m"),

		// Publish contestant's port to a random available host port.
		"-p", fmt.Sprintf("0:%s", cfg.ExposedPort),

		// Environment markers so the contestant's code knows its context.
		"-e", fmt.Sprintf("SUBMISSION_ID=%s", cfg.SubmissionID),

		cfg.ImageName,
	}

	cmd := exec.CommandContext(ctx, "docker", args...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("docker run failed: %w\noutput: %s", err, string(out))
	}

	// docker run -d prints the full container ID on stdout.
	containerID := strings.TrimSpace(string(out))

	// Give the server 2 seconds to start before we resolve the port.
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

// Stop terminates and removes a sandbox container.
func Stop(ctx context.Context, containerID string) error {
	// docker stop sends SIGTERM, waits 5s, then sends SIGKILL.
	stopCmd := exec.CommandContext(ctx, "docker", "stop", "-t", "5", containerID)
	if out, err := stopCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker stop %s: %w\n%s", containerID[:12], err, out)
	}

	rmCmd := exec.CommandContext(ctx, "docker", "rm", containerID)
	if out, err := rmCmd.CombinedOutput(); err != nil {
		return fmt.Errorf("docker rm %s: %w\n%s", containerID[:12], err, out)
	}

	return nil
}

// StopAllForSubmission removes every running sandbox for a submission.
func StopAllForSubmission(ctx context.Context, submissionID string) {
	// List containers whose names start with "sandbox-{id prefix}".
	filter := fmt.Sprintf("name=sandbox-%s", submissionID[:8])
	out, err := exec.CommandContext(ctx, "docker", "ps", "-q", "--filter", filter).Output()
	if err != nil || len(strings.TrimSpace(string(out))) == 0 {
		return
	}

	for _, id := range strings.Fields(string(out)) {
		_ = Stop(ctx, id)
	}
}

// resolveHostPort inspects the container JSON to find the host port
// Docker dynamically assigned when PublishAllPorts / -p 0:PORT is used.
func resolveHostPort(ctx context.Context, containerID, containerPort string) (string, error) {
	out, err := exec.CommandContext(ctx, "docker", "inspect", containerID).Output()
	if err != nil {
		return "", fmt.Errorf("docker inspect: %w", err)
	}

	// docker inspect returns a JSON array; unmarshal the first element.
	var inspectResult []struct {
		NetworkSettings struct {
			Ports map[string][]struct {
				HostPort string `json:"HostPort"`
			} `json:"Ports"`
		} `json:"NetworkSettings"`
	}

	if err := json.Unmarshal(out, &inspectResult); err != nil {
		return "", fmt.Errorf("parse inspect JSON: %w", err)
	}
	if len(inspectResult) == 0 {
		return "", fmt.Errorf("empty inspect result")
	}

	portKey := containerPort + "/tcp"
	bindings := inspectResult[0].NetworkSettings.Ports[portKey]
	if len(bindings) == 0 {
		return "", fmt.Errorf("no host binding for port %s", containerPort)
	}

	return bindings[0].HostPort, nil
}

// Logs returns the stdout/stderr of a running sandbox container.
func Logs(ctx context.Context, containerID string) ([]byte, error) {
	return exec.CommandContext(ctx, "docker", "logs", containerID).CombinedOutput()
}

// writeSeccompProfile writes the JSON seccomp profile to /tmp and returns
// the path, a cleanup function, and any error.
func writeSeccompProfile() (path string, cleanup func(), err error) {
	profile := defaultSeccompProfile()
	tmp := fmt.Sprintf("/tmp/iicpc-seccomp-%d.json", time.Now().UnixNano())

	cmd := exec.Command("sh", "-c", fmt.Sprintf("cat > %s << 'SECEOF'\n%s\nSECEOF", tmp, profile))
	var buf bytes.Buffer
	cmd.Stderr = &buf
	if err := cmd.Run(); err != nil {
		return "", nil, fmt.Errorf("write seccomp: %s", buf.String())
	}

	return tmp, func() {
		exec.Command("rm", "-f", tmp).Run()
	}, nil
}
