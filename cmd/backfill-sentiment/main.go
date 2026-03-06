// backfill-sentiment enqueues River sentiment analysis jobs for feedback records that have
// non-empty value_text, field_type='text', and no sentiment in metadata. Run this when the
// API server is not handling backfill. Workers in the API process the jobs.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"github.com/amrhym/hivecfm-hub/internal/openai"
	"github.com/amrhym/hivecfm-hub/internal/repository"
	"github.com/amrhym/hivecfm-hub/internal/service"
	"github.com/amrhym/hivecfm-hub/internal/workers"
	"github.com/amrhym/hivecfm-hub/pkg/database"
)

const (
	defaultSentimentMaxAttempts = 3
	defaultSentimentModel      = "gpt-4o-mini"
	exitSuccess                = 0
	exitFailure                = 1
)

func main() {
	os.Exit(run())
}

func run() int {
	if err := godotenv.Load(); err != nil && !errors.Is(err, os.ErrNotExist) {
		slog.Warn("failed to load .env file", "error", err)
	}

	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		slog.Error("DATABASE_URL is required")

		return exitFailure
	}

	apiKey := os.Getenv("EMBEDDING_PROVIDER_API_KEY")
	if apiKey == "" {
		slog.Error("EMBEDDING_PROVIDER_API_KEY is required for sentiment analysis")

		return exitFailure
	}

	sentimentModel := os.Getenv("SENTIMENT_MODEL")
	if sentimentModel == "" {
		sentimentModel = defaultSentimentModel
	}

	ctx := context.Background()

	db, err := database.NewPostgresPool(ctx, databaseURL)
	if err != nil {
		slog.Error("Failed to connect to database", "error", err)

		return exitFailure
	}
	defer db.Close()

	// Query for text records with non-empty value_text that don't have sentiment in metadata.
	query := `
		SELECT id, value_text
		FROM feedback_records
		WHERE field_type = 'text'
			AND value_text IS NOT NULL
			AND value_text != ''
			AND (metadata IS NULL OR NOT (metadata ? 'sentiment'))
	`

	rows, err := db.Query(ctx, query)
	if err != nil {
		slog.Error("Failed to query records", "error", err)

		return exitFailure
	}
	defer rows.Close()

	type record struct {
		id        uuid.UUID
		valueText string
	}

	var records []record

	for rows.Next() {
		var r record
		if err := rows.Scan(&r.id, &r.valueText); err != nil {
			slog.Error("Failed to scan record", "error", err)

			return exitFailure
		}

		records = append(records, r)
	}

	if err := rows.Err(); err != nil {
		slog.Error("Error iterating records", "error", err)

		return exitFailure
	}

	if len(records) == 0 {
		slog.Info("No records need sentiment analysis")
		fmt.Println("No records need sentiment backfill.")

		return exitSuccess
	}

	// Create a minimal River client to enqueue jobs.
	// Worker registration is required by River even though we only enqueue.
	feedbackRecordsRepo := repository.NewFeedbackRecordsRepository(db)
	embeddingsRepo := repository.NewEmbeddingsRepository(db)
	feedbackRecordsService := service.NewFeedbackRecordsService(
		feedbackRecordsRepo, embeddingsRepo, "", nil, nil, "", 0,
	)

	chatClient := openai.NewChatClient(apiKey, sentimentModel)
	sentimentWorker := workers.NewSentimentAnalysisWorker(chatClient, feedbackRecordsService)
	riverWorkers := river.NewWorkers()
	river.AddWorker(riverWorkers, sentimentWorker)

	riverClient, err := river.NewClient(riverpgxv5.New(db), &river.Config{
		Queues: map[string]river.QueueConfig{
			service.SentimentQueueName: {MaxWorkers: 1},
		},
		Workers: riverWorkers,
	})
	if err != nil {
		slog.Error("Failed to create River client", "error", err)

		return exitFailure
	}

	opts := &river.InsertOpts{
		Queue:       service.SentimentQueueName,
		MaxAttempts: defaultSentimentMaxAttempts,
		UniqueOpts:  river.UniqueOpts{ByArgs: true, ByPeriod: 24 * time.Hour},
	}

	enqueued := 0

	for _, r := range records {
		hash := sentimentHash(r.valueText)

		_, err := riverClient.Insert(ctx, service.SentimentAnalysisArgs{
			FeedbackRecordID: r.id,
			ValueTextHash:    hash,
		}, opts)
		if err != nil {
			slog.Error("Failed to enqueue sentiment job", "id", r.id, "error", err)

			return exitFailure
		}

		enqueued++
	}

	slog.Info("Backfill complete", "enqueued", enqueued)
	fmt.Printf("Enqueued %d sentiment job(s).\n", enqueued)

	return exitSuccess
}

func sentimentHash(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return "backfill"
	}

	sum := sha256.Sum256([]byte(trimmed))

	return hex.EncodeToString(sum[:])
}
