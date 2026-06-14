package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/victus/iicpc-platform/internal/sandbox"
)

type Config struct {
	Port         string
	RedpandaAddr string
	BotFleetURL  string
}

func loadConfig() Config {
	return Config{
		Port:         getEnv("PORT", "8081"),
		RedpandaAddr: getEnv("REDPANDA_ADDR", "localhost:9092"),
		BotFleetURL:  getEnv("BOT_FLEET_URL", "http://localhost:8083"),
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
	Message      string    `json:"message"`
}

// BotFleetRequest matches the expected payload of your bot-fleet service
type BotFleetRequest struct {
	RunID        string `json:"run_id"`
	SubmissionID string `json:"submission_id"`
	TargetWSURL  string `json:"target_ws_url"`
	BotCount     int    `json:"bot_count"`
	DurationSecs int    `json:"duration_secs"`
}

func main() {
	cfg := loadConfig()
	mux := http.NewServeMux()

	// CORS Middleware for the React Command Center
	corsMiddleware := func(next http.Handler) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}
			next.ServeHTTP(w, r)
		}
	}

	mux.HandleFunc("POST /submit", corsMiddleware(handleSubmit(cfg)))
	mux.HandleFunc("POST /run/{id}", corsMiddleware(handleRun(cfg)))
	mux.HandleFunc("GET /health", corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})))

	log.Printf("upload-api orchestrator listening on :%s", cfg.Port)
	if err := http.ListenAndServe(":"+cfg.Port, mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func handleSubmit(cfg Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.ParseMultipartForm(50 << 20)
		file, header, err := r.FormFile("source")
		if err != nil {
			http.Error(w, "missing source file", http.StatusBadRequest)
			return
		}
		defer file.Close()

		// Validate extension
		ext := strings.ToLower(filepath.Ext(header.Filename))
		if !strings.Contains(".cpp.rs.go.c.zip.tar.gz.tar", ext) {
			http.Error(w, "invalid file type", http.StatusUnsupportedMediaType)
			return
		}

		submissionID := uuid.New().String()
		
		// CALL THE FIX: Pass header.Filename here
		buildRes, err := sandbox.Build(submissionID, header.Filename, file)
		
		// 1. Pipeline error handling (no nil pointer risk)
		if err != nil {
			log.Printf("Pipeline failed for %s: %v", submissionID, err)
			http.Error(w, "internal pipeline error", http.StatusInternalServerError)
			return
		}
		
		// 2. Compilation check
		if !buildRes.Success {
			log.Printf("Compilation failed for %s:\n%s", submissionID, buildRes.Logs)
			http.Error(w, "build failed: " + buildRes.Logs, http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(SubmitResponse{
			SubmissionID: submissionID,
			Status:       "ready",
			Message:      "Successfully built and sandboxed",
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

		// 1. Containerized Deployment
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
		log.Printf("run %s deployed for submission %s at %s. Triggering Bot Fleet...", runID, submissionID, targetWSURL)

		// 2. Distributed Load Testing Trigger
		botReq := BotFleetRequest{
			RunID:        runID,
			SubmissionID: submissionID,
			TargetWSURL:  targetWSURL,
			BotCount:     500, // Massive concurrency
			DurationSecs: 15,
		}

		botReqBytes, _ := json.Marshal(botReq)
		botResp, err := http.Post(cfg.BotFleetURL+"/run", "application/json", bytes.NewBuffer(botReqBytes))
		if err != nil || botResp.StatusCode != http.StatusAccepted {
			log.Printf("Failed to trigger bot fleet: %v", err)
			// We don't fail the whole request, but we note the error.
		}

		// 3. Hand off to Real-Time Scoring
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		json.NewEncoder(w).Encode(RunResponse{
			RunID:        runID,
			SubmissionID: submissionID,
			StartedAt:    time.Now().UTC(),
			TargetURL:    targetWSURL,
			Message:      "Container deployed and Bot Fleet unleashed.",
		})
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}