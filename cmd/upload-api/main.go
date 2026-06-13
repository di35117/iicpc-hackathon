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
