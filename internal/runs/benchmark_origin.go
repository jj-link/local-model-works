package runs

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
)

var ErrInvalidCursor = errors.New("invalid cursor")

type BenchmarkOriginFilter struct {
	ClientID  string
	RunID     string
	ProjectID string
	Cursor    string
	Limit     int
}

type benchmarkCursor struct {
	CreatedAt string `json:"created_at"`
	ID        string `json:"id"`
}

// ListBenchmarkOrigin returns a stable newest-first page from the complete
// benchmark ledger, including runs which have not published aggregate results.
func (s *Service) ListBenchmarkOrigin(ctx context.Context, filter BenchmarkOriginFilter) ([]Run, string, error) {
	limit := filter.Limit
	if limit == 0 {
		limit = 50
	}
	if limit < 1 || limit > 200 {
		return nil, "", fmt.Errorf("limit must be between 1 and 200")
	}
	cursor := benchmarkCursor{}
	if filter.Cursor != "" {
		raw, err := base64.RawURLEncoding.DecodeString(filter.Cursor)
		if err != nil || json.Unmarshal(raw, &cursor) != nil || cursor.CreatedAt == "" || cursor.ID == "" {
			return nil, "", ErrInvalidCursor
		}
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT id,created_at FROM runs
WHERE module='benchmarks'
  AND (?='' OR json_extract(input,'$.origin.client_id')=?)
  AND (?='' OR json_extract(input,'$.origin.run_id')=?)
  AND (?='' OR json_extract(input,'$.origin.project_id')=?)
  AND (?='' OR created_at<? OR (created_at=? AND id<?))
ORDER BY created_at DESC,id DESC
LIMIT ?`,
		filter.ClientID, filter.ClientID, filter.RunID, filter.RunID,
		filter.ProjectID, filter.ProjectID, cursor.CreatedAt, cursor.CreatedAt,
		cursor.CreatedAt, cursor.ID, limit+1)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	type identity struct{ id, createdAt string }
	identities := make([]identity, 0, limit+1)
	for rows.Next() {
		var item identity
		if err := rows.Scan(&item.id, &item.createdAt); err != nil {
			return nil, "", err
		}
		identities = append(identities, item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(identities) > limit {
		last := identities[limit-1]
		encoded, _ := json.Marshal(benchmarkCursor{CreatedAt: last.createdAt, ID: last.id})
		next = base64.RawURLEncoding.EncodeToString(encoded)
		identities = identities[:limit]
	}
	out := make([]Run, 0, len(identities))
	for _, item := range identities {
		run, err := s.Get(ctx, item.id)
		if err != nil {
			return nil, "", err
		}
		out = append(out, run)
	}
	return out, next, nil
}
