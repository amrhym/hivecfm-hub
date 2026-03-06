package workers

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/riverqueue/river"

	"github.com/amrhym/hivecfm-hub/internal/huberrors"
	"github.com/amrhym/hivecfm-hub/internal/models"
	"github.com/amrhym/hivecfm-hub/internal/service"
)

// SentimentClassifier classifies text sentiment. Implemented by openai.ChatClient.
type SentimentClassifier interface {
	ClassifySentiment(ctx context.Context, text string) (sentiment string, score float64, err error)
}

// sentimentRecordService is the minimal interface needed by the sentiment worker.
type sentimentRecordService interface {
	GetFeedbackRecord(ctx context.Context, id uuid.UUID) (*models.FeedbackRecord, error)
	UpdateMetadata(ctx context.Context, id uuid.UUID, newMeta map[string]interface{}) error
}

// SentimentAnalysisWorker runs sentiment analysis on feedback records.
type SentimentAnalysisWorker struct {
	river.WorkerDefaults[service.SentimentAnalysisArgs]

	classifier    SentimentClassifier
	recordService sentimentRecordService
}

// NewSentimentAnalysisWorker creates a worker that fetches the record, classifies sentiment, and updates metadata.
func NewSentimentAnalysisWorker(
	classifier SentimentClassifier,
	recordService sentimentRecordService,
) *SentimentAnalysisWorker {
	return &SentimentAnalysisWorker{
		classifier:    classifier,
		recordService: recordService,
	}
}

const sentimentAnalysisTimeout = 30 * time.Second

// Timeout limits how long a single sentiment analysis job can run.
func (w *SentimentAnalysisWorker) Timeout(*river.Job[service.SentimentAnalysisArgs]) time.Duration {
	return sentimentAnalysisTimeout
}

// Work loads the record, classifies sentiment, and persists it in metadata.
func (w *SentimentAnalysisWorker) Work(ctx context.Context, job *river.Job[service.SentimentAnalysisArgs]) error {
	args := job.Args

	record, err := w.recordService.GetFeedbackRecord(ctx, args.FeedbackRecordID)
	if err != nil {
		slog.Error("sentiment: get record failed",
			"feedback_record_id", args.FeedbackRecordID,
			"error", err,
		)

		if errors.Is(err, huberrors.ErrNotFound) {
			return nil
		}

		return fmt.Errorf("get feedback record: %w", err)
	}

	// Only process text fields with non-empty value_text.
	if record.FieldType != models.FieldTypeText || record.ValueText == nil || *record.ValueText == "" {
		slog.Info("sentiment: skipped (no text content)",
			"feedback_record_id", args.FeedbackRecordID,
		)

		return nil
	}

	sentiment, score, err := w.classifier.ClassifySentiment(ctx, *record.ValueText)
	if err != nil {
		isLastAttempt := job.Attempt >= job.MaxAttempts

		if isLastAttempt {
			slog.Error("sentiment: API failed (final attempt)",
				"feedback_record_id", args.FeedbackRecordID,
				"error", err,
			)

			return fmt.Errorf("sentiment API (final attempt): %w", err)
		}

		return fmt.Errorf("sentiment API: %w", err)
	}

	newMeta := map[string]interface{}{
		"sentiment":       sentiment,
		"sentiment_score": score,
	}

	if err := w.recordService.UpdateMetadata(ctx, args.FeedbackRecordID, newMeta); err != nil {
		slog.Error("sentiment: update metadata failed",
			"feedback_record_id", args.FeedbackRecordID,
			"error", err,
		)

		return fmt.Errorf("update feedback record metadata: %w", err)
	}

	slog.Info("sentiment: stored",
		"feedback_record_id", args.FeedbackRecordID,
		"sentiment", sentiment,
		"score", score,
	)

	return nil
}
