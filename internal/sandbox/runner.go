// sandbox/runner.go: spawns and manages contestant containers with strict isolation.
//
// Security model:
//   - CPU pinned via cgroup quota (2 vCPUs max per container)
//   - Memory hard-limited to 512MB with swap disabled
//   - All Linux capabilities dropped; none added back
//   - Filesystem mounted read-only; /tmp gets a small tmpfs (no exec)
//   - Network attached only to sandbox_isolated (no internet egress)
//   - no-new-privileges prevents privilege escalation via setuid binaries
//   - Seccomp profile blocks dangerous syscalls (fork bombs, raw sockets, ptrace)

package sandbox

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
	"github.com/google/uuid"
)

const (
	// sandboxNetwork is the isolated Docker network contestants run in.
	// Defined as internal:true in docker-compose so it has no external routing.
	sandboxNetwork = "sandbox_isolated"

	// Resource limits per contestant container.
	// CPU: 2 cores max (200000us quota per 100000us period).
	// Memory: 512MB hard limit, swap disabled (MemorySwap == Memory).
	cpuPeriodUS  = 100_000
	cpuQuotaUS   = 200_000
	memoryBytes  = 512 * 1024 * 1024
	tmpfsSizesMB = 64
)

// Runner manages the lifecycle of contestant sandbox containers.
type Runner struct {
	docker *client.Client
}

// NewRunner creates a Runner connected to the local Docker daemon.
func NewRunner() (*Runner, error) {
	cli, err := client.NewClientWithOpts(
		client.FromEnv,
		client.WithAPIVersionNegotiation(),
	)
	if err != nil {
		return nil, fmt.Errorf("docker client init: %w", err)
	}
	return &Runner{docker: cli}, nil
}

// SandboxConfig holds the parameters for a single contestant sandbox.
type SandboxConfig struct {
	// ImageName is the pre-built Docker image for this submission.
	// Format: "iicpc-submission-{submission_id}:latest"
	ImageName string

	// SubmissionID ties the container back to the submission record.
	SubmissionID string

	// ExposedPort is the port the contestant's server listens on inside the container.
	// The runner maps this to a random host port and returns the host address.
	ExposedPort string
}

// SandboxHandle is returned after a container is successfully started.
// The bot-fleet uses HostEndpoint to send orders to the contestant's engine.
type SandboxHandle struct {
	ContainerID  string
	SubmissionID string
	HostEndpoint string // e.g. "localhost:34521" — routable from the bot fleet
}

// Start pulls the image, creates a fully isolated container, and starts it.
// Returns a SandboxHandle the caller uses to direct bot traffic at the container.
func (r *Runner) Start(ctx context.Context, cfg SandboxConfig) (*SandboxHandle, error) {
	containerName := fmt.Sprintf("sandbox-%s-%s", cfg.SubmissionID[:8], uuid.New().String()[:8])

	// Host configuration enforces all resource and security constraints.
	hostCfg := &container.HostConfig{
		// Resource limits — prevents one contestant from starving others.
		Resources: container.Resources{
			CPUPeriod:  cpuPeriodUS,
			CPUQuota:   cpuQuotaUS,
			Memory:     memoryBytes,
			MemorySwap: memoryBytes, // MemorySwap == Memory disables swap entirely.
		},

		// Read-only root filesystem prevents the contestant from modifying system files.
		// /tmp is mounted separately as tmpfs so the process can still write temp files,
		// but with noexec so it cannot execute anything written there.
		ReadonlyRootfs: true,
		Tmpfs: map[string]string{
			"/tmp": fmt.Sprintf("rw,noexec,nosuid,size=%dm", tmpfsSizesMB),
		},

		// Drop ALL Linux capabilities. Contestants get zero elevated permissions.
		// This blocks: raw socket creation, mounting filesystems, ptrace, kill signals
		// to other containers, and dozens of other privileged operations.
		CapDrop: []string{"ALL"},

		// Security options:
		//   no-new-privileges: prevents gaining privileges via setuid/setgid binaries.
		//   seccomp: syscall filter — blocks fork bombs, perf_event_open, etc.
		SecurityOpt: []string{
			"no-new-privileges:true",
			"seccomp=" + defaultSeccompProfile(),
		},

		// Attach to the isolated network only. sandbox_isolated has internal:true
		// in docker-compose, meaning no routing to the internet or host services.
		NetworkMode: container.NetworkMode(sandboxNetwork),

		// Publish contestant's server port to a random host port.
		// Docker assigns the host port; we read it back after Start().
		PublishAllPorts: true,
	}

	// Container configuration — minimal, just the image and an env marker.
	containerCfg := &container.Config{
		Image: cfg.ImageName,
		Env: []string{
			fmt.Sprintf("SUBMISSION_ID=%s", cfg.SubmissionID),
		},
		// No entrypoint override — contestants define their own CMD/ENTRYPOINT.
	}

	// Network configuration — connect to sandbox_isolated only.
	netCfg := &network.NetworkingConfig{
		EndpointsConfig: map[string]*network.EndpointSettings{
			sandboxNetwork: {},
		},
	}

	resp, err := r.docker.ContainerCreate(ctx, containerCfg, hostCfg, netCfg, nil, containerName)
	if err != nil {
		return nil, fmt.Errorf("create container for submission %s: %w", cfg.SubmissionID, err)
	}

	if err := r.docker.ContainerStart(ctx, resp.ID, container.StartOptions{}); err != nil {
		// Clean up the created-but-not-started container.
		_ = r.docker.ContainerRemove(ctx, resp.ID, container.RemoveOptions{Force: true})
		return nil, fmt.Errorf("start container %s: %w", resp.ID[:12], err)
	}

	// Inspect to discover the host port Docker assigned.
	hostPort, err := r.resolveHostPort(ctx, resp.ID, cfg.ExposedPort)
	if err != nil {
		_ = r.Stop(context.Background(), resp.ID)
		return nil, fmt.Errorf("resolve host port: %w", err)
	}

	return &SandboxHandle{
		ContainerID:  resp.ID,
		SubmissionID: cfg.SubmissionID,
		HostEndpoint: fmt.Sprintf("localhost:%s", hostPort),
	}, nil
}

// Stop forcefully terminates a sandbox container and removes it.
// Called when a stress test run ends or when a timeout is exceeded.
func (r *Runner) Stop(ctx context.Context, containerID string) error {
	timeout := 5 // seconds to wait for graceful shutdown before kill
	stopOpts := container.StopOptions{Timeout: &timeout}

	if err := r.docker.ContainerStop(ctx, containerID, stopOpts); err != nil {
		return fmt.Errorf("stop container %s: %w", containerID[:12], err)
	}

	if err := r.docker.ContainerRemove(ctx, containerID, container.RemoveOptions{}); err != nil {
		return fmt.Errorf("remove container %s: %w", containerID[:12], err)
	}

	return nil
}

// StopAllForSubmission stops every running sandbox tied to a submission ID.
// Used as a cleanup sweep if a run ends abnormally.
func (r *Runner) StopAllForSubmission(ctx context.Context, submissionID string) error {
	containers, err := r.docker.ContainerList(ctx, container.ListOptions{
		Filters: filters.NewArgs(
			filters.Arg("name", fmt.Sprintf("sandbox-%s", submissionID[:8])),
		),
	})
	if err != nil {
		return fmt.Errorf("list containers for submission %s: %w", submissionID, err)
	}

	for _, c := range containers {
		if err := r.Stop(ctx, c.ID); err != nil {
			// Log but continue — we want to stop as many as possible.
			fmt.Printf("warning: failed to stop container %s: %v\n", c.ID[:12], err)
		}
	}
	return nil
}

// Logs streams the contestant container's stdout/stderr.
// Useful for debugging why a submission's server failed to start.
func (r *Runner) Logs(ctx context.Context, containerID string, since time.Time) (io.ReadCloser, error) {
	return r.docker.ContainerLogs(ctx, containerID, container.LogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Since:      since.Format(time.RFC3339),
		Follow:     true,
	})
}

// resolveHostPort inspects the container and returns the host port mapped
// from the contestant's server port. Docker assigns this dynamically when
// PublishAllPorts is true.
func (r *Runner) resolveHostPort(ctx context.Context, containerID, containerPort string) (string, error) {
	inspect, err := r.docker.ContainerInspect(ctx, containerID)
	if err != nil {
		return "", err
	}

	portKey := containerPort + "/tcp"
	bindings, ok := inspect.NetworkSettings.Ports[portKey]
	if !ok || len(bindings) == 0 {
		return "", fmt.Errorf("no host port binding found for container port %s", containerPort)
	}

	return bindings[0].HostPort, nil
}
