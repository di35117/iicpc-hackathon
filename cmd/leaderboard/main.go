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
	RunID               string  `json:"run_id"`
	SubmissionID        string  `json:"submission_id"`
	PeakTPS             int64   `json:"peak_tps"`
	AverageP99Lat_us    float64 `json:"avg_p99_latency_us"`
	IntegrityPercentage float64 `json:"integrity_percentage"`
	CompositeScore      float64 `json:"composite_score"`
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
	
	corsMiddleware := func(next http.Handler) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusOK)
				return
			}
			next.ServeHTTP(w, r)
		}
	}

	mux.HandleFunc("GET /score/{run_id}", corsMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runID := r.PathValue("run_id")
		
		var peakTPS int64
		var avgP99 float64
		var subID string
		var totalVolume int64
		var violations int64

		// Calculate stats and count FIFO violations in a single query
		query := `
			WITH stats AS (
				SELECT 
					MAX(total_orders) as peak_tps,
					AVG(p99_us) as avg_p99,
					MAX(submission_id) as sub_id,
					SUM(total_orders) as total_volume
				FROM latency_per_second 
				WHERE run_id = $1
			),
			violations AS (
				SELECT COUNT(*) as violation_count
				FROM correctness_violations
				WHERE run_id = $1
			)
			SELECT 
				COALESCE(s.peak_tps, 0),
				COALESCE(s.avg_p99, 0),
				COALESCE(s.sub_id, ''),
				COALESCE(s.total_volume, 1),
				COALESCE(v.violation_count, 0)
			FROM stats s
			CROSS JOIN violations v;
		`
		
		err := dbPool.QueryRow(ctx, query, runID).Scan(&peakTPS, &avgP99, &subID, &totalVolume, &violations)
		if err != nil || peakTPS == 0 {
			http.Error(w, "Run not found or stats not aggregated yet.", http.StatusNotFound)
			return
		}

		// Calculate Algorithmic Integrity
		integrity := 100.0
		if totalVolume > 0 {
			errorRate := float64(violations) / float64(totalVolume)
			integrity = 100.0 - (errorRate * 100.0)
			if integrity < 0 {
				integrity = 0
			}
		}

		// Calculate Composite Score with Integrity Penalty
		latencyMS := avgP99 / 1000.0
		baseScore := (float64(peakTPS) / 1000.0) - latencyMS + 100.0
		finalScore := baseScore * (integrity / 100.0)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(RunScore{
			RunID:               runID,
			SubmissionID:        subID,
			PeakTPS:             peakTPS,
			AverageP99Lat_us:    avgP99,
			IntegrityPercentage: integrity,
			CompositeScore:      finalScore,
		})
	})))

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
