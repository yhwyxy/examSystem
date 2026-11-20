package questionbank

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

var ErrInsufficientQuestions = errors.New("not enough enabled questions match the sampling criteria")

// Sample chooses distinct current enabled versions in one database snapshot.
// Selection is a preview; Assemble revalidates versions before paper insertion.
func (s *Store) Sample(ctx context.Context, filter Filter, count int, excludeIDs []string) ([]Item, error) {
	if s == nil || s.pool == nil {
		return nil, ErrNotConfigured
	}
	if count < 1 || count > 200 || len(excludeIDs) > 1000 {
		return nil, fmt.Errorf("%w: sample count must be 1 to 200; at most 1000 exclusions", ErrInvalidInput)
	}
	excluded := make([]string, 0, len(excludeIDs))
	for _, id := range excludeIDs {
		id = strings.TrimSpace(id)
		if id == "" || len(id) > 200 {
			return nil, fmt.Errorf("%w: exclusions need nonempty question ids of at most 200 bytes", ErrInvalidInput)
		}
		excluded = append(excluded, id)
	}
	filter.Status = "enabled"
	where, args := filterSQL(filter)
	args = append(args, excluded, count)
	query := `SELECT v.question_id,v.version,true,v.discipline,v.difficulty,v.knowledge_points,
		v.status,v.reviewed_at,v.created_at,v.question_json
		FROM question_bank_questions q JOIN question_bank_versions v
		ON v.question_id=q.id AND v.version=q.current_version ` + where +
		fmt.Sprintf(` AND NOT (v.question_id=ANY($%d::text[])) ORDER BY random() LIMIT $%d`, len(args)-1, len(args))
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Item, 0, count)
	for rows.Next() {
		item, err := scanItem(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, *item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(items) != count {
		return nil, ErrInsufficientQuestions
	}
	return items, nil
}
