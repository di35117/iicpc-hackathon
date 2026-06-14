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

		// --- STRICT FILE VALIDATION ---
		ext := strings.ToLower(filepath.Ext(header.Filename))
		if strings.HasSuffix(strings.ToLower(header.Filename), ".tar.gz") {
			ext = ".tar.gz"
		}

		allowedExts := map[string]bool{
			".cpp": true, ".rs": true, ".go": true, ".c": true, ".zip": true, ".tar.gz": true,
		}

		if !allowedExts[ext] {
			log.Printf("SECURITY REJECT: Invalid file type %s", header.Filename)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnsupportedMediaType)
			json.NewEncoder(w).Encode(SubmitResponse{
				Status:  "error",
				Message: "Security Violation: Only C++, Rust, Go source or archives (.zip, .tar.gz) allowed.",
			})
			return
		}
		// ------------------------------

		submissionID := uuid.New().String()
		log.Printf("submission %s: %s (%d bytes)", submissionID, header.Filename, header.Size)

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