package repository

import (
	"context"
	"errors"

	"github.com/aadityya4real/Url-shortener-service/internal/model"
)

var (
	ErrNotFound = errors.New("link not found")
	ErrConflict = errors.New("short code already exists")
)

type LinkRepository interface {
	Create(ctx context.Context, link *model.Link) error
	GetByCode(ctx context.Context, code string) (*model.Link, error)
	Resolve(ctx context.Context, code string) (*model.Link, error)
	Delete(ctx context.Context, code string) error
}
