package repository

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"

	"github.com/amrhym/hivecfm-hub/internal/models"
)

const (
	// hnswEfSearch increases HNSW graph traversal candidates (default 40); higher improves recall.
	hnswEfSearch = 200
	// nearestOverFetchFactor: request this many times the requested limit, then filter by minScore in Go,
	// so that after tenant/minScore filtering we still have enough results (avoids iterative scan being blocked by WHERE score threshold).
	nearestOverFetchFactor = 3
	// maxNearestFetchLimit caps over-fetched rows per query to avoid excessive memory/scan.
	maxNearestFetchLimit = 2000
)

// ErrEmbeddingDimensionMismatch is returned when an embedding slice length does not match EmbeddingVectorDimensions.
var ErrEmbeddingDimensionMismatch = errors.New("embedding dimension mismatch")

// EmbeddingsRepository handles data access for the embeddings table.
type EmbeddingsRepository struct {
	db *pgxpool.Pool
}

// NewEmbeddingsRepository creates a new embeddings repository.
func NewEmbeddingsRepository(db *pgxpool.Pool) *EmbeddingsRepository {
	return &EmbeddingsRepository{db: db}
}

// Upsert inserts or updates the embedding for (feedback_record_id, model). On conflict updates embedding and updated_at.
// Uses halfvec storage (2 bytes per dimension); pgvector-go converts float32 to float16 when encoding.
// embedding must have length models.EmbeddingVectorDimensions (fixed 768).
func (r *EmbeddingsRepository) Upsert(
	ctx context.Context, feedbackRecordID uuid.UUID, model string, embedding []float32,
) error {
	if len(embedding) != models.EmbeddingVectorDimensions {
		return fmt.Errorf("%w: got %d, want %d", ErrEmbeddingDimensionMismatch, len(embedding), models.EmbeddingVectorDimensions)
	}

	vec := pgvector.NewHalfVector(embedding)
	now := time.Now()

	_, err := r.db.Exec(ctx, `
		INSERT INTO embeddings (feedback_record_id, embedding, model, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (feedback_record_id, model)
		DO UPDATE SET embedding = EXCLUDED.embedding, updated_at = $5`,
		feedbackRecordID, vec, model, now, now,
	)
	if err != nil {
		return fmt.Errorf("embeddings upsert: %w", err)
	}

	return nil
}

// DeleteByFeedbackRecordAndModel removes the embedding row for the given feedback record and model.
func (r *EmbeddingsRepository) DeleteByFeedbackRecordAndModel(
	ctx context.Context, feedbackRecordID uuid.UUID, model string,
) error {
	_, err := r.db.Exec(ctx,
		`DELETE FROM embeddings WHERE feedback_record_id = $1 AND model = $2`,
		feedbackRecordID, model,
	)
	if err != nil {
		return fmt.Errorf("embeddings delete: %w", err)
	}

	return nil
}

// ListFeedbackRecordIDsForBackfill returns IDs of feedback records that have non-empty value_text
// and no row in embeddings for the given model (so they need an embedding for that model).
func (r *EmbeddingsRepository) ListFeedbackRecordIDsForBackfill(ctx context.Context, model string) ([]uuid.UUID, error) {
	rows, err := r.db.Query(ctx, `
		SELECT fr.id FROM feedback_records fr
		WHERE fr.value_text IS NOT NULL AND trim(fr.value_text) != ''
		  AND NOT EXISTS (
		    SELECT 1 FROM embeddings e
		    WHERE e.feedback_record_id = fr.id AND e.model = $1
		  )
	`, model)
	if err != nil {
		return nil, fmt.Errorf("list feedback record ids for backfill: %w", err)
	}
	defer rows.Close()

	var ids []uuid.UUID

	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan feedback record id: %w", err)
		}

		ids = append(ids, id)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating backfill ids: %w", err)
	}

	return ids, nil
}

// ErrEmbeddingNotFound is returned when no embedding row exists for the given feedback record and model.
var ErrEmbeddingNotFound = errors.New("embedding not found for feedback record and model")

// GetEmbeddingByFeedbackRecordAndModel returns the stored embedding for the given feedback record and model.
// Returns ErrEmbeddingNotFound when no row exists (record not embedded yet).
func (r *EmbeddingsRepository) GetEmbeddingByFeedbackRecordAndModel(
	ctx context.Context, feedbackRecordID uuid.UUID, model string,
) ([]float32, error) {
	var vec pgvector.HalfVector

	err := r.db.QueryRow(ctx,
		`SELECT embedding FROM embeddings WHERE feedback_record_id = $1 AND model = $2`,
		feedbackRecordID, model,
	).Scan(&vec)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrEmbeddingNotFound
		}

		return nil, fmt.Errorf("get embedding: %w", err)
	}

	return vec.Slice(), nil
}

// GetEmbeddingByFeedbackRecordAndModelAndTenant returns the stored embedding only when the feedback record
// belongs to the given tenant. Used by SimilarFeedback to enforce tenant isolation (source record must match tenant).
// Returns ErrEmbeddingNotFound when no row exists or tenant does not match.
func (r *EmbeddingsRepository) GetEmbeddingByFeedbackRecordAndModelAndTenant(
	ctx context.Context, feedbackRecordID uuid.UUID, model, tenantID string,
) ([]float32, error) {
	var vec pgvector.HalfVector

	err := r.db.QueryRow(ctx,
		`SELECT e.embedding FROM embeddings e
		 INNER JOIN feedback_records fr ON fr.id = e.feedback_record_id
		 WHERE e.feedback_record_id = $1 AND e.model = $2 AND fr.tenant_id = $3`,
		feedbackRecordID, model, tenantID,
	).Scan(&vec)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrEmbeddingNotFound
		}

		return nil, fmt.Errorf("get embedding by tenant: %w", err)
	}

	return vec.Slice(), nil
}

// NearestFeedbackRecordsByEmbedding returns feedback record IDs and similarity scores (0..1) for the
// nearest neighbors to queryEmbedding, filtered by model and tenant. Rows with score < minScore are
// filtered in application code (not in WHERE) so pgvector's iterative index scan can run. Uses
// full-precision query vector (no quantization); sets hnsw.ef_search for better recall. Over-fetches
// then trims to limit to account for tenant/minScore filtering. excludeID optionally excludes one
// feedback record (e.g. for "similar" endpoint). First page only; use NearestFeedbackRecordsByEmbeddingAfterCursor for next pages.
// filters is optional; when non-nil, source_id/since/until narrow results.
func (r *EmbeddingsRepository) NearestFeedbackRecordsByEmbedding(
	ctx context.Context, model string, queryEmbedding []float32, tenantID string, limit int, excludeID *uuid.UUID, minScore float64,
	filters *models.SearchFilters,
) ([]models.FeedbackRecordWithScore, bool, error) {
	if len(queryEmbedding) != models.EmbeddingVectorDimensions {
		return nil, false, fmt.Errorf("%w: got %d, want %d", ErrEmbeddingDimensionMismatch, len(queryEmbedding), models.EmbeddingVectorDimensions)
	}

	queryVec := pgvector.NewVector(queryEmbedding)
	fetchLimit := min(limit*nearestOverFetchFactor, maxNearestFetchLimit)

	dbTx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, false, fmt.Errorf("begin tx: %w", err)
	}

	defer func() {
		if err := dbTx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			slog.Error("nearest feedback records: rollback failed", "error", err)
		}
	}()

	if _, err := dbTx.Exec(ctx, fmt.Sprintf("SET LOCAL hnsw.ef_search = %d", hnswEfSearch)); err != nil {
		return nil, false, fmt.Errorf("set hnsw.ef_search: %w", err)
	}

	// Build dynamic query with optional filters.
	args := []any{queryVec, model, tenantID}
	whereClauses := []string{"e.model = $2", "fr.tenant_id = $3"}

	nextParam := 4
	if excludeID != nil {
		whereClauses = append(whereClauses, fmt.Sprintf("e.feedback_record_id != $%d", nextParam))
		args = append(args, *excludeID)
		nextParam++
	}

	nextParam = appendFilterClauses(&whereClauses, &args, nextParam, filters)

	query := fmt.Sprintf(`
		SELECT e.feedback_record_id, (1 - (e.embedding <=> $1)) AS score,
			COALESCE(fr.field_label, ''), fr.value_text,
			COALESCE(fr.source_id, ''), COALESCE(fr.source_name, ''),
			fr.submission_id, fr.collected_at, fr.metadata
		FROM embeddings e
		INNER JOIN feedback_records fr ON fr.id = e.feedback_record_id
		WHERE %s
		ORDER BY (e.embedding <=> $1), e.feedback_record_id
		LIMIT $%d`, strings.Join(whereClauses, " AND "), nextParam)
	args = append(args, fetchLimit)

	rows, err := dbTx.Query(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("nearest feedback records: %w", err)
	}

	defer rows.Close()

	results, rowCount, brokeWithFullPage, err := scanEnrichedResults(rows, limit, minScore)
	if err != nil {
		return nil, false, err
	}

	rows.Close()

	if err := dbTx.Commit(ctx); err != nil {
		slog.Error("nearest feedback records: commit failed", "error", err)
		return nil, false, fmt.Errorf("commit: %w", err)
	}

	hasMore := brokeWithFullPage || rowCount >= fetchLimit
	return results, hasMore, nil
}

// NearestFeedbackRecordsByEmbeddingAfterCursor returns the next page of nearest neighbors after the given
// cursor (lastDistance, lastFeedbackRecordID). Order is by (distance ASC, feedback_record_id ASC). minScore
// is applied in application code; query uses full-precision vector and hnsw.ef_search like NearestFeedbackRecordsByEmbedding.
// filters is optional; when non-nil, source_id/since/until narrow results.
func (r *EmbeddingsRepository) NearestFeedbackRecordsByEmbeddingAfterCursor(
	ctx context.Context, model string, queryEmbedding []float32, tenantID string, limit int,
	lastDistance float64, lastFeedbackRecordID uuid.UUID, excludeID *uuid.UUID, minScore float64,
	filters *models.SearchFilters,
) ([]models.FeedbackRecordWithScore, bool, error) {
	if len(queryEmbedding) != models.EmbeddingVectorDimensions {
		return nil, false, fmt.Errorf("%w: got %d, want %d", ErrEmbeddingDimensionMismatch, len(queryEmbedding), models.EmbeddingVectorDimensions)
	}

	queryVec := pgvector.NewVector(queryEmbedding)
	fetchLimit := min(limit*nearestOverFetchFactor, maxNearestFetchLimit)

	dbTx, err := r.db.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, false, fmt.Errorf("begin tx: %w", err)
	}

	defer func() {
		if err := dbTx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			slog.Error("nearest feedback records after cursor: rollback failed", "error", err)
		}
	}()

	if _, err := dbTx.Exec(ctx, fmt.Sprintf("SET LOCAL hnsw.ef_search = %d", hnswEfSearch)); err != nil {
		return nil, false, fmt.Errorf("set hnsw.ef_search: %w", err)
	}

	// Build dynamic query with cursor and optional filters.
	args := []any{queryVec, model, tenantID}
	whereClauses := []string{"e.model = $2", "fr.tenant_id = $3"}

	nextParam := 4
	if excludeID != nil {
		whereClauses = append(whereClauses, fmt.Sprintf("e.feedback_record_id != $%d", nextParam))
		args = append(args, *excludeID)
		nextParam++
	}

	// Cursor condition.
	whereClauses = append(whereClauses, fmt.Sprintf("((e.embedding <=> $1), e.feedback_record_id) > ($%d, $%d)", nextParam, nextParam+1))
	args = append(args, lastDistance, lastFeedbackRecordID)
	nextParam += 2

	nextParam = appendFilterClauses(&whereClauses, &args, nextParam, filters)

	query := fmt.Sprintf(`
		SELECT e.feedback_record_id, (1 - (e.embedding <=> $1)) AS score,
			COALESCE(fr.field_label, ''), fr.value_text,
			COALESCE(fr.source_id, ''), COALESCE(fr.source_name, ''),
			fr.submission_id, fr.collected_at, fr.metadata
		FROM embeddings e
		INNER JOIN feedback_records fr ON fr.id = e.feedback_record_id
		WHERE %s
		ORDER BY (e.embedding <=> $1), e.feedback_record_id
		LIMIT $%d`, strings.Join(whereClauses, " AND "), nextParam)
	args = append(args, fetchLimit)

	rows, err := dbTx.Query(ctx, query, args...)
	if err != nil {
		return nil, false, fmt.Errorf("nearest feedback records after cursor: %w", err)
	}

	defer rows.Close()

	results, rowCount, brokeWithFullPage, err := scanEnrichedResults(rows, limit, minScore)
	if err != nil {
		return nil, false, err
	}

	rows.Close()

	if err := dbTx.Commit(ctx); err != nil {
		slog.Error("nearest feedback records after cursor: commit failed", "error", err)
		return nil, false, fmt.Errorf("commit: %w", err)
	}

	hasMore := brokeWithFullPage || rowCount >= fetchLimit
	return results, hasMore, nil
}

// appendFilterClauses adds optional source_id, since, until WHERE conditions. Returns the next available parameter number.
func appendFilterClauses(whereClauses *[]string, args *[]any, nextParam int, filters *models.SearchFilters) int {
	if filters == nil {
		return nextParam
	}

	if filters.SourceID != nil {
		*whereClauses = append(*whereClauses, fmt.Sprintf("fr.source_id = $%d", nextParam))
		*args = append(*args, *filters.SourceID)
		nextParam++
	}

	if filters.Since != nil {
		*whereClauses = append(*whereClauses, fmt.Sprintf("fr.collected_at >= $%d", nextParam))
		*args = append(*args, *filters.Since)
		nextParam++
	}

	if filters.Until != nil {
		*whereClauses = append(*whereClauses, fmt.Sprintf("fr.collected_at <= $%d", nextParam))
		*args = append(*args, *filters.Until)
		nextParam++
	}

	return nextParam
}

// sentimentMeta holds sentiment fields extracted from metadata JSON.
type sentimentMeta struct {
	Sentiment string  `json:"sentiment"`
	Score     float64 `json:"sentiment_score"`
}

// scanEnrichedResults scans rows from the enriched SELECT into FeedbackRecordWithScore, extracting sentiment from metadata.
func scanEnrichedResults(rows pgx.Rows, limit int, minScore float64) ([]models.FeedbackRecordWithScore, int, bool, error) {
	var (
		results         []models.FeedbackRecordWithScore
		rowCount        int
		brokeWithFullPage bool
	)

	for rows.Next() {
		rowCount++

		var (
			row       models.FeedbackRecordWithScore
			valueText *string
			sourceID  string
			sourceName string
			metadata  []byte
		)

		if err := rows.Scan(
			&row.FeedbackRecordID, &row.Score, &row.FieldLabel, &valueText,
			&sourceID, &sourceName,
			&row.SubmissionID, &row.CollectedAt, &metadata,
		); err != nil {
			return nil, 0, false, fmt.Errorf("scan feedback record with score: %w", err)
		}

		if valueText != nil {
			row.ValueText = *valueText
		}

		row.SourceID = sourceID
		row.SourceName = sourceName

		// Extract sentiment from metadata JSON if present.
		if len(metadata) > 0 {
			var sm sentimentMeta
			if err := json.Unmarshal(metadata, &sm); err == nil {
				row.Sentiment = sm.Sentiment
				row.SentimentScore = sm.Score
			}
		}

		if row.Score >= minScore {
			results = append(results, row)
			if len(results) >= limit {
				brokeWithFullPage = true
				break
			}
		}
	}

	if err := rows.Err(); err != nil {
		return nil, 0, false, fmt.Errorf("iterating nearest: %w", err)
	}

	return results, rowCount, brokeWithFullPage, nil
}
