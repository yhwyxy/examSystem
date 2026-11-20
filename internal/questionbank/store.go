// Package questionbank stores reusable, versioned questions independently of exam papers.
package questionbank

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yhwyxy/examSystem/internal/papers"
)

var (
	ErrNotConfigured   = errors.New("question bank is not configured")
	ErrNotFound        = errors.New("question bank item not found")
	ErrAlreadyExists   = errors.New("question bank item already exists")
	ErrVersionConflict = errors.New("question bank version conflict")
	ErrInvalidState    = errors.New("invalid question bank state transition")
	ErrInvalidInput    = errors.New("invalid question bank input")
)

type Input struct {
	ID              string          `json:"id,omitempty"`
	Discipline      string          `json:"discipline"`
	Difficulty      string          `json:"difficulty"`
	KnowledgePoints []string        `json:"knowledge_points"`
	Question        papers.Question `json:"question"`
	Reason          string          `json:"reason,omitempty"`
}

type Item struct {
	ID              string          `json:"id"`
	Version         int             `json:"version"`
	Current         bool            `json:"current"`
	Discipline      string          `json:"discipline"`
	Difficulty      string          `json:"difficulty"`
	KnowledgePoints []string        `json:"knowledge_points"`
	Status          string          `json:"status"`
	ReviewedAt      *time.Time      `json:"reviewed_at,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	Question        papers.Question `json:"question"`
}

type Filter struct {
	Discipline     string
	Difficulty     string
	KnowledgePoint string
	QuestionType   string
	Status         string
	Search         string
	Limit          int
	Offset         int
}

type Reference struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
}

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Create(ctx context.Context, in Input, audit ...AuditInput) (*Item, error) {
	if s == nil || s.pool == nil {
		return nil, ErrNotConfigured
	}
	normalized, err := normalize(in)
	if err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	item, err := insertInitial(ctx, tx, normalized)
	if err != nil {
		return nil, err
	}
	if err := recordAudit(ctx, tx, normalized.ID, item.Version, "create", nil, in.Reason, audit); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return item, nil
}

func (s *Store) Update(ctx context.Context, id string, expectedVersion int, in Input, audit ...AuditInput) (*Item, error) {
	if s == nil || s.pool == nil {
		return nil, ErrNotConfigured
	}
	if expectedVersion < 1 {
		return nil, fmt.Errorf("%w: expected_version must be positive", ErrInvalidInput)
	}
	in.ID = id
	normalized, err := normalize(in)
	if err != nil {
		return nil, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var current int
	if err := tx.QueryRow(ctx, `SELECT current_version FROM question_bank_questions WHERE id=$1 FOR UPDATE`, id).Scan(&current); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	if current != expectedVersion {
		return nil, ErrVersionConflict
	}
	before, err := versionSnapshot(ctx, tx, id, current)
	if err != nil {
		return nil, err
	}
	var currentQuestion []byte
	if err := tx.QueryRow(ctx, `SELECT question_json FROM question_bank_versions WHERE question_id=$1 AND version=$2`, id, current).Scan(&currentQuestion); err != nil {
		return nil, err
	}
	var old papers.Question
	if err := json.Unmarshal(currentQuestion, &old); err != nil {
		return nil, err
	}
	if oldID, _ := old["id"].(string); oldID != id {
		return nil, fmt.Errorf("%w: stored question id does not match bank id", ErrInvalidInput)
	}
	if _, err := tx.Exec(ctx, `UPDATE question_bank_versions SET status='superseded' WHERE question_id=$1 AND version=$2`, id, current); err != nil {
		return nil, err
	}
	next := current + 1
	item, err := insertVersion(ctx, tx, normalized, next, "draft")
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE question_bank_questions SET current_version=$2, updated_at=now() WHERE id=$1`, id, next); err != nil {
		return nil, err
	}
	if err := recordAudit(ctx, tx, id, next, "update", before, in.Reason, audit); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return item, nil
}

func (s *Store) Get(ctx context.Context, id string, version int) (*Item, error) {
	if s == nil || s.pool == nil {
		return nil, ErrNotConfigured
	}
	query := `SELECT v.question_id,v.version,(v.version=q.current_version),v.discipline,v.difficulty,
		v.knowledge_points,v.status,v.reviewed_at,v.created_at,v.question_json
		FROM question_bank_questions q JOIN question_bank_versions v ON v.question_id=q.id
		WHERE q.id=$1`
	args := []any{id}
	if version > 0 {
		query += ` AND v.version=$2`
		args = append(args, version)
	} else {
		query += ` AND v.version=q.current_version`
	}
	item, err := scanItem(s.pool.QueryRow(ctx, query, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return item, err
}

func (s *Store) List(ctx context.Context, filter Filter) ([]Item, int, error) {
	if s == nil || s.pool == nil {
		return nil, 0, ErrNotConfigured
	}
	if filter.Limit <= 0 || filter.Limit > 200 {
		filter.Limit = 50
	}
	if filter.Offset < 0 {
		filter.Offset = 0
	}
	where, args := filterSQL(filter)
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM question_bank_questions q
		JOIN question_bank_versions v ON v.question_id=q.id AND v.version=q.current_version `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, filter.Limit, filter.Offset)
	query := `SELECT v.question_id,v.version,true,v.discipline,v.difficulty,v.knowledge_points,
		v.status,v.reviewed_at,v.created_at,v.question_json
		FROM question_bank_questions q JOIN question_bank_versions v
		ON v.question_id=q.id AND v.version=q.current_version ` + where +
		fmt.Sprintf(` ORDER BY v.created_at DESC,v.question_id LIMIT $%d OFFSET $%d`, len(args)-1, len(args))
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := make([]Item, 0)
	for rows.Next() {
		item, err := scanItem(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, *item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return items, total, nil
}

func (s *Store) Versions(ctx context.Context, id string) ([]Item, error) {
	if s == nil || s.pool == nil {
		return nil, ErrNotConfigured
	}
	rows, err := s.pool.Query(ctx, `SELECT v.question_id,v.version,(v.version=q.current_version),v.discipline,
		v.difficulty,v.knowledge_points,v.status,v.reviewed_at,v.created_at,v.question_json
		FROM question_bank_questions q JOIN question_bank_versions v ON v.question_id=q.id
		WHERE q.id=$1 ORDER BY v.version DESC`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]Item, 0)
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
	if len(items) == 0 {
		return nil, ErrNotFound
	}
	return items, nil
}

// Assemble returns immutable content for selected current, enabled versions from one
// consistent database snapshot. Callers embed these documents into an exam paper.
func (s *Store) Assemble(ctx context.Context, refs []Reference) ([]Item, error) {
	if s == nil || s.pool == nil {
		return nil, ErrNotConfigured
	}
	if len(refs) == 0 || len(refs) > 200 {
		return nil, fmt.Errorf("%w: assembly requires 1 to 200 questions", ErrInvalidInput)
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	seen := make(map[string]struct{}, len(refs))
	items := make([]Item, 0, len(refs))
	for _, ref := range refs {
		if strings.TrimSpace(ref.ID) == "" || ref.Version < 1 {
			return nil, fmt.Errorf("%w: assembly references need an id and positive version", ErrInvalidInput)
		}
		key := fmt.Sprintf("%s/%d", ref.ID, ref.Version)
		if _, exists := seen[key]; exists {
			return nil, fmt.Errorf("%w: duplicate assembly reference %s", ErrInvalidInput, key)
		}
		seen[key] = struct{}{}
		item, err := scanItem(tx.QueryRow(ctx, `SELECT v.question_id,v.version,true,v.discipline,v.difficulty,
			v.knowledge_points,v.status,v.reviewed_at,v.created_at,v.question_json
			FROM question_bank_questions q JOIN question_bank_versions v
			ON v.question_id=q.id AND v.version=q.current_version
			WHERE q.id=$1 AND q.current_version=$2 AND v.status='enabled'`, ref.ID, ref.Version))
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w: question %s version %d is not current and enabled", ErrInvalidState, ref.ID, ref.Version)
		}
		if err != nil {
			return nil, err
		}
		items = append(items, *item)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return items, nil
}

// Transition changes only the current version's lifecycle. Question content remains versioned.
func (s *Store) Transition(ctx context.Context, id, action string, audit ...AuditInput) (*Item, error) {
	if s == nil || s.pool == nil {
		return nil, ErrNotConfigured
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var version int
	if err := tx.QueryRow(ctx, `SELECT current_version FROM question_bank_questions WHERE id=$1 FOR UPDATE`, id).Scan(&version); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	var status string
	if len(audit) > 0 && audit[0].ExpectedVersion > 0 && audit[0].ExpectedVersion != version {
		return nil, ErrVersionConflict
	}
	var reviewedAt *time.Time
	if err := tx.QueryRow(ctx, `SELECT status,reviewed_at FROM question_bank_versions WHERE question_id=$1 AND version=$2 FOR UPDATE`, id, version).Scan(&status, &reviewedAt); err != nil {
		return nil, err
	}
	before, err := versionSnapshot(ctx, tx, id, version)
	if err != nil {
		return nil, err
	}
	var next string
	switch action {
	case "review":
		if status != "draft" {
			return nil, ErrInvalidState
		}
		next = "reviewed"
	case "enable":
		if status != "reviewed" && !(status == "disabled" && reviewedAt != nil) {
			return nil, ErrInvalidState
		}
		next = "enabled"
	case "disable":
		if status != "enabled" {
			return nil, ErrInvalidState
		}
		next = "disabled"
	case "archive":
		if status == "enabled" || status == "archived" || status == "superseded" {
			return nil, ErrInvalidState
		}
		next = "archived"
	default:
		return nil, fmt.Errorf("%w: unknown action %q", ErrInvalidInput, action)
	}
	if action == "review" {
		_, err = tx.Exec(ctx, `UPDATE question_bank_versions SET status=$3,reviewed_at=now() WHERE question_id=$1 AND version=$2`, id, version, next)
	} else {
		_, err = tx.Exec(ctx, `UPDATE question_bank_versions SET status=$3 WHERE question_id=$1 AND version=$2`, id, version, next)
	}
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE question_bank_questions SET updated_at=now() WHERE id=$1`, id); err != nil {
		return nil, err
	}
	item, err := scanItem(tx.QueryRow(ctx, `SELECT v.question_id,v.version,true,v.discipline,v.difficulty,
		v.knowledge_points,v.status,v.reviewed_at,v.created_at,v.question_json
		FROM question_bank_questions q JOIN question_bank_versions v
		ON v.question_id=q.id AND v.version=q.current_version WHERE q.id=$1`, id))
	if err != nil {
		return nil, err
	}
	if err := recordAudit(ctx, tx, id, version, action, before, "", audit); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return item, nil
}

func (s *Store) Import(ctx context.Context, inputs []Input, audit ...AuditInput) ([]Item, error) {
	if s == nil || s.pool == nil {
		return nil, ErrNotConfigured
	}
	if len(inputs) == 0 || len(inputs) > 500 {
		return nil, fmt.Errorf("%w: import requires 1 to 500 items", ErrInvalidInput)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	items := make([]Item, 0, len(inputs))
	seen := make(map[string]struct{}, len(inputs))
	for _, in := range inputs {
		normalized, err := normalize(in)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[normalized.ID]; exists {
			return nil, fmt.Errorf("%w: duplicate id %q within import", ErrAlreadyExists, normalized.ID)
		}
		seen[normalized.ID] = struct{}{}
		item, err := insertInitial(ctx, tx, normalized)
		if err != nil {
			return nil, err
		}
		if err := recordAudit(ctx, tx, normalized.ID, item.Version, "import", nil, in.Reason, audit); err != nil {
			return nil, err
		}
		items = append(items, *item)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return items, nil
}

func normalize(in Input) (Input, error) {
	in.ID = strings.TrimSpace(in.ID)
	if in.ID == "" {
		in.ID = "q-" + uuid.NewString()
	}
	in.Discipline = strings.TrimSpace(in.Discipline)
	in.Difficulty = strings.ToLower(strings.TrimSpace(in.Difficulty))
	if in.Discipline == "" {
		return Input{}, fmt.Errorf("%w: discipline is required", ErrInvalidInput)
	}
	switch in.Difficulty {
	case "easy", "medium", "hard":
	default:
		return Input{}, fmt.Errorf("%w: difficulty must be easy, medium, or hard", ErrInvalidInput)
	}
	points := make([]string, 0, len(in.KnowledgePoints))
	seen := map[string]bool{}
	for _, point := range in.KnowledgePoints {
		point = strings.TrimSpace(point)
		if point == "" {
			return Input{}, fmt.Errorf("%w: knowledge points cannot be empty", ErrInvalidInput)
		}
		key := strings.ToLower(point)
		if seen[key] {
			return Input{}, fmt.Errorf("%w: duplicate knowledge point %q", ErrInvalidInput, point)
		}
		seen[key] = true
		points = append(points, point)
	}
	if len(points) == 0 {
		return Input{}, fmt.Errorf("%w: at least one knowledge point is required", ErrInvalidInput)
	}
	in.KnowledgePoints = points
	if in.Question == nil {
		return Input{}, fmt.Errorf("%w: question is required", ErrInvalidInput)
	}
	if questionID, ok := in.Question["id"].(string); ok && strings.TrimSpace(questionID) != "" && strings.TrimSpace(questionID) != in.ID {
		return Input{}, fmt.Errorf("%w: question.id must match the stable bank id", ErrInvalidInput)
	}
	in.Question["id"] = in.ID
	if err := papers.ValidateStandaloneQuestion(in.Question); err != nil {
		return Input{}, fmt.Errorf("%w: %v", ErrInvalidInput, err)
	}
	return in, nil
}

func insertInitial(ctx context.Context, tx pgx.Tx, in Input) (*Item, error) {
	if _, err := tx.Exec(ctx, `INSERT INTO question_bank_questions(id,current_version) VALUES ($1,1)`, in.ID); err != nil {
		return nil, mapDBError(err)
	}
	return insertVersion(ctx, tx, in, 1, "draft")
}

func insertVersion(ctx context.Context, tx pgx.Tx, in Input, version int, status string) (*Item, error) {
	raw, err := json.Marshal(in.Question)
	if err != nil {
		return nil, fmt.Errorf("%w: question cannot be encoded: %v", ErrInvalidInput, err)
	}
	var reviewedAt *time.Time
	if _, err := tx.Exec(ctx, `INSERT INTO question_bank_versions
		(question_id,version,discipline,difficulty,knowledge_points,question_json,status)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`, in.ID, version, in.Discipline, in.Difficulty, in.KnowledgePoints, raw, status); err != nil {
		return nil, mapDBError(err)
	}
	return &Item{ID: in.ID, Version: version, Current: true, Discipline: in.Discipline,
		Difficulty: in.Difficulty, KnowledgePoints: in.KnowledgePoints, Status: status,
		CreatedAt: time.Now().UTC(), Question: in.Question, ReviewedAt: reviewedAt}, nil
}

func scanItem(row pgx.Row) (*Item, error) {
	item := &Item{}
	var raw []byte
	err := row.Scan(&item.ID, &item.Version, &item.Current, &item.Discipline, &item.Difficulty,
		&item.KnowledgePoints, &item.Status, &item.ReviewedAt, &item.CreatedAt, &raw)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &item.Question); err != nil {
		return nil, err
	}
	return item, nil
}

func filterSQL(f Filter) (string, []any) {
	parts := make([]string, 0, 6)
	args := make([]any, 0, 6)
	add := func(sql string, value any) {
		args = append(args, value)
		parts = append(parts, fmt.Sprintf(sql, len(args)))
	}
	if f.Discipline != "" {
		add("v.discipline=$%d", f.Discipline)
	}
	if f.Difficulty != "" {
		add("v.difficulty=$%d", f.Difficulty)
	}
	if f.KnowledgePoint != "" {
		add("$%d=ANY(v.knowledge_points)", f.KnowledgePoint)
	}
	if f.QuestionType != "" {
		add("v.question_json->>'type'=$%d", f.QuestionType)
	}
	if f.Status != "" {
		add("v.status=$%d", f.Status)
	}
	if f.Search != "" {
		args = append(args, "%"+f.Search+"%", "%"+f.Search+"%")
		parts = append(parts, fmt.Sprintf("(v.question_id ILIKE $%d OR v.question_json::text ILIKE $%d)", len(args)-1, len(args)))
	}
	if len(parts) == 0 {
		return "", args
	}
	return "WHERE " + strings.Join(parts, " AND "), args
}

func mapDBError(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return fmt.Errorf("%w: %s", ErrAlreadyExists, pgErr.Detail)
	}
	return err
}
