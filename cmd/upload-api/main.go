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
	"github.com/victus/iicpc-platform/internal/sandbox"
)

type Config struct {
	Port          string
	RedpandaAddr  string
}

func loadConfig() Config {
	return Config{
		Port:         getEnv("PORT", "8081"),
		RedpandaAddr: getEnv("REDPANDA_ADDR", "localhost:9092"),
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
	TargetURL    string    `json:"target_url"`
}

func main() {
	cfg := loadConfig()
	mux := http.NewServeMux()

	mux.HandleFunc("POST /submit", handleSubmit(cfg))
	mux.HandleFunc("POST /run/{id}", handleRun(cfg))
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

		// Synchronous build for the hackathon prototype.
		buildRes, err := sandbox.Build(submissionID, file)
		if err != nil || !buildRes.Success {
			log.Printf("build failed for %s: %v\nLogs: %s", submissionID, err, buildRes.Logs)
			http.Error(w, "build failed", http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(SubmitResponse{
			SubmissionID: submissionID,
			Status:       "ready",
			Message:      "source uploaded and built successfully",
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
		imageName := fmt.Sprintf("iicpc-submission-%s:latest", submissionID)

		// Start the sandboxed container
		handle, err := sandbox.Start(context.Background(), sandbox.SandboxConfig{
			ImageName:    imageName,
			SubmissionID: submissionID,
			ExposedPort:  "8080", 
		})
		
		if err != nil {
			log.Printf("sandbox start failed for %s: %v", submissionID, err)
			http.Error(w, "failed to start sandbox container", http.StatusInternalServerError)
			return
		}

		targetWSURL := fmt.Sprintf("ws://%s/orders", handle.HostEndpoint)
		log.Printf("run %s triggered for submission %s at %s", runID, submissionID, targetWSURL)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(RunResponse{
			RunID:        runID,
			SubmissionID: submissionID,
			StartedAt:    time.Now().UTC(),
			TargetURL:    targetWSURL,
		})
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
