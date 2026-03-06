package service

import (
	"github.com/google/uuid"
	"github.com/riverqueue/river"
)

const (
	sentimentAnalysisKind = "sentiment_analysis"
	// SentimentQueueName is the River queue used for sentiment analysis jobs.
	SentimentQueueName = "sentiment"
)

// SentimentAnalysisArgs is the job payload for running sentiment analysis on one feedback record.
// Uniqueness is by (FeedbackRecordID, ValueTextHash) so that edits within the uniqueness window
// get a new job when value_text changes; same content within 24h is deduped.
type SentimentAnalysisArgs struct {
	FeedbackRecordID uuid.UUID `json:"feedback_record_id" river:"unique"`
	ValueTextHash    string    `json:"value_text_hash" river:"unique"`
}

// Kind returns the River job kind.
func (SentimentAnalysisArgs) Kind() string { return sentimentAnalysisKind }

var _ river.JobArgs = SentimentAnalysisArgs{}
