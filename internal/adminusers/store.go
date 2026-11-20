package adminusers

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yhwyxy/examSystem/internal/auth"
	"golang.org/x/crypto/bcrypt"
)

var (
	ErrInvalidUser  = errors.New("invalid admin user")
	ErrUserExists   = errors.New("admin username already exists")
	ErrUserNotFound = errors.New("admin user not found")
	ErrLastAdmin    = errors.New("cannot disable or demote the last active admin")
	ErrSelfDemotion = errors.New("cannot disable or demote the current admin account")
)

var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{2,63}$`)

const (
	RoleAdmin              = auth.RoleAdmin
	RoleQuestionBankEditor = auth.RoleQuestionBankEditor
	RoleReviewer           = auth.RoleReviewer
	RolePublisher          = auth.RolePublisher
)

type User struct {
	Username  string    `json:"username"`
	Role      string    `json:"role"`
	Active    bool      `json:"active"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type CreateInput struct {
	Username string
	Password string
	Role     string
	Actor    string
}

type UpdateInput struct {
	Username string
	Password string
	Role     string
	Active   *bool
	Actor    string
}

type Store struct{ pool *pgxpool.Pool }

func NewStore(pool *pgxpool.Pool) *Store { return &Store{pool: pool} }

func (s *Store) Authenticate(ctx context.Context, username, password string) (string, error) {
	if s == nil || s.pool == nil {
		return "", auth.ErrInvalidCredentials
	}
	username = strings.TrimSpace(username)
	var passwordHash, role string
	var active bool
	err := s.pool.QueryRow(ctx, `SELECT password_hash,role,active FROM admin_users WHERE username=$1`, username).
		Scan(&passwordHash, &role, &active)
	if err != nil || !active || bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(password)) != nil {
		return "", auth.ErrInvalidCredentials
	}
	return role, nil
}

// EnsureBootstrapAdmin imports the existing configured or stored password only
// when no named admin account exists yet. It never overwrites managed accounts.
func (s *Store) EnsureBootstrapAdmin(ctx context.Context, plainPassword, existingHash string) error {
	if s == nil || s.pool == nil {
		return errors.New("admin user store is not configured")
	}
	passwordHash := strings.TrimSpace(existingHash)
	if passwordHash == "" && plainPassword != "" {
		hash, err := bcrypt.GenerateFromPassword([]byte(plainPassword), bcrypt.DefaultCost)
		if err != nil {
			return err
		}
		passwordHash = string(hash)
	}
	if passwordHash == "" {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(0x41444d49)); err != nil {
		return err
	}
	var count int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM admin_users`).Scan(&count); err != nil {
		return err
	}
	if count == 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO admin_users(username,password_hash,role,active) VALUES ('admin',$1,'admin',true)`, passwordHash); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO admin_user_audit(actor,username,action,new_role,new_active,password_changed)
VALUES ('bootstrap','admin','created','admin',true,true)`); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (s *Store) SetAdminPassword(ctx context.Context, password string) error {
	if s == nil || s.pool == nil || len(password) < 6 || len(password) > 72 {
		return ErrInvalidUser
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(0x41444d49)); err != nil {
		return err
	}
	var role string
	var active bool
	if err := tx.QueryRow(ctx, `SELECT role,active FROM admin_users WHERE username='admin' FOR UPDATE`).Scan(&role, &active); errors.Is(err, pgx.ErrNoRows) {
		return ErrUserNotFound
	} else if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE admin_users SET password_hash=$1,updated_at=now() WHERE username='admin'`, string(hash)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO admin_user_audit(actor,username,action,old_role,new_role,old_active,new_active,password_changed)
VALUES ('admin','admin','updated',$1,$1,$2,$2,true)`, role, active); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) List(ctx context.Context) ([]User, error) {
	if s == nil || s.pool == nil {
		return nil, errors.New("admin user store is not configured")
	}
	rows, err := s.pool.Query(ctx, `SELECT username,role,active,created_at,updated_at FROM admin_users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	users := make([]User, 0)
	for rows.Next() {
		var user User
		if err := rows.Scan(&user.Username, &user.Role, &user.Active, &user.CreatedAt, &user.UpdatedAt); err != nil {
			return nil, err
		}
		users = append(users, user)
	}
	return users, rows.Err()
}

func (s *Store) Create(ctx context.Context, in CreateInput) (User, error) {
	if s == nil || s.pool == nil {
		return User{}, errors.New("admin user store is not configured")
	}
	in.Username, in.Role, in.Actor = strings.TrimSpace(in.Username), strings.TrimSpace(in.Role), strings.TrimSpace(in.Actor)
	if !usernamePattern.MatchString(in.Username) || !validRole(in.Role) || len(in.Actor) == 0 || len(in.Actor) > 200 || len(in.Password) < 8 || len(in.Password) > 72 {
		return User{}, ErrInvalidUser
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
	if err != nil {
		return User{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(0x41444d49)); err != nil {
		return User{}, err
	}
	var user User
	err = tx.QueryRow(ctx, `INSERT INTO admin_users(username,password_hash,role,active)
VALUES ($1,$2,$3,true) RETURNING username,role,active,created_at,updated_at`, in.Username, string(hash), in.Role).
		Scan(&user.Username, &user.Role, &user.Active, &user.CreatedAt, &user.UpdatedAt)
	if err != nil {
		if isUniqueViolation(err) {
			return User{}, ErrUserExists
		}
		return User{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO admin_user_audit(actor,username,action,new_role,new_active,password_changed)
VALUES ($1,$2,'created',$3,true,true)`, in.Actor, in.Username, in.Role); err != nil {
		return User{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, err
	}
	return user, nil
}

func (s *Store) Update(ctx context.Context, in UpdateInput) (User, error) {
	if s == nil || s.pool == nil {
		return User{}, errors.New("admin user store is not configured")
	}
	in.Username, in.Role, in.Actor = strings.TrimSpace(in.Username), strings.TrimSpace(in.Role), strings.TrimSpace(in.Actor)
	if !usernamePattern.MatchString(in.Username) || (in.Role != "" && !validRole(in.Role)) || len(in.Actor) == 0 || len(in.Actor) > 200 || (in.Password != "" && (len(in.Password) < 8 || len(in.Password) > 72)) || (in.Role == "" && in.Active == nil && in.Password == "") {
		return User{}, ErrInvalidUser
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return User{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(0x41444d49)); err != nil {
		return User{}, err
	}
	var old User
	if err := tx.QueryRow(ctx, `SELECT username,role,active,created_at,updated_at FROM admin_users WHERE username=$1 FOR UPDATE`, in.Username).
		Scan(&old.Username, &old.Role, &old.Active, &old.CreatedAt, &old.UpdatedAt); errors.Is(err, pgx.ErrNoRows) {
		return User{}, ErrUserNotFound
	} else if err != nil {
		return User{}, err
	}
	newRole, newActive := old.Role, old.Active
	if in.Role != "" {
		newRole = in.Role
	}
	if in.Active != nil {
		newActive = *in.Active
	}
	if old.Username == "admin" && (newRole != RoleAdmin || !newActive) {
		return User{}, ErrSelfDemotion
	}
	if old.Role == RoleAdmin && old.Active && (newRole != RoleAdmin || !newActive) {
		var otherAdmins int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM admin_users WHERE role='admin' AND active=true AND username<>$1`, in.Username).Scan(&otherAdmins); err != nil {
			return User{}, err
		}
		if otherAdmins == 0 {
			return User{}, ErrLastAdmin
		}
	}
	var passwordHash any
	passwordChanged := in.Password != ""
	if passwordChanged {
		hash, err := bcrypt.GenerateFromPassword([]byte(in.Password), bcrypt.DefaultCost)
		if err != nil {
			return User{}, err
		}
		passwordHash = string(hash)
	}
	var user User
	err = tx.QueryRow(ctx, `UPDATE admin_users SET role=$2,active=$3,password_hash=COALESCE($4,password_hash),updated_at=now()
WHERE username=$1 RETURNING username,role,active,created_at,updated_at`, in.Username, newRole, newActive, passwordHash).
		Scan(&user.Username, &user.Role, &user.Active, &user.CreatedAt, &user.UpdatedAt)
	if err != nil {
		return User{}, err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO admin_user_audit(actor,username,action,old_role,new_role,old_active,new_active,password_changed)
VALUES ($1,$2,'updated',$3,$4,$5,$6,$7)`, in.Actor, user.Username, old.Role, user.Role, old.Active, user.Active, passwordChanged); err != nil {
		return User{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return User{}, err
	}
	return user, nil
}

func validRole(role string) bool {
	switch role {
	case RoleAdmin, RoleQuestionBankEditor, RoleReviewer, RolePublisher:
		return true
	default:
		return false
	}
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
