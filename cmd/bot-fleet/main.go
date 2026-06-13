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
