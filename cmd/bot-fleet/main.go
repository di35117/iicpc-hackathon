package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/twmb/franz-go/pkg/kgo"
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
		ListenPort:   getEnv("PORT", "8083"),
		DefaultBots:  1000,
		DefaultSecs:  60,
	}
}

type Order struct {
	OrderID   string  `json:"order_id"`
	Type      string  `json:"type"`
	Side      string  `json:"side"`
	Price     float64 `json:"price"`
	Quantity  float64 `json:"quantity"`
	Timestamp int64   `json:"timestamp_ns"`
}

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

type RunJob struct {
	RunID        string `json:"run_id"`
	SubmissionID string `json:"submission_id"`
	TargetWSURL  string `json:"target_ws_url"`
	BotCount     int    `json:"bot_count"`
	DurationSecs int    `json:"duration_secs"`
}

func main() {
	cfg := loadConfig()

	cl, err := kgo.NewClient(
		kgo.SeedBrokers(cfg.RedpandaAddr),
		kgo.AllowAutoTopicCreation(),
	)
	if err != nil {
		log.Fatalf("failed to connect to redpanda: %v", err)
	}
	defer cl.Close()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /run", handleDirectRun(cfg, cl))
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})

	log.Printf("bot-fleet listening on :%s", cfg.ListenPort)
	if err := http.ListenAndServe(":"+cfg.ListenPort, mux); err != nil {
		log.Fatalf("server error: %v", err)
	}
}

func handleDirectRun(cfg Config, cl *kgo.Client) http.HandlerFunc {
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

		go runFleet(job, cl)

		w.WriteHeader(http.StatusAccepted)
		fmt.Fprintf(w, "fleet started: %d bots for %ds\n", job.BotCount, job.DurationSecs)
	}
}

func runFleet(job RunJob, cl *kgo.Client) {
	ctx, cancel := context.WithTimeout(
		context.Background(),
		time.Duration(job.DurationSecs)*time.Second,
	)
	defer cancel()

	var totalOrders atomic.Int64
	var totalAcks atomic.Int64

	log.Printf("[run %s] starting fleet: %d bots -> %s", job.RunID, job.BotCount, job.TargetWSURL)

	eventCh := make(chan TelemetryEvent, job.BotCount*10)

	// Keep the drainer alive to process events
	go drainEvents(ctx, eventCh, cl, &totalOrders, &totalAcks)

	var wg sync.WaitGroup // Added WaitGroup to track running bots

	for i := 0; i < job.BotCount; i++ {
		wg.Add(1) // Increment counter for each bot
		go func(botID int) {
			defer wg.Done() // Decrement counter when the bot gracefully exits
			runBot(ctx, botID, job, eventCh)
		}(i)
	}

	<-ctx.Done()   // 1. Wait for the 10-second timer to expire
	wg.Wait()      // 2. Wait for EVERY bot to finish their final write
	close(eventCh) // 3. NOW it is 100% safe to close the channel

	cl.Flush(context.Background())
	log.Printf("[run %s] complete — sent=%d acked=%d", job.RunID, totalOrders.Load(), totalAcks.Load())
}

func runBot(ctx context.Context, botID int, job RunJob, eventCh chan<- TelemetryEvent) {
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, job.TargetWSURL, nil)
	if err != nil {
		log.Printf("[bot %d] connect failed: %v", botID, err)
		return
	}
	defer conn.Close()

	midPrice := 100.0
	rng := rand.New(rand.NewSource(int64(botID)))

	for {
		select {
		case <-ctx.Done():
			return // The bot sees the timer is up and exits cleanly
		default:
		}

		order := generateOrder(rng, &midPrice, job.RunID)
		payload, _ := json.Marshal(order)
		sendTime := time.Now()

		if err := conn.WriteMessage(websocket.TextMessage, payload); err != nil {
			return
		}

		eventCh <- TelemetryEvent{
			Time:         sendTime,
			SubmissionID: job.SubmissionID,
			RunID:        job.RunID,
			OrderID:      order.OrderID,
			EventType:    "send",
			OrderType:    order.Type,
			Price:        order.Price,
			Quantity:     order.Quantity,
		}

		conn.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, _, err := conn.ReadMessage()
		ackTime := time.Now()

		if err != nil {
			eventCh <- TelemetryEvent{
				Time:         ackTime,
				SubmissionID: job.SubmissionID,
				RunID:        job.RunID,
				OrderID:      order.OrderID,
				EventType:    "reject",
				LatencyUS:    ackTime.Sub(sendTime).Microseconds(),
				OrderType:    order.Type,
			}
			continue
		}

		eventCh <- TelemetryEvent{
			Time:         ackTime,
			SubmissionID: job.SubmissionID,
			RunID:        job.RunID,
			OrderID:      order.OrderID,
			EventType:    "ack",
			LatencyUS:    ackTime.Sub(sendTime).Microseconds(),
			OrderType:    order.Type,
			Price:        order.Price,
			Quantity:     order.Quantity,
		}

		time.Sleep(time.Duration(rng.Intn(10)) * time.Millisecond)
	}
}

func generateOrder(rng *rand.Rand, midPrice *float64, runID string) Order {
	*midPrice *= 1.0 + (rng.Float64()-0.5)*0.001
	roll := rng.Float64()
	switch {
	case roll < 0.60:
		side := "buy"
		if rng.Float64() > 0.5 {
			side = "sell"
		}
		offset := (rng.Float64() - 0.5) * 0.005 * (*midPrice)
		return Order{
			OrderID:   uuid.New().String(),
			Type:      "limit",
			Side:      side,
			Price:     *midPrice + offset,
			Quantity:  float64(rng.Intn(100)+1) * 0.1,
			Timestamp: time.Now().UnixNano(),
		}
	case roll < 0.85:
		side := "buy"
		if rng.Float64() > 0.5 {
			side = "sell"
		}
		return Order{
			OrderID:   uuid.New().String(),
			Type:      "market",
			Side:      side,
			Quantity:  float64(rng.Intn(50)+1) * 0.1,
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

func drainEvents(ctx context.Context, ch <-chan TelemetryEvent, cl *kgo.Client, orders *atomic.Int64, acks *atomic.Int64) {
	for event := range ch {
		switch event.EventType {
		case "send":
			orders.Add(1)
		case "ack":
			acks.Add(1)
		}
		
		// Fire-and-forget payload into Redpanda
		payload, _ := json.Marshal(event)
		cl.Produce(ctx, &kgo.Record{
			Topic: "telemetry.events",
			Value: payload,
		}, nil)
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
