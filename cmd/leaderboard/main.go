package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Config struct {
	Port  string
	DBURL string
}

func loadConfig() Config {
	return Config{
		Port:  getEnv("PORT", "8084"),
		DBURL: getEnv("DATABASE_URL", "postgres://platform:platform_secret@localhost:5432/metrics"),
	}
}

type RunScore struct {
	RunID            string  `json:"run_id"`
	SubmissionID     string  `json:"submission_id"`
	PeakTPS          int64   `json:"peak_tps"`
	AverageP99Lat_us float64 `json:"avg_p99_latency_us"`
	CompositeScore   float64 `json:"composite_score"`
}

func main() {
	cfg := loadConfig()
	ctx := context.Background()

	dbPool, err := pgxpool.New(ctx, cfg.DBURL)
	if err != nil {
		log.Fatalf("Unable to connect to database: %v", err)
	}
	defer dbPool.Close()

	mux := http.NewServeMux()
	
	// Endpoint to get the score for a specific run
	mux.HandleFunc("GET /score/{run_id}", func(w http.ResponseWriter, r *http.Request) {
		runID := r.PathValue("run_id")
		
		// Query TimescaleDB for the Peak TPS and Average p99 Latency for this run
		var peakTPS int64
		var avgP99 float64
		var subID string

		query := `
			SELECT 
				MAX(total_orders) as peak_tps,
				AVG(p99_us) as avg_p99,
				MAX(submission_id) as sub_id
			FROM latency_per_second 
			WHERE run_id = $1
		`
		
		err := dbPool.QueryRow(ctx, query, runID).Scan(&peakTPS, &avgP99, &subID)
		if err != nil || peakTPS == 0 {
			http.Error(w, "Run not found or stats not aggregated yet. (Did you call refresh_continuous_aggregate?)", http.StatusNotFound)
			return
		}

		// Calculate Composite Score
		// Formula: (TPS / 1000) - (Latency_ms) + 100 (Base Score)
		// This is a simplified prototype formula. Higher TPS = better, Higher Latency = worse.
		latencyMS := avgP99 / 1000.0
		score := (float64(peakTPS) / 1000.0) - latencyMS + 100.0

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(RunScore{
			RunID:            runID,
			SubmissionID:     subID,
			PeakTPS:          peakTPS,
			AverageP99Lat_us: avgP99,
			CompositeScore:   score,
		})
	})

	log.Printf("leaderboard API listening on :%s", cfg.Port)
	if err := http.ListenAndServe(":"+cfg.Port, mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
