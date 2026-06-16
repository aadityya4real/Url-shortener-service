package handler

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/aadityya4real/Url-shortener-service/internal/model"
	"github.com/aadityya4real/Url-shortener-service/internal/repository"
	"github.com/aadityya4real/Url-shortener-service/internal/service"
	"github.com/aadityya4real/Url-shortener-service/pkg/response"
)

type Handler struct {
	service      *service.LinkService
	db           *sql.DB
	baseURL      string
	maxBodyBytes int64
}

type createRequest struct {
	URL              string `json:"url"`
	CustomAlias      string `json:"custom_alias,omitempty"`
	ExpiresInSeconds int64  `json:"expires_in_seconds,omitempty"`
}

type linkResponse struct {
	ID          int64      `json:"id"`
	Code        string     `json:"code"`
	ShortURL    string     `json:"short_url"`
	OriginalURL string     `json:"original_url"`
	CreatedAt   time.Time  `json:"created_at"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	Visits      int64      `json:"visits"`
}

type conflictResponse struct {
	Error    string `json:"error"`
	ShortURL string `json:"short_url"`
}

func New(service *service.LinkService, db *sql.DB, baseURL string, maxBodyBytes int64) *Handler {
	return &Handler{
		service:      service,
		db:           db,
		baseURL:      strings.TrimRight(baseURL, "/"),
		maxBodyBytes: maxBodyBytes,
	}
}

func (h *Handler) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /", h.home)
	mux.HandleFunc("GET /favicon.ico", h.favicon)
	mux.HandleFunc("GET /healthz", h.health)
	mux.HandleFunc("GET /readyz", h.ready)
	mux.HandleFunc("POST /api/v1/urls", h.create)
	mux.HandleFunc("GET /api/v1/urls/{code}", h.get)
	mux.HandleFunc("DELETE /api/v1/urls/{code}", h.delete)
	mux.HandleFunc("GET /{code}", h.redirect)
	return mux
}

func (h *Handler) home(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		response.Error(w, http.StatusNotFound, "page not found")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, homePage)
}

func (h *Handler) favicon(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) health(w http.ResponseWriter, _ *http.Request) {
	response.JSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (h *Handler) ready(w http.ResponseWriter, r *http.Request) {
	if err := h.db.PingContext(r.Context()); err != nil {
		response.Error(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	response.JSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
	contentType := r.Header.Get("Content-Type")
	if !strings.HasPrefix(strings.ToLower(contentType), "application/json") {
		response.Error(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, h.maxBodyBytes)
	defer r.Body.Close()

	var request createRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		response.Error(w, http.StatusBadRequest, decodeError(err))
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		response.Error(w, http.StatusBadRequest, "request body must contain one JSON object")
		return
	}

	if request.ExpiresInSeconds < 0 {
		response.Error(w, http.StatusBadRequest, "expires_in_seconds cannot be negative")
		return
	}
	if request.ExpiresInSeconds > 9223372036 {
		response.Error(w, http.StatusBadRequest, "expires_in_seconds is too large")
		return
	}

	link, err := h.service.Create(r.Context(), service.CreateInput{
		OriginalURL: request.URL,
		CustomAlias: request.CustomAlias,
		ExpiresIn:   time.Duration(request.ExpiresInSeconds) * time.Second,
	})
	if err != nil {
		if errors.Is(err, repository.ErrConflict) && request.CustomAlias != "" {
			response.JSON(w, http.StatusConflict, conflictResponse{
				Error:    "short code already exists",
				ShortURL: h.baseURL + "/" + request.CustomAlias,
			})
			return
		}
		h.writeServiceError(w, err)
		return
	}

	response.JSON(w, http.StatusCreated, h.toResponse(link))
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	link, err := h.service.Get(r.Context(), r.PathValue("code"))
	if err != nil {
		h.writeServiceError(w, err)
		return
	}
	response.JSON(w, http.StatusOK, h.toResponse(link))
}

func (h *Handler) redirect(w http.ResponseWriter, r *http.Request) {
	link, err := h.service.Resolve(r.Context(), r.PathValue("code"))
	if err != nil {
		h.writeServiceError(w, err)
		return
	}
	http.Redirect(w, r, link.OriginalURL, http.StatusFound)
}

func (h *Handler) delete(w http.ResponseWriter, r *http.Request) {
	if err := h.service.Delete(r.Context(), r.PathValue("code")); err != nil {
		h.writeServiceError(w, err)
		return
	}
	response.JSON(w, http.StatusNoContent, nil)
}

func (h *Handler) toResponse(link *model.Link) linkResponse {
	return linkResponse{
		ID:          link.ID,
		Code:        link.Code,
		ShortURL:    h.baseURL + "/" + link.Code,
		OriginalURL: link.OriginalURL,
		CreatedAt:   link.CreatedAt,
		ExpiresAt:   link.ExpiresAt,
		Visits:      link.Visits,
	}
}

func (h *Handler) writeServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, service.ErrInvalidInput):
		response.Error(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, repository.ErrConflict):
		response.Error(w, http.StatusConflict, "short code already exists")
	case errors.Is(err, repository.ErrNotFound), errors.Is(err, service.ErrExpired):
		response.Error(w, http.StatusNotFound, "short link not found")
	default:
		slog.Error("request failed", "error", err)
		response.Error(w, http.StatusInternalServerError, "internal server error")
	}
}

func decodeError(err error) string {
	var maxBytesError *http.MaxBytesError
	switch {
	case errors.As(err, &maxBytesError):
		return "request body is too large"
	case errors.Is(err, io.EOF):
		return "request body is required"
	default:
		return fmt.Sprintf("invalid JSON body: %v", err)
	}
}

const homePage = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>URL Shortener</title>
  <style>
    :root { color-scheme: light dark; font-family: system-ui, sans-serif; }
    body { margin: 0; min-height: 100vh; display: grid; place-items: center; background: #101827; }
    main { width: min(560px, calc(100% - 32px)); padding: 32px; border-radius: 16px; background: #1f2937; box-shadow: 0 20px 50px #0005; }
    h1 { margin-top: 0; }
    p { color: #cbd5e1; }
    label { display: block; margin: 18px 0 6px; font-weight: 600; }
    input, button { box-sizing: border-box; width: 100%; padding: 12px; border: 1px solid #475569; border-radius: 8px; font: inherit; }
    input { background: #111827; }
    button { margin-top: 20px; border: 0; background: #2563eb; color: white; font-weight: 700; cursor: pointer; }
    button:hover { background: #1d4ed8; }
    #result { display: none; margin-top: 20px; padding: 14px; border-radius: 8px; background: #111827; overflow-wrap: anywhere; }
    #error { color: #fca5a5; }
    a { color: #60a5fa; }
  </style>
</head>
<body>
  <main>
    <h1>URL Shortener</h1>
    <p>Create a compact link backed by the Go Service.</p>
    <form id="shorten-form">
      <label for="url">Long URL</label>
      <input id="url" type="url" placeholder="https://example.com/long/path" required>
      <label for="alias">Custom alias (optional)</label>
      <input id="alias" pattern="[A-Za-z0-9_-]{4,32}" placeholder="my-link">
      <button type="submit">Shorten URL</button>
    </form>
    <div id="result"></div>
  </main>
  <script>
    const form = document.querySelector("#shorten-form");
    const result = document.querySelector("#result");
    form.addEventListener("submit", async (event) => {
      event.preventDefault();
      result.style.display = "block";
      result.textContent = "Creating link...";
      const payload = { url: document.querySelector("#url").value };
      const alias = document.querySelector("#alias").value.trim();
      if (alias) payload.custom_alias = alias;
      try {
        const response = await fetch("/api/v1/urls", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(payload)
        });
        const data = await response.json();
        if (!response.ok) {
          const error = new Error(data.error || "Unable to create link");
          error.shortURL = data.short_url;
          throw error;
        }
        result.innerHTML = "Short URL: <a href=\"" + data.short_url + "\" target=\"_blank\" rel=\"noopener\"></a>";
        result.querySelector("a").textContent = data.short_url;
      } catch (error) {
        result.innerHTML = "";
        const message = document.createElement("span");
        message.id = "error";
        message.textContent = error.message;
        result.appendChild(message);
        if (error.shortURL) {
          const linkLine = document.createElement("div");
          linkLine.textContent = "Existing link: ";
          const link = document.createElement("a");
          link.href = error.shortURL;
          link.target = "_blank";
          link.rel = "noopener";
          link.textContent = error.shortURL;
          linkLine.appendChild(link);
          result.appendChild(linkLine);
        }
      }
    });
  </script>
</body>
</html>`
