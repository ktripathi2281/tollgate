package store

import (
	"context"
	"fmt"
	"math"

	"github.com/ktripathi2281/tollgate/internal/store/db"
	"github.com/ktripathi2281/tollgate/internal/usage"
)

// WriteRequestLogs stores a batch of request-log rows with one COPY. It
// implements usage.Writer.
func (s *Store) WriteRequestLogs(ctx context.Context, entries []usage.Entry) error {
	rows := make([]db.InsertRequestLogsParams, 0, len(entries))
	for _, e := range entries {
		rows = append(rows, db.InsertRequestLogsParams{
			CreatedAt:        e.Time,
			RequestID:        e.RequestID,
			KeyID:            nonZero(e.KeyID),
			Alias:            nonZero(e.Alias),
			Provider:         nonZero(e.Provider),
			Model:            nonZero(e.Model),
			Stream:           e.Stream,
			Status:           int32(e.Status),
			ErrorCode:        nonZero(e.ErrorCode),
			Attempts:         clamp32(e.Attempts),
			PromptTokens:     clamp32(e.PromptTokens),
			CompletionTokens: clamp32(e.CompletionTokens),
			UsageEstimated:   e.UsageEstimated,
			CostMicros:       int64(e.Cost),
			LatencyUs:        e.Latency.Microseconds(),
			TtftUs:           nonZero(e.TTFT.Microseconds()),
			Replayed:         e.Replayed,
		})
	}
	if _, err := s.q.InsertRequestLogs(ctx, rows); err != nil {
		return fmt.Errorf("write %d request logs: %w", len(rows), err)
	}
	return nil
}

// Ping checks that the database answers, for the readiness check.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// nonZero returns a pointer to v, or nil for the zero value, which the
// request log stores as NULL.
func nonZero[T comparable](v T) *T {
	var zero T
	if v == zero {
		return nil
	}
	return &v
}

// clamp32 converts a count to a 32-bit column, capping it instead of
// letting it wrap.
func clamp32(n int) int32 {
	return int32(min(max(n, 0), math.MaxInt32))
}
