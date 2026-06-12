package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aadityya4real/Url-shortener-service/internal/model"
)

type SQLiteRepository struct {
	db *sql.DB
}

func NewSQLite(db *sql.DB) *SQLiteRepository {
	return &SQLiteRepository{db: db}
}

func (r *SQLiteRepository) Create(ctx context.Context, link *model.Link) error {
	result, err := r.db.ExecContext(
		ctx,
		`INSERT INTO links (code, original_url, created_at, expires_at, visits)
		 VALUES (?, ?, ?, ?, ?)`,
		link.Code,
		link.OriginalURL,
		formatTime(link.CreatedAt),
		formatNullableTime(link.ExpiresAt),
		link.Visits,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return ErrConflict
		}
		return fmt.Errorf("insert link: %w", err)
	}

	link.ID, err = result.LastInsertId()
	if err != nil {
		return fmt.Errorf("read inserted id: %w", err)
	}
	return nil
}

func (r *SQLiteRepository) GetByCode(ctx context.Context, code string) (*model.Link, error) {
	row := r.db.QueryRowContext(
		ctx,
		`SELECT id, code, original_url, created_at, expires_at, visits
		 FROM links WHERE code = ?`,
		code,
	)
	return scanLink(row)
}

func (r *SQLiteRepository) Resolve(ctx context.Context, code string) (*model.Link, error) {
	now := formatTime(time.Now().UTC())
	row := r.db.QueryRowContext(
		ctx,
		`UPDATE links
		 SET visits = visits + 1
		 WHERE code = ? AND (expires_at IS NULL OR julianday(expires_at) > julianday(?))
		 RETURNING id, code, original_url, created_at, expires_at, visits`,
		code,
		now,
	)
	return scanLink(row)
}

func (r *SQLiteRepository) Delete(ctx context.Context, code string) error {
	result, err := r.db.ExecContext(ctx, `DELETE FROM links WHERE code = ?`, code)
	if err != nil {
		return fmt.Errorf("delete link: %w", err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("read affected rows: %w", err)
	}
	if affected == 0 {
		return ErrNotFound
	}
	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanLink(row rowScanner) (*model.Link, error) {
	var (
		link      model.Link
		createdAt string
		expiresAt sql.NullString
	)
	if err := row.Scan(
		&link.ID,
		&link.Code,
		&link.OriginalURL,
		&createdAt,
		&expiresAt,
		&link.Visits,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("scan link: %w", err)
	}

	var err error
	link.CreatedAt, err = time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return nil, fmt.Errorf("parse created_at: %w", err)
	}
	if expiresAt.Valid {
		parsed, parseErr := time.Parse(time.RFC3339Nano, expiresAt.String)
		if parseErr != nil {
			return nil, fmt.Errorf("parse expires_at: %w", parseErr)
		}
		link.ExpiresAt = &parsed
	}

	return &link, nil
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func formatNullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}
	return formatTime(*value)
}

func isUniqueViolation(err error) bool {
	return strings.Contains(strings.ToLower(err.Error()), "unique constraint failed")
}
