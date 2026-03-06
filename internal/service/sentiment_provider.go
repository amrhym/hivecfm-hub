package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"strings"
	"time"

	"github.com/riverqueue/river"

	"github.com/amrhym/hivecfm-hub/internal/datatypes"
	"github.com/amrhym/hivecfm-hub/internal/models"
)

const uniqueByPeriodSentiment = 24 * time.Hour

// SentimentProvider implements eventPublisher by enqueueing one River job per feedback record event
// when the event is FeedbackRecordCreated (with non-empty value_text and text field type) or
// FeedbackRecordUpdated (with value_text in ChangedFields).
type SentimentProvider struct {
	inserter    FeedbackEmbeddingInserter
	queueName   string
	maxAttempts int
}

// NewSentimentProvider creates a provider that enqueues sentiment_analysis jobs.
func NewSentimentProvider(
	inserter FeedbackEmbeddingInserter,
	queueName string,
	maxAttempts int,
) *SentimentProvider {
	return &SentimentProvider{
		inserter:    inserter,
		queueName:   queueName,
		maxAttempts: maxAttempts,
	}
}

// PublishEvent enqueues a sentiment_analysis job when the event is FeedbackRecordCreated
// (with non-empty value_text and text field type) or FeedbackRecordUpdated (with value_text in ChangedFields).
func (p *SentimentProvider) PublishEvent(ctx context.Context, event Event) {
	if event.Type == datatypes.FeedbackRecordUpdated {
		if !contains(event.ChangedFields, "value_text") {
			slog.Debug("sentiment: skip, value_text not in changed fields",
				"event_id", event.ID,
				"feedback_record_id", recordIDFromEventData(event.Data),
			)

			return
		}
	} else if event.Type != datatypes.FeedbackRecordCreated {
		return
	}

	record, ok := event.Data.(*models.FeedbackRecord)
	if !ok {
		slog.Debug("sentiment: skip, event data is not *FeedbackRecord", "event_id", event.ID)

		return
	}

	// Only enqueue for text-type fields with non-empty value_text.
	if record.FieldType != models.FieldTypeText {
		slog.Debug("sentiment: skip, not a text field",
			"feedback_record_id", record.ID,
			"field_type", record.FieldType,
		)

		return
	}

	if record.ValueText == nil || strings.TrimSpace(*record.ValueText) == "" {
		slog.Debug("sentiment: skip, no value_text",
			"feedback_record_id", record.ID,
		)

		return
	}

	valueTextHash := sentimentValueTextHash(*record.ValueText)

	opts := &river.InsertOpts{
		Queue:       p.queueName,
		MaxAttempts: p.maxAttempts,
		UniqueOpts:  river.UniqueOpts{ByArgs: true, ByPeriod: uniqueByPeriodSentiment},
	}

	_, err := p.inserter.Insert(ctx, SentimentAnalysisArgs{
		FeedbackRecordID: record.ID,
		ValueTextHash:    valueTextHash,
	}, opts)
	if err != nil {
		slog.Error("sentiment: enqueue failed",
			"event_id", event.ID,
			"feedback_record_id", record.ID,
			"error", err,
		)

		return
	}

	slog.Info("sentiment: job enqueued",
		"event_id", event.ID,
		"feedback_record_id", record.ID,
	)
}

// sentimentValueTextHash returns a SHA256 hash of the value_text for dedupe.
func sentimentValueTextHash(valueText string) string {
	trimmed := strings.TrimSpace(valueText)
	if trimmed == "" {
		return "empty"
	}

	sum := sha256.Sum256([]byte(trimmed))

	return hex.EncodeToString(sum[:])
}
