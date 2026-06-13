#!/bin/bash
# Run this script from /mnt/d/Projects/iicpc-platform in your WSL2 terminal.
# It creates the full project structure with all files.

set -e
echo "Setting up IICPC platform project..."

# ─── docker-compose.yml ───────────────────────────────────────────────────────
mkdir -p deployments/init

cat > deployments/docker-compose.yml << 'EOF'
version: '3.8'

# IICPC Platform - Local Development Stack
# Runs all infrastructure services locally.
# Contestant sandbox containers are spawned separately by the upload-api service.

services:

  # Redpanda: Kafka-compatible message bus. Single binary, no ZooKeeper.
  # All telemetry events flow through here between bot-fleet and telemetry-ingester.
  redpanda:
    image: redpandadata/redpanda:latest
    container_name: redpanda
    command:
      - redpanda
      - start
      - --smp=1
      - --memory=512M
      - --overprovisioned
      - --kafka-addr=PLAINTEXT://0.0.0.0:29092,OUTSIDE://0.0.0.0:9092
      - --advertise-kafka-addr=PLAINTEXT://redpanda:29092,OUTSIDE://localhost:9092
      - --pandaproxy-addr=0.0.0.0:8082
    ports:
      - "9092:9092"
      - "29092:29092"
      - "8082:8082"
    volumes:
      - redpanda_data:/var/lib/redpanda/data
    healthcheck:
      test: ["CMD", "rpk", "cluster", "info"]
      interval: 10s
      timeout: 5s
      retries: 5

  # Redpanda Console: web UI for inspecting topics during development.
  redpanda-console:
    image: redpandadata/console:latest
    container_name: redpanda-console
    ports:
      - "8080:8080"
    environment:
      KAFKA_BROKERS: redpanda:29092
    depends_on:
      redpanda:
        condition: service_healthy

  # TimescaleDB: PostgreSQL with time-series extension.
  # Stores historical latency/throughput samples for post-run analytics.
  timescaledb:
    image: timescale/timescaledb:latest-pg16
    container_name: timescaledb
    environment:
      POSTGRES_USER: platform
      POSTGRES_PASSWORD: platform_secret
      POSTGRES_DB: metrics
    ports:
      - "5432:5432"
    volumes:
      - timescale_data:/var/lib/postgresql/data
      - ./init/timescale-init.sql:/docker-entrypoint-initdb.d/init.sql
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U platform -d metrics"]
      interval: 10s
      timeout: 5s
      retries: 5

  # Redis: sorted sets for live leaderboard, pub/sub for broadcasting score updates.
  redis:
    image: redis:7-alpine
    container_name: redis
    command: redis-server --appendonly yes
    ports:
      - "6379:6379"
    volumes:
      - redis_data:/data
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 10s
      timeout: 5s
      retries: 5

  # MinIO: S3-compatible local object store for contestant source uploads.
  # Replaced by real AWS S3 in the Terraform deployment.
  minio:
    image: minio/minio:latest
    container_name: minio
    command: server /data --console-address ":9001"
    environment:
      MINIO_ROOT_USER: platform
      MINIO_ROOT_PASSWORD: platform_secret_2024
    ports:
      - "9000:9000"
      - "9001:9001"
    volumes:
      - minio_data:/data
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:9000/minio/health/live"]
      interval: 10s
      timeout: 5s
      retries: 5

networks:
  default:
    name: platform_internal
  sandbox_net:
    name: sandbox_isolated
    driver: bridge
    internal: true

volumes:
  redpanda_data:
  timescale_data:
  redis_data:
  minio_data:
EOF

# ─── TimescaleDB schema ───────────────────────────────────────────────────────
cat > deployments/init/timescale-init.sql << 'EOF'
CREATE EXTENSION IF NOT EXISTS timescaledb;

-- Raw telemetry events from the bot fleet.
CREATE TABLE IF NOT EXISTS telemetry_events (
    time            TIMESTAMPTZ     NOT NULL,
    submission_id   TEXT            NOT NULL,
    run_id          TEXT            NOT NULL,
    order_id        TEXT            NOT NULL,
    event_type      TEXT            NOT NULL,  -- 'send' | 'ack' | 'fill' | 'reject'
    latency_us      BIGINT,
    order_type      TEXT,
    price           NUMERIC(18, 8),
    quantity        NUMERIC(18, 8)
);

SELECT create_hypertable('telemetry_events', 'time', if_not_exists => TRUE);

CREATE INDEX IF NOT EXISTS idx_telemetry_submission
    ON telemetry_events (submission_id, time DESC);

-- Pre-computed per-second latency percentiles for the live dashboard.
CREATE MATERIALIZED VIEW IF NOT EXISTS latency_per_second
WITH (timescaledb.continuous) AS
SELECT
    time_bucket('1 second', time)                                  AS bucket,
    submission_id,
    run_id,
    percentile_disc(0.50) WITHIN GROUP (ORDER BY latency_us)       AS p50_us,
    percentile_disc(0.90) WITHIN GROUP (ORDER BY latency_us)       AS p90_us,
    percentile_disc(0.99) WITHIN GROUP (ORDER BY latency_us)       AS p99_us,
    COUNT(*)                                                        AS total_orders
FROM telemetry_events
WHERE event_type = 'ack'
GROUP BY bucket, submission_id, run_id
WITH NO DATA;

-- Correctness violations: any fill that breaks price-time priority.
CREATE TABLE IF NOT EXISTS correctness_violations (
    time            TIMESTAMPTZ     NOT NULL,
    submission_id   TEXT            NOT NULL,
    run_id          TEXT            NOT NULL,
    order_id        TEXT            NOT NULL,
    expected_fill   TEXT            NOT NULL,
    actual_fill     TEXT            NOT NULL,
    violation_type  TEXT            NOT NULL
);

SELECT create_hypertable('correctness_violations', 'time', if_not_exists => TRUE);

-- Run lifecycle tracking.
CREATE TABLE IF NOT EXISTS runs (
    run_id          TEXT            PRIMARY KEY,
    submission_id   TEXT            NOT NULL,
    started_at      TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    ended_at        TIMESTAMPTZ,
    status          TEXT            NOT NULL DEFAULT 'running',
    bot_count       INTEGER         NOT NULL,
    duration_secs   INTEGER         NOT NULL
);
EOF

# ─── go.mod ──────────────────────────────────────────────────────────────────
cat > go.mod << 'EOF'
module github.com/victus/iicpc-platform

go 1.22

require (
    github.com/docker/docker v26.1.4+incompatible
    github.com/docker/go-connections v0.5.0
    github.com/twmb/franz-go v1.17.0
    github.com/twmb/franz-go/pkg/kadm v1.12.0
    github.com/gorilla/websocket v1.5.3
    github.com/HdrHistogram/hdrhistogram-go v1.1.2
    github.com/jackc/pgx/v5 v5.6.0
    github.com/redis/go-redis/v9 v9.6.1
    github.com/minio/minio-go/v7 v7.0.77
    github.com/google/uuid v1.6.0
    go.uber.org/zap v1.27.0
)
EOF

# ─── upload-api/main.go ───────────────────────────────────────────────────────
mkdir -p cmd/upload-api
cat > cmd/upload-api/main.go << 'EOF'
// upload-api: REST API for contestant code submissions.
//
// POST /submit        — receives source tarball, stores in MinIO, triggers build
// POST /run/{id}      — spawns sandbox container, notifies bot-fleet to begin test
// GET  /runs/{run_id} — returns live scores and run status

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/google/uuid"
)

type Config struct {
	Port          string
	MinIOEndpoint string
	MinIOUser     string
	MinIOPassword string
	MinIOBucket   string
	DockerHost    string
	RedpandaAddr  string
}

func loadConfig() Config {
	return Config{
		Port:          getEnv("PORT", "8081"),
		MinIOEndpoint: getEnv("MINIO_ENDPOINT", "localhost:9000"),
		MinIOUser:     getEnv("MINIO_USER", "platform"),
		MinIOPassword: getEnv("MINIO_PASSWORD", "platform_secret_2024"),
		MinIOBucket:   getEnv("MINIO_BUCKET", "submissions"),
		DockerHost:    getEnv("DOCKER_HOST", "unix:///var/run/docker.sock"),
		RedpandaAddr:  getEnv("REDPANDA_ADDR", "localhost:9092"),
	}
}

type SubmitResponse struct {
	SubmissionID string `json:"submission_id"`
	Status       string `json:"status"`
	Message      string `json:"message"`
}

type RunResponse struct {
	RunID        string    `json:"run_id"`
	SubmissionID string    `json:"submission_id"`
	StartedAt    time.Time `json:"started_at"`
	BotCount     int       `json:"bot_count"`
	DurationSecs int       `json:"duration_secs"`
}

func main() {
	cfg := loadConfig()
	mux := http.NewServeMux()

	mux.HandleFunc("POST /submit", handleSubmit(cfg))
	mux.HandleFunc("POST /run/{id}", handleRun(cfg))
	mux.HandleFunc("GET /runs/{run_id}", handleRunStatus(cfg))
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	log.Printf("upload-api listening on :%s", cfg.Port)
	if err := http.ListenAndServe(":"+cfg.Port, mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func handleSubmit(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 50<<20)
		if err := r.ParseMultipartForm(50 << 20); err != nil {
			http.Error(w, "request too large or malformed", http.StatusBadRequest)
			return
		}
		file, header, err := r.FormFile("source")
		if err != nil {
			http.Error(w, "missing source file field", http.StatusBadRequest)
			return
		}
		defer file.Close()

		submissionID := uuid.New().String()
		log.Printf("submission %s: %s (%d bytes)", submissionID, header.Filename, header.Size)

		// TODO Day 3: stream to MinIO, trigger sandboxed build pipeline.
		_ = file
		_ = cfg

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(SubmitResponse{
			SubmissionID: submissionID,
			Status:       "building",
			Message:      "source uploaded, build pipeline starting",
		})
	}
}

func handleRun(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		submissionID := r.PathValue("id")
		if submissionID == "" {
			http.Error(w, "missing submission id", http.StatusBadRequest)
			return
		}
		runID := uuid.New().String()

		// TODO Day 3: pull image, spawn sandbox, publish run_started to Redpanda.
		_ = cfg
		_ = context.Background()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(RunResponse{
			RunID:        runID,
			SubmissionID: submissionID,
			StartedAt:    time.Now().UTC(),
			BotCount:     1000,
			DurationSecs: 60,
		})
		log.Printf("run %s triggered for submission %s", runID, submissionID)
	}
}

func handleRunStatus(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		runID := r.PathValue("run_id")
		_ = cfg
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"run_id": runID,
			"status": "stub — wire storage on Day 4",
		})
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
EOF

# ─── bot-fleet/main.go ───────────────────────────────────────────────────────
mkdir -p cmd/bot-fleet
cat > cmd/bot-fleet/main.go << 'EOF'
// bot-fleet: Distributed load generator for contestant exchange endpoints.
//
// The Orchestrator receives RunJob instructions (via HTTP for dev, Redpanda in prod).
// It spawns N goroutines — one per bot — each connecting to the contestant's
// WebSocket endpoint and firing a realistic mix of orders for the run duration.
//
// Every send/ack pair is published as a TelemetryEvent to the telemetry-ingester.
// Concurrency is managed via context cancellation: when the run timer expires,
// all bots receive ctx.Done() simultaneously and exit cleanly.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

type Config struct {
	RedpandaAddr string
	ListenPort   string
	DefaultBots  int
	DefaultSecs  int
}

func loadConfig() Config {
	return Config{
		RedpandaAddr: getEnv("REDPANDA_ADDR", "localhost:9092"),
		ListenPort:   getEnv("PORT", "8082"),
		DefaultBots:  1000,
		DefaultSecs:  60,
	}
}

// Order is a single trading instruction sent to the contestant's matching engine.
type Order struct {
	OrderID   string  `json:"order_id"`
	Type      string  `json:"type"`      // "limit" | "market" | "cancel"
	Side      string  `json:"side"`      // "buy" | "sell"
	Price     float64 `json:"price"`     // 0 for market orders
	Quantity  float64 `json:"quantity"`
	Timestamp int64   `json:"timestamp_ns"`
}

// TelemetryEvent is published to Redpanda after each send/ack cycle.
type TelemetryEvent struct {
	Time         time.Time `json:"time"`
	SubmissionID string    `json:"submission_id"`
	RunID        string    `json:"run_id"`
	OrderID      string    `json:"order_id"`
	EventType    string    `json:"event_type"` // "send" | "ack" | "reject"
	LatencyUS    int64     `json:"latency_us"`
	OrderType    string    `json:"order_type"`
	Price        float64   `json:"price"`
	Quantity     float64   `json:"quantity"`
}

// RunJob is the instruction that kicks off a stress test.
type RunJob struct {
	RunID        string `json:"run_id"`
	SubmissionID string `json:"submission_id"`
	TargetWSURL  string `json:"target_ws_url"` // ws://sandbox-container:8080/orders
	BotCount     int    `json:"bot_count"`
	DurationSecs int    `json:"duration_secs"`
}

func main() {
	cfg := loadConfig()
	mux := http.NewServeMux()

	// HTTP trigger for local development. In prod, runs are triggered via Redpanda.
	mux.HandleFunc("POST /run", handleDirectRun(cfg))
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	log.Printf("bot-fleet listening on :%s", cfg.ListenPort)
	if err := http.ListenAndServe(":"+cfg.ListenPort, mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func handleDirectRun(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var job RunJob
		if err := json.NewDecoder(r.Body).Decode(&job); err != nil {
			http.Error(w, "invalid run job payload", http.StatusBadRequest)
			return
		}
		if job.BotCount == 0 {
			job.BotCount = cfg.DefaultBots
		}
		if job.DurationSecs == 0 {
			job.DurationSecs = cfg.DefaultSecs
		}
		go runFleet(job)
		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, "fleet started: %d bots for %ds\n", job.BotCount, job.DurationSecs)
	}
}

// runFleet is the top-level coordinator for a stress test run.
// It creates a shared context with the run timeout, spawns all bots,
// and waits for the timer to expire before logging final stats.
func runFleet(job RunJob) {
	ctx, cancel := context.WithTimeout(
		context.Background(),
		time.Duration(job.DurationSecs)*time.Second,
	)
	defer cancel()

	var totalSent atomic.Int64
	var totalAcked atomic.Int64

	log.Printf("[run %s] fleet starting: %d bots -> %s", job.RunID, job.BotCount, job.TargetWSURL)

	// Buffered channel prevents slow telemetry publishing from blocking bots.
	eventCh := make(chan TelemetryEvent, job.BotCount*10)
	go drainEvents(ctx, eventCh, &totalSent, &totalAcked)

	for i := 0; i < job.BotCount; i++ {
		go runBot(ctx, i, job, eventCh)
	}

	<-ctx.Done()
	log.Printf("[run %s] complete — sent=%d acked=%d", job.RunID, totalSent.Load(), totalAcked.Load())
}

// runBot is the lifecycle of a single trading bot.
// It connects once and fires orders continuously until context is cancelled.
func runBot(ctx context.Context, botID int, job RunJob, eventCh chan<- TelemetryEvent) {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, job.TargetWSURL, nil)
	if err != nil {
		log.Printf("[bot %d] connect failed: %v", botID, err)
		return
	}
	defer conn.Close()

	// Each bot gets its own RNG seeded by its ID so order streams are independent.
	rng := rand.New(rand.NewSource(int64(botID)))
	midPrice := 100.0

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		order := generateOrder(rng, &midPrice)
		payload, _ := json.Marshal(order)
		sendTime := time.Now()

		if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
			return
		}

		eventCh <- TelemetryEvent{
			Time: sendTime, SubmissionID: job.SubmissionID,
			RunID: job.RunID, OrderID: order.OrderID,
			EventType: "send", OrderType: order.Type,
			Price: order.Price, Quantity: order.Quantity,
		}

		// 500ms read deadline: anything slower counts as a latency violation.
		conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, _, err := conn.ReadMessage()
		ackTime := time.Now()

		eventType := "ack"
		if err != nil {
			eventType = "reject"
		}

		eventCh <- TelemetryEvent{
			Time: ackTime, SubmissionID: job.SubmissionID,
			RunID: job.RunID, OrderID: order.OrderID,
			EventType: eventType, LatencyUS: ackTime.Sub(sendTime).Microseconds(),
			OrderType: order.Type, Price: order.Price, Quantity: order.Quantity,
		}

		// Small random sleep prevents thundering herd from a single bot.
		time.Sleep(time.Duration(rng.Intn(10)) * time.Millisecond)
	}
}

// generateOrder produces a realistic order mix:
// 60% limit orders, 25% market orders, 15% cancels.
func generateOrder(rng *rand.Rand, midPrice *float64) Order {
	*midPrice *= 1.0 + (rng.Float64()-0.5)*0.001

	side := "buy"
	if rng.Float64() > 0.5 {
		side = "sell"
	}

	roll := rng.Float64()
	switch {
	case roll < 0.60:
		offset := (rng.Float64() - 0.5) * 0.005 * (*midPrice)
		return Order{
			OrderID: uuid.New().String(), Type: "limit", Side: side,
			Price: *midPrice + offset, Quantity: float64(rng.Intn(100)+1) * 0.1,
			Timestamp: time.Now().UnixNano(),
		}
	case roll < 0.85:
		return Order{
			OrderID: uuid.New().String(), Type: "market", Side: side,
			Quantity: float64(rng.Intn(50)+1) * 0.1,
			Timestamp: time.Now().UnixNano(),
		}
	default:
		return Order{
			OrderID:   uuid.New().String(),
			Type:      "cancel",
			Timestamp: time.Now().UnixNano(),
		}
	}
}

// drainEvents consumes the telemetry channel and publishes to Redpanda.
// Stub implementation logs counts; real producer wired on Day 4.
func drainEvents(ctx context.Context, ch <-chan TelemetryEvent, sent *atomic.Int64, acked *atomic.Int64) {
	for event := range ch {
		switch event.EventType {
		case "send":
			sent.Add(1)
		case "ack":
			acked.Add(1)
		}
		// TODO Day 4: publish to Redpanda "telemetry.events" topic.
		_ = event
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
EOF

echo ""
echo "✓ All files created. Run the following next:"
echo ""
echo "  cd deployments && docker compose up -d"
echo ""
