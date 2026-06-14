package main

import (
	"context"
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/twmb/franz-go/pkg/kgo"
)

// TelemetryEvent matches our Redpanda payload
type TelemetryEvent struct {
	Time         time.Time `json:"time"`
	SubmissionID string    `json:"submission_id"`
	RunID        string    `json:"run_id"`
	OrderID      string    `json:"order_id"`
	EventType    string    `json:"event_type"`
	OrderType    string    `json:"order_type"`
	Price        float64   `json:"price"`
	Quantity     float64   `json:"quantity"`
}

// OrderState tracks when an order was first seen
type OrderState struct {
	Timestamp time.Time
	Price     float64
}

func main() {
	kafkaURL := getEnv("REDPANDA_ADDR", "localhost:9092")
	dbURL := getEnv("DATABASE_URL", "postgres://platform:platform_secret@localhost:5432/metrics")

	ctx := context.Background()

	// 1. Connect to TimescaleDB to record violations
	dbPool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		log.Fatalf("Unable to connect to database: %v", err)
	}
	defer dbPool.Close()

	// Ensure our violations table exists
	_, err = dbPool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS correctness_violations (
			run_id TEXT,
			violation_type TEXT,
			details TEXT,
			recorded_at TIMESTAMPTZ DEFAULT NOW()
		);
	`)
	if err != nil {
		log.Fatalf("Failed to initialize violations table: %v", err)
	}

	// 2. Connect to Redpanda
	cl, err := kgo.NewClient(
		kgo.SeedBrokers(kafkaURL),
		kgo.ConsumeTopics("telemetry.events"),
		kgo.ConsumerGroup("correctness-validator-group"),
	)
	if err != nil {
		log.Fatalf("Unable to connect to Redpanda: %v", err)
	}
	defer cl.Close()

	log.Println("Shadow Orderbook Validator running. Auditing FIFO integrity...")

	// Shadow State: Tracks pending orders by Price Level
	// Key: Price -> Value: slice of pending OrderIDs
	pendingOrders := make(map[float64][]string)
	orderLookup := make(map[string]OrderState)
	var mu sync.Mutex

	for {
		fetches := cl.PollFetches(ctx)
		if fetches.IsClientClosed() {
			break
		}

		fetches.EachRecord(func(r *kgo.Record) {
			var evt TelemetryEvent
			if err := json.Unmarshal(r.Value, &evt); err != nil {
				return
			}

			mu.Lock()
			defer mu.Unlock()

			if evt.EventType == "send" && evt.OrderType == "limit" {
				// Record the order arriving at the exchange
				pendingOrders[evt.Price] = append(pendingOrders[evt.Price], evt.OrderID)
				orderLookup[evt.OrderID] = OrderState{Timestamp: evt.Time, Price: evt.Price}
			} else if evt.EventType == "ack" || evt.EventType == "fill" {
				// Validate FIFO on acknowledgment
				state, exists := orderLookup[evt.OrderID]
				if !exists {
					return // Ignored or cancel
				}

				priceQueue := pendingOrders[state.Price]
				if len(priceQueue) > 0 {
					// FIFO Check: Is the order being acked the OLDEST order in the queue for this price?
					oldestOrderID := priceQueue[0]
					
					if oldestOrderID != evt.OrderID {
						log.Printf("⚠️ FIFO VIOLATION DETECTED: Order %s acked before older order %s at price %f", evt.OrderID, oldestOrderID, state.Price)
						
						// Log violation to database to impact final score
						dbPool.Exec(ctx, `
							INSERT INTO correctness_violations (run_id, violation_type, details) 
							VALUES ($1, 'FIFO_SKIP', $2)
						`, evt.RunID, "Acked newer order before older order at same price level")
					}

					// Remove the order from our tracking queue
					var newQueue []string
					for _, id := range priceQueue {
						if id != evt.OrderID {
							newQueue = append(newQueue, id)
						}
					}
					pendingOrders[state.Price] = newQueue
				}
				delete(orderLookup, evt.OrderID)
			}
		})
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
