package questionbank

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// AuditInput comes from the authenticated transport, never from an actor in JSON.
// Direct legacy callers may omit attribution; unknown identities remain NULL.
type AuditInput struct {
	Actor           string
	Reason          string
	ExpectedVersion int
}

type AuditEvent struct {
	ID        int64           `json:"id"`
	Version   int             `json:"version"`
	Action    string          `json:"action"`
	Actor     *string         `json:"actor"`
	Reason    *string         `json:"reason"`
	Before    json.RawMessage `json:"before_state"`
	After     json.RawMessage `json:"after_state"`
	CreatedAt time.Time       `json:"created_at"`
}

func versionSnapshot(ctx context.Context, tx pgx.Tx, id string, version int) (json.RawMessage, error) {
	var raw json.RawMessage
	err := tx.QueryRow(ctx, `SELECT to_jsonb(v) FROM question_bank_versions v WHERE question_id=$1 AND version=$2`, id, version).Scan(&raw)
	return raw, err
}

func recordAudit(ctx context.Context, tx pgx.Tx, id string, version int, action string, before json.RawMessage, reason string, inputs []AuditInput) error {
	var actor string
	if len(inputs) > 0 {
		actor = strings.TrimSpace(inputs[0].Actor)
		if inputs[0].Reason != "" {
			reason = inputs[0].Reason
		}
	}
	reason = strings.TrimSpace(reason)
	if len([]rune(reason)) > 2000 {
		return fmt.Errorf("%w: reason must not exceed 2000 characters", ErrInvalidInput)
	}
	after, err := versionSnapshot(ctx, tx, id, version)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO question_bank_audit(question_id,version,action,actor,reason,before_state,after_state)
		VALUES ($1,$2,$3,NULLIF($4,''),NULLIF($5,''),$6,$7)`, id, version, action, actor, reason, before, after)
	return err
}

func (s *Store) AuditEvents(ctx context.Context, id string) ([]AuditEvent, error) {
	if s == nil || s.pool == nil {
		return nil, ErrNotConfigured
	}
	rows, err := s.pool.Query(ctx, `SELECT id,version,action,actor,reason,before_state,after_state,created_at
		FROM question_bank_audit WHERE question_id=$1 ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := make([]AuditEvent, 0)
	for rows.Next() {
		var event AuditEvent
		if err := rows.Scan(&event.ID, &event.Version, &event.Action, &event.Actor, &event.Reason, &event.Before, &event.After, &event.CreatedAt); err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	return events, rows.Err()
}
