package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/twmb/franz-go/pkg/kgo"
)

type TelemetryEvent struct {
	Time         time.Time `json:"time"`
	SubmissionID string    `json:"submission_id"`
	RunID        string    `json:"run_id"`
	OrderID      string    `json:"order_id"`
	EventType    string    `json:"event_type"`
	LatencyUS    int64     `json:"latency_us"`
	OrderType    string    `json:"order_type"`
	Price        float64   `json:"price"`
	Quantity     float64   `json:"quantity"`
}

func main() {
	dbURL := getEnv("DATABASE_URL", "postgres://platform:platform_secret@localhost:5432/metrics")
	kafkaURL := getEnv("REDPANDA_ADDR", "localhost:9092")

	ctx := context.Background()

	// 1. Connect to TimescaleDB
	dbPool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		log.Fatalf("Unable to connect to database: %v", err)
	}
	defer dbPool.Close()
	log.Println("Connected to TimescaleDB")

	// 2. Connect to Redpanda
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(kafkaURL),
		kgo.ConsumeTopics("telemetry.events"),
		kgo.ConsumerGroup("telemetry-ingester-group"),
	)
	if err != nil {
		log.Fatalf("Unable to connect to Redpanda: %v", err)
	}
	defer cl.Close()
	log.Println("Connected to Redpanda, listening for events...")

	// 3. High-throughput Batch Ingestion
	const batchSize = 10000
	batch := make([][]any, 0, batchSize)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		fetches := cl.PollFetches(ctx)
		if fetches.IsClientClosed() {
			break
		}

		fetches.EachRecord(func(r *kgo.Record) {
			var evt TelemetryEvent
			if err := json.Unmarshal(r.Value, &evt); err == nil {
				batch = append(batch, []any{
					evt.Time, evt.SubmissionID, evt.RunID, evt.OrderID,
					evt.EventType, evt.LatencyUS, evt.OrderType, evt.Price, evt.Quantity,
				})
			}
		})

		// Flush to DB if batch is full OR if a second has passed
		select {
		case <-ticker.C:
			flushBatch(ctx, dbPool, cl, &batch)
		default:
			if len(batch) >= batchSize {
				flushBatch(ctx, dbPool, cl, &batch)
			}
		}
	}
}

func flushBatch(ctx context.Context, dbPool *pgxpool.Pool, cl *kgo.Client, batch *[][]any) {
	if len(*batch) == 0 {
		return
	}

	_, err := dbPool.CopyFrom(
		ctx,
		pgx.Identifier{"telemetry_events"},
		[]string{"time", "submission_id", "run_id", "order_id", "event_type", "latency_us", "order_type", "price", "quantity"},
		pgx.CopyFromRows(*batch),
	)
	
	if err != nil {
		log.Printf("Failed to insert batch: %v", err)
	} else {
		log.Printf("Successfully inserted batch of %d events", len(*batch))
		// Commit offsets only after successful DB write (At-Least-Once delivery)
		cl.CommitUncommittedOffsets(ctx)
	}
	
	// Reset the batch slice, keeping capacity
	*batch = (*batch)[:0]
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
