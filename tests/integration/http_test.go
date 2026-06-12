package integration_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aadityya4real/Url-shortener-service/internal/database"
	"github.com/aadityya4real/Url-shortener-service/internal/handler"
	"github.com/aadityya4real/Url-shortener-service/internal/repository"
	"github.com/aadityya4real/Url-shortener-service/internal/service"
	"github.com/aadityya4real/Url-shortener-service/internal/shortener"
	"github.com/aadityya4real/Url-shortener-service/migrations"
)

func TestURLLifecycle(t *testing.T) {
	app, db := newTestApp(t)
	defer db.Close()

	createBody := []byte(`{"url":"https://example.com/articles/1","custom_alias":"article-1"}`)
	createRequest := httptest.NewRequest(http.MethodPost, "/api/v1/urls", bytes.NewReader(createBody))
	createResponse := httptest.NewRecorder()
	app.ServeHTTP(createResponse, createRequest)
	if createResponse.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d; body=%s", createResponse.Code, http.StatusCreated, createResponse.Body)
	}

	var created struct {
		Code     string `json:"code"`
		ShortURL string `json:"short_url"`
		Visits   int64  `json:"visits"`
	}
	if err := json.NewDecoder(createResponse.Body).Decode(&created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.Code != "article-1" || created.ShortURL != "http://short.test/article-1" {
		t.Fatalf("unexpected create response: %+v", created)
	}

	redirectRequest := httptest.NewRequest(http.MethodGet, "/article-1", nil)
	redirectResponse := httptest.NewRecorder()
	app.ServeHTTP(redirectResponse, redirectRequest)
	if redirectResponse.Code != http.StatusFound {
		t.Fatalf("redirect status = %d, want %d", redirectResponse.Code, http.StatusFound)
	}
	if location := redirectResponse.Header().Get("Location"); location != "https://example.com/articles/1" {
		t.Fatalf("redirect location = %q", location)
	}

	getRequest := httptest.NewRequest(http.MethodGet, "/api/v1/urls/article-1", nil)
	getResponse := httptest.NewRecorder()
	app.ServeHTTP(getResponse, getRequest)
	if getResponse.Code != http.StatusOK {
		t.Fatalf("get status = %d, want %d", getResponse.Code, http.StatusOK)
	}
	var fetched struct {
		Visits int64 `json:"visits"`
	}
	if err := json.NewDecoder(getResponse.Body).Decode(&fetched); err != nil {
		t.Fatalf("decode get response: %v", err)
	}
	if fetched.Visits != 1 {
		t.Fatalf("visits = %d, want 1", fetched.Visits)
	}

	deleteRequest := httptest.NewRequest(http.MethodDelete, "/api/v1/urls/article-1", nil)
	deleteResponse := httptest.NewRecorder()
	app.ServeHTTP(deleteResponse, deleteRequest)
	if deleteResponse.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want %d", deleteResponse.Code, http.StatusNoContent)
	}

	missingResponse := httptest.NewRecorder()
	app.ServeHTTP(missingResponse, httptest.NewRequest(http.MethodGet, "/article-1", nil))
	if missingResponse.Code != http.StatusNotFound {
		t.Fatalf("missing status = %d, want %d", missingResponse.Code, http.StatusNotFound)
	}
}

func TestReadyEndpoint(t *testing.T) {
	app, db := newTestApp(t)
	defer db.Close()

	request := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	response := httptest.NewRecorder()
	app.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("ready status = %d, want %d", response.Code, http.StatusOK)
	}
}

func TestHomePage(t *testing.T) {
	app, db := newTestApp(t)
	defer db.Close()

	response := httptest.NewRecorder()
	app.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("home status = %d, want %d", response.Code, http.StatusOK)
	}
	if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/html") {
		t.Fatalf("home Content-Type = %q, want text/html", contentType)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read home response: %v", err)
	}
	if !bytes.Contains(body, []byte("URL Shortener")) {
		t.Fatal("home page does not contain the application title")
	}
}

func TestFaviconDoesNotReturnNotFound(t *testing.T) {
	app, db := newTestApp(t)
	defer db.Close()

	response := httptest.NewRecorder()
	app.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/favicon.ico", nil))

	if response.Code != http.StatusNoContent {
		t.Fatalf("favicon status = %d, want %d", response.Code, http.StatusNoContent)
	}
}

func newTestApp(t *testing.T) (http.Handler, *sql.DB) {
	t.Helper()

	db, err := database.Open(":memory:")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	if err := database.Migrate(context.Background(), db, migrations.Files); err != nil {
		db.Close()
		t.Fatalf("migrate database: %v", err)
	}

	repo := repository.NewSQLite(db)
	linkService := service.NewLinkService(repo, shortener.Generator{}, 7)
	return handler.New(linkService, db, "http://short.test", 1<<20).Routes(), db
}
