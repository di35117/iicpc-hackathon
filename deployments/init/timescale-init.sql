CREATE EXTENSION IF NOT EXISTS timescaledb;

-- Raw telemetry events from the bot fleet.
CREATE TABLE IF NOT EXISTS telemetry_events (
    time            TIMESTAMPTZ     NOT NULL,
    submission_id   TEXT            NOT NULL,
    run_id          TEXT            NOT NULL,
    order_id        TEXT            NOT NULL,
    event_type      TEXT            NOT NULL,  -- 'send' | 'ack' | 'fill' | 'reject'
    latency_us      BIGINT,
    order_type      TEXT,
    price           NUMERIC(18, 8),
    quantity        NUMERIC(18, 8)
);

SELECT create_hypertable('telemetry_events', 'time', if_not_exists => TRUE);

CREATE INDEX IF NOT EXISTS idx_telemetry_submission
    ON telemetry_events (submission_id, time DESC);

-- Pre-computed per-second latency percentiles for the live dashboard.
CREATE MATERIALIZED VIEW IF NOT EXISTS latency_per_second
WITH (timescaledb.continuous) AS
SELECT
    time_bucket('1 second', time)                                  AS bucket,
    submission_id,
    run_id,
    percentile_disc(0.50) WITHIN GROUP (ORDER BY latency_us)       AS p50_us,
    percentile_disc(0.90) WITHIN GROUP (ORDER BY latency_us)       AS p90_us,
    percentile_disc(0.99) WITHIN GROUP (ORDER BY latency_us)       AS p99_us,
    COUNT(*)                                                        AS total_orders
FROM telemetry_events
WHERE event_type = 'ack'
GROUP BY bucket, submission_id, run_id
WITH NO DATA;

-- Correctness violations: any fill that breaks price-time priority.
CREATE TABLE IF NOT EXISTS correctness_violations (
    time            TIMESTAMPTZ     NOT NULL,
    submission_id   TEXT            NOT NULL,
    run_id          TEXT            NOT NULL,
    order_id        TEXT            NOT NULL,
    expected_fill   TEXT            NOT NULL,
    actual_fill     TEXT            NOT NULL,
    violation_type  TEXT            NOT NULL
);

SELECT create_hypertable('correctness_violations', 'time', if_not_exists => TRUE);

-- Run lifecycle tracking.
CREATE TABLE IF NOT EXISTS runs (
    run_id          TEXT            PRIMARY KEY,
    submission_id   TEXT            NOT NULL,
    started_at      TIMESTAMPTZ     NOT NULL DEFAULT NOW(),
    ended_at        TIMESTAMPTZ,
    status          TEXT            NOT NULL DEFAULT 'running',
    bot_count       INTEGER         NOT NULL,
    duration_secs   INTEGER         NOT NULL
);
