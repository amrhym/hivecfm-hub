package models

import (
	"time"

	"github.com/google/uuid"
)

// EmbeddingVectorDimensions is the fixed size for all embedding vectors (DB column, index, and provider APIs).
const EmbeddingVectorDimensions = 768

// Embedding represents one embedding row: one vector per feedback record per model.
type Embedding struct {
	ID               uuid.UUID `json:"id"`
	FeedbackRecordID uuid.UUID `json:"feedback_record_id"`
	Embedding        []float32 `json:"embedding"`
	Model            string    `json:"model"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// FeedbackRecordWithScore is a feedback record ID, similarity score, and the record's field_label and value_text for display.
// Embeddings exist only for text, so ValueText is always set for any search result.
type FeedbackRecordWithScore struct {
	FeedbackRecordID uuid.UUID `json:"feedback_record_id"`
	Score            float64   `json:"score"`
	FieldLabel       string    `json:"field_label"` // label of the field (included in embedding for context)
	ValueText        string    `json:"value_text"`  // text that was embedded (with field_label)
	SourceID         string    `json:"source_id"`
	SourceName       string    `json:"source_name"`
	SubmissionID     string    `json:"submission_id"`
	CollectedAt      time.Time `json:"collected_at"`
	Sentiment        string    `json:"sentiment,omitempty"`        // positive, negative, neutral (from metadata)
	SentimentScore   float64   `json:"sentiment_score,omitempty"` // confidence 0-1 (from metadata)
}

// SearchFilters holds optional filters for semantic search queries.
type SearchFilters struct {
	SourceID *string
	Since    *time.Time
	Until    *time.Time
}
