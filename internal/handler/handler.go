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
  <title>SnapLink &mdash; Premium URL Shortener</title>
  <link rel="preconnect" href="https://fonts.googleapis.com">
  <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
  <link href="https://fonts.googleapis.com/css2?family=Outfit:wght@300;400;500;600;700;800&family=JetBrains+Mono:wght@400;500;600&display=swap" rel="stylesheet">
  <style>
    :root {
      --bg-primary: #030712;
      --card-bg: rgba(17, 24, 39, 0.75);
      --card-border: rgba(255, 255, 255, 0.08);
      --accent-purple: #a855f7;
      --accent-indigo: #6366f1;
      --accent-cyan: #06b6d4;
      --text-primary: #f8fafc;
      --text-secondary: #94a3b8;
      --text-muted: #64748b;
      --transition-smooth: all 0.3s cubic-bezier(0.4, 0, 0.2, 1);
    }

    * {
      box-sizing: border-box;
      margin: 0;
      padding: 0;
    }

    body {
      font-family: 'Outfit', sans-serif;
      background-color: var(--bg-primary);
      background-image: 
        radial-gradient(circle at 10% 20%, rgba(99, 102, 241, 0.12) 0%, transparent 45%),
        radial-gradient(circle at 90% 80%, rgba(168, 85, 247, 0.12) 0%, transparent 45%);
      color: var(--text-primary);
      min-height: 100vh;
      display: flex;
      flex-direction: column;
      align-items: center;
      justify-content: flex-start;
      padding: 60px 20px;
      overflow-y: auto;
    }

    main {
      width: 100%;
      max-width: 600px;
      margin-bottom: 24px;
    }

    header {
      text-align: center;
      margin-bottom: 36px;
    }

    header h1 {
      font-size: 3rem;
      font-weight: 800;
      letter-spacing: -0.05em;
      background: linear-gradient(135deg, var(--accent-purple), var(--accent-indigo), var(--accent-cyan));
      -webkit-background-clip: text;
      -webkit-text-fill-color: transparent;
      margin-bottom: 12px;
      display: flex;
      align-items: center;
      justify-content: center;
      gap: 10px;
    }

    header p {
      color: var(--text-secondary);
      font-size: 1.15rem;
      font-weight: 400;
    }

    .glass-card {
      background: var(--card-bg);
      backdrop-filter: blur(16px);
      -webkit-backdrop-filter: blur(16px);
      border: 1px solid var(--card-border);
      border-radius: 24px;
      padding: 36px;
      box-shadow: 0 25px 50px -12px rgba(0, 0, 0, 0.5);
    }

    .form-group {
      margin-bottom: 22px;
      display: flex;
      flex-direction: column;
      gap: 8px;
    }

    label {
      font-size: 0.8rem;
      font-weight: 600;
      color: var(--text-secondary);
      letter-spacing: 0.05em;
      text-transform: uppercase;
      display: flex;
      align-items: center;
      gap: 6px;
    }

    .input-wrapper {
      position: relative;
      display: flex;
      align-items: center;
    }

    .input-wrapper svg {
      position: absolute;
      left: 16px;
      width: 18px;
      height: 18px;
      color: var(--text-muted);
      pointer-events: none;
    }

    input {
      width: 100%;
      padding: 14px 16px 14px 46px;
      background: rgba(17, 24, 39, 0.6);
      border: 1px solid rgba(255, 255, 255, 0.08);
      border-radius: 14px;
      color: var(--text-primary);
      font-size: 1rem;
      font-family: inherit;
      transition: var(--transition-smooth);
    }

    input:focus {
      outline: none;
      border-color: var(--accent-indigo);
      box-shadow: 0 0 0 3px rgba(99, 102, 241, 0.25);
      background: rgba(17, 24, 39, 0.8);
    }

    .split-row {
      display: grid;
      grid-template-columns: 1.2fr 0.8fr;
      gap: 18px;
    }

    @media (max-width: 520px) {
      .split-row {
        grid-template-columns: 1fr;
      }
    }

    .pill-selector {
      display: flex;
      background: rgba(17, 24, 39, 0.6);
      border: 1px solid rgba(255, 255, 255, 0.08);
      border-radius: 14px;
      padding: 4px;
      width: 100%;
    }

    .pill-option {
      flex: 1;
      text-align: center;
      padding: 10px 6px;
      font-size: 0.85rem;
      font-weight: 600;
      color: var(--text-secondary);
      border-radius: 10px;
      cursor: pointer;
      transition: var(--transition-smooth);
      user-select: none;
    }

    .pill-option:hover {
      color: var(--text-primary);
      background: rgba(255, 255, 255, 0.04);
    }

    .pill-option.active {
      color: #ffffff;
      background: linear-gradient(135deg, var(--accent-purple), var(--accent-indigo));
      box-shadow: 0 4px 15px rgba(99, 102, 241, 0.3);
    }

    button.btn-primary {
      width: 100%;
      padding: 16px;
      margin-top: 12px;
      background: linear-gradient(135deg, var(--accent-purple), var(--accent-indigo));
      border: none;
      border-radius: 14px;
      color: #ffffff;
      font-size: 1.1rem;
      font-weight: 700;
      cursor: pointer;
      transition: var(--transition-smooth);
      box-shadow: 0 8px 20px rgba(99, 102, 241, 0.2);
      display: flex;
      align-items: center;
      justify-content: center;
      gap: 8px;
    }

    button.btn-primary:hover {
      transform: translateY(-2px);
      box-shadow: 0 15px 30px rgba(99, 102, 241, 0.35);
      background: linear-gradient(135deg, #b866ff, #7376ff);
    }

    button.btn-primary:active {
      transform: translateY(0);
    }

    .result-card {
      display: none;
      margin-top: 28px;
      background: rgba(99, 102, 241, 0.06);
      border: 1px dashed rgba(99, 102, 241, 0.3);
      border-radius: 16px;
      padding: 24px;
      animation: fadeIn 0.4s cubic-bezier(0.16, 1, 0.3, 1) forwards;
    }

    .result-card h3 {
      font-size: 1rem;
      font-weight: 700;
      margin-bottom: 14px;
      color: var(--accent-cyan);
      display: flex;
      align-items: center;
      gap: 6px;
    }

    .result-body {
      display: flex;
      align-items: center;
      justify-content: space-between;
      gap: 16px;
      background: rgba(3, 7, 18, 0.6);
      padding: 14px;
      border-radius: 10px;
      border: 1px solid rgba(255, 255, 255, 0.05);
    }

    .result-link {
      font-family: 'JetBrains Mono', monospace;
      color: var(--text-primary);
      font-size: 1rem;
      text-decoration: none;
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
      flex: 1;
    }

    .result-link:hover {
      color: var(--accent-cyan);
      text-decoration: underline;
    }

    .copy-btn {
      background: rgba(255, 255, 255, 0.05);
      border: 1px solid rgba(255, 255, 255, 0.08);
      padding: 8px 14px;
      border-radius: 8px;
      color: var(--text-primary);
      cursor: pointer;
      font-size: 0.8rem;
      font-weight: 600;
      display: flex;
      align-items: center;
      gap: 6px;
      transition: var(--transition-smooth);
      white-space: nowrap;
    }

    .copy-btn:hover {
      background: rgba(255, 255, 255, 0.12);
    }

    .copy-btn.copied {
      background: rgba(16, 185, 129, 0.15);
      border-color: rgba(16, 185, 129, 0.3);
      color: #10b981;
    }

    .history-section {
      width: 100%;
      max-width: 600px;
      margin-top: 36px;
      display: none;
    }

    .history-section h2 {
      font-size: 1.2rem;
      font-weight: 700;
      margin-bottom: 18px;
      color: var(--text-secondary);
      display: flex;
      align-items: center;
      gap: 8px;
    }

    .history-list {
      display: flex;
      flex-direction: column;
      gap: 14px;
    }

    .history-item {
      background: var(--card-bg);
      border: 1px solid var(--card-border);
      border-radius: 16px;
      padding: 20px;
      display: flex;
      flex-direction: column;
      gap: 12px;
      transition: var(--transition-smooth);
      animation: slideIn 0.4s cubic-bezier(0.16, 1, 0.3, 1) forwards;
    }

    .history-item:hover {
      border-color: rgba(99, 102, 241, 0.25);
      transform: translateY(-2px);
      box-shadow: 0 10px 20px -10px rgba(0, 0, 0, 0.3);
    }

    .history-header {
      display: flex;
      justify-content: space-between;
      align-items: flex-start;
      gap: 16px;
    }

    .history-urls {
      flex: 1;
      overflow: hidden;
    }

    .short-url-row {
      display: flex;
      align-items: center;
      gap: 8px;
      margin-bottom: 6px;
    }

    .short-url-row a {
      font-family: 'JetBrains Mono', monospace;
      color: var(--accent-cyan);
      text-decoration: none;
      font-weight: 600;
      font-size: 1.05rem;
    }

    .short-url-row a:hover {
      text-decoration: underline;
    }

    .original-url {
      font-size: 0.85rem;
      color: var(--text-secondary);
      overflow: hidden;
      text-overflow: ellipsis;
      white-space: nowrap;
    }

    .history-actions {
      display: flex;
      gap: 8px;
      align-items: center;
    }

    .history-footer {
      display: flex;
      align-items: center;
      justify-content: space-between;
      font-size: 0.8rem;
      color: var(--text-muted);
      border-top: 1px solid rgba(255, 255, 255, 0.05);
      padding-top: 12px;
    }

    .footer-badges {
      display: flex;
      gap: 8px;
    }

    .badge {
      padding: 3px 8px;
      border-radius: 6px;
      font-size: 0.75rem;
      font-weight: 600;
    }

    .badge-visits {
      background: rgba(99, 102, 241, 0.12);
      color: #818cf8;
      border: 1px solid rgba(99, 102, 241, 0.15);
    }

    .badge-active {
      background: rgba(16, 185, 129, 0.1);
      color: #34d399;
      border: 1px solid rgba(16, 185, 129, 0.15);
    }

    .badge-expired {
      background: rgba(239, 68, 68, 0.1);
      color: #f87171;
      border: 1px solid rgba(239, 68, 68, 0.15);
    }

    .delete-btn {
      background: transparent;
      border: none;
      color: var(--text-muted);
      cursor: pointer;
      padding: 6px;
      border-radius: 8px;
      transition: var(--transition-smooth);
      display: flex;
      align-items: center;
      justify-content: center;
    }

    .delete-btn:hover {
      color: #f87171;
      background: rgba(239, 68, 68, 0.08);
    }

    @keyframes fadeIn {
      from { opacity: 0; transform: translateY(8px); }
      to { opacity: 1; transform: translateY(0); }
    }

    @keyframes slideIn {
      from { opacity: 0; transform: translateY(12px); }
      to { opacity: 1; transform: translateY(0); }
    }
  </style>
</head>
<body>
  <main>
    <header>
      <h1>
        <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" stroke-width="2.5" stroke="currentColor" style="width:34px;height:34px;color:#a855f7;">
          <path stroke-linecap="round" stroke-linejoin="round" d="M13.19 8.688a4.5 4.5 0 011.242 7.244l-4.5 4.5a4.5 4.5 0 01-6.364-6.364l1.757-1.757m13.35-.622l1.757-1.757a4.5 4.5 0 00-6.364-6.364l-4.5 4.5a4.5 4.5 0 001.242 7.244" />
        </svg>
        SnapLink
      </h1>
      <p>Shorten, manage, and track your links with ease.</p>
    </header>

    <div class="glass-card">
      <form id="shorten-form">
        <div class="form-group">
          <label for="url">
            Destination URL
          </label>
          <div class="input-wrapper">
            <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" stroke-width="2" stroke="currentColor">
              <path stroke-linecap="round" stroke-linejoin="round" d="M12 21a9.004 9.004 0 008.716-6.747M12 21a9.004 9.004 0 01-8.716-6.747M12 21c2.485 0 4.5-4.03 4.5-9S14.485 3 12 3m0 18c-2.485 0-4.5-4.03-4.5-9S9.515 3 12 3m0 0a8.997 8.997 0 017.843 4.582M12 3a8.997 8.997 0 00-7.843 4.582m15.686 0A11.953 11.953 0 0112 10.5c-2.998 0-5.74-1.1-7.843-2.918m15.686 0A8.959 8.959 0 0121 12c0 .778-.099 1.533-.284 2.253m0 0A17.919 17.919 0 0112 16.5c-3.162 0-6.133-.815-8.716-2.247m0 0A9.015 9.015 0 013 12c0-1.605.42-3.113 1.157-4.418" />
            </svg>
            <input id="url" type="url" placeholder="https://example.com/some/long/destination/path" required>
          </div>
        </div>

        <div class="split-row">
          <div class="form-group">
            <label for="alias">
              Custom Alias <span style="text-transform:none;color:var(--text-muted);font-weight:400;margin-left:4px;">(optional)</span>
            </label>
            <div class="input-wrapper">
              <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" stroke-width="2" stroke="currentColor">
                <path stroke-linecap="round" stroke-linejoin="round" d="M9.528 17.116A1.5 1.5 0 1111 15.5h.01a1.5 1.5 0 111.472 1.616l-1.472.01-.01-1.626zm0 0l-.01 2.25m3.73-3.73l-.01-2.25m-3.72-3.72a4.5 4.5 0 117.47 0" />
              </svg>
              <input id="alias" pattern="[A-Za-z0-9_-]{4,32}" placeholder="e.g. my-shortcut">
            </div>
          </div>

          <div class="form-group">
            <label>Link Expiry</label>
            <div class="pill-selector">
              <div class="pill-option active" data-value="0">Never</div>
              <div class="pill-option" data-value="3600">1 Hour</div>
              <div class="pill-option" data-value="86400">1 Day</div>
              <div class="pill-option" data-value="604800">7 Days</div>
            </div>
          </div>
        </div>

        <button type="submit" class="btn-primary">
          <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" stroke-width="2.5" stroke="currentColor" style="width:18px;height:18px;">
            <path stroke-linecap="round" stroke-linejoin="round" d="M9.813 15.904L9 21l8.904-4.452L21 9l-4.5-4.5L9.813 15.904z" stroke-linejoin="round" />
            <path stroke-linecap="round" stroke-linejoin="round" d="M9 21l3-3m-3 3l-3-3" />
          </svg>
          Shorten URL
        </button>
      </form>

      <div id="result" class="result-card"></div>
    </div>
  </main>

  <section id="history-section" class="history-section">
    <h2>
      <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" stroke-width="2.2" stroke="currentColor" style="width:20px;height:20px;color:var(--accent-indigo);">
        <path stroke-linecap="round" stroke-linejoin="round" d="M3.75 6A2.25 2.25 0 016 3.75h2.25A2.25 2.25 0 0110.5 6v2.25a2.25 2.25 0 01-2.25 2.25H6a2.25 2.25 0 01-2.25-2.25V6zM3.75 15.75A2.25 2.25 0 016 13.5h2.25a2.25 2.25 0 012.25 2.25V18a2.25 2.25 0 01-2.25 2.25H6A2.25 2.25 0 013.75 18v-2.25zM13.5 6a2.25 2.25 0 012.25-2.25H18A2.25 2.25 0 0120.25 6v2.25A2.25 2.25 0 0118 10.5h-2.25a2.25 2.25 0 01-2.25-2.25V6zM13.5 15.75a2.25 2.25 0 012.25-2.25H18a2.25 2.25 0 012.25 2.25V18A2.25 2.25 0 0118 20.25h-2.25A2.25 2.25 0 0113.5 18v-2.25z" />
      </svg>
      Your Recent Links
    </h2>
    <div id="history-list" class="history-list"></div>
  </section>

  <script>
    document.addEventListener("DOMContentLoaded", () => {
      const form = document.querySelector("#shorten-form");
      const resultCard = document.querySelector("#result");
      const historyList = document.querySelector("#history-list");
      const historySection = document.querySelector("#history-section");
      const pillOptions = document.querySelectorAll(".pill-option");

      let selectedExpiresIn = 0;

      pillOptions.forEach(pill => {
        pill.addEventListener("click", () => {
          pillOptions.forEach(p => p.classList.remove("active"));
          pill.classList.add("active");
          selectedExpiresIn = parseInt(pill.dataset.value);
        });
      });

      function getHistory() {
        return JSON.parse(localStorage.getItem("snaplink_history") || "[]");
      }

      function saveHistory(history) {
        localStorage.setItem("snaplink_history", JSON.stringify(history));
      }

      function addToHistory(link) {
        let history = getHistory();
        history = history.filter(item => item.code !== link.code);
        history.unshift({
          code: link.code,
          short_url: link.short_url,
          original_url: link.original_url,
          created_at: link.created_at,
          expires_at: link.expires_at,
          visits: link.visits || 0
        });
        if (history.length > 15) history.pop();
        saveHistory(history);
        renderHistory();
        refreshHistoryStats();
      }

      function deleteHistoryItem(code) {
        let history = getHistory();
        history = history.filter(item => item.code !== code);
        saveHistory(history);
        renderHistory();
      }

      window.copyText = (text, elementId) => {
        navigator.clipboard.writeText(text).then(() => {
          const btn = document.getElementById(elementId);
          if (!btn) return;
          const originalHTML = btn.innerHTML;
          btn.classList.add("copied");
          btn.innerHTML = '<svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" stroke-width="2.5" stroke="currentColor" style="width:14px;height:14px;"><path stroke-linecap="round" stroke-linejoin="round" d="M4.5 12.75l6 6 9-13.5" /></svg> Copied!';
          setTimeout(() => {
            btn.classList.remove("copied");
            btn.innerHTML = originalHTML;
          }, 2000);
        });
      };

      function formatRelativeTime(dateStr) {
        if (!dateStr) return "Never";
        const date = new Date(dateStr);
        const now = new Date();
        if (date < now) return "Expired";
        
        const diffMs = date - now;
        const diffMins = Math.round(diffMs / 60000);
        const diffHours = Math.round(diffMs / 3600000);
        const diffDays = Math.round(diffMs / 86400000);

        if (diffMins < 60) return "in " + diffMins + "m";
        if (diffHours < 24) return "in " + diffHours + "h";
        return "in " + diffDays + "d";
      }

      function renderHistory() {
        const history = getHistory();
        if (history.length === 0) {
          historySection.style.display = "none";
          return;
        }
        historySection.style.display = "block";
        historyList.innerHTML = "";

        history.forEach((item, index) => {
          const isExpired = item.expires_at && new Date(item.expires_at) < new Date();
          const statusText = isExpired ? "Expired" : formatRelativeTime(item.expires_at);
          const badgeClass = isExpired ? "badge-expired" : "badge-active";
          const btnId = "history-copy-" + index;

          const card = document.createElement("div");
          card.className = "history-item";
          card.innerHTML = '\n' +
'            <div class="history-header">\n' +
'              <div class="history-urls">\n' +
'                <div class="short-url-row">\n' +
'                  <a href="' + item.short_url + '" target="_blank" rel="noopener">' + item.short_url + '</a>\n' +
'                </div>\n' +
'                <div class="original-url" title="' + item.original_url + '">' + item.original_url + '</div>\n' +
'              </div>\n' +
'              <div class="history-actions">\n' +
'                <button id="' + btnId + '" class="copy-btn" onclick="copyText(\'' + item.short_url + '\', \'' + btnId + '\')">\n' +
'                  <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" stroke-width="2" stroke="currentColor" style="width:14px;height:14px;">\n' +
'                    <path stroke-linecap="round" stroke-linejoin="round" d="M15.75 17.25v3.375c0 .621-.504 1.125-1.125 1.125h-9.75a1.125 1.125 0 01-1.125-1.125V7.875c0-.621.504-1.125 1.125-1.125H5.25m9.9 9.9l3.89-3.89a.75.75 0 000-1.06l-3.89-3.89m3.89 3.89H11.25M9 11.25h.008v.008H9v-.008z" />\n' +
'                  </svg> Copy\n' +
'                </button>\n' +
'                <button class="delete-btn" title="Remove from History" data-code="' + item.code + '">\n' +
'                  <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" stroke-width="2" stroke="currentColor" style="width:16px;height:16px;">\n' +
'                    <path stroke-linecap="round" stroke-linejoin="round" d="M14.74 9l-.346 9m-4.788 0L9.26 9m9.968-3.21c.342.052.682.107 1.022.166m-1.022-.165L18.16 19.673a2.25 2.25 0 01-2.244 2.077H8.084a2.25 2.25 0 01-2.244-2.077L4.772 5.79m14.456 0a48.108 48.108 0 00-3.478-.397m-12 .562c.34-.059.68-.114 1.022-.165m0 0a48.11 48.11 0 013.478-.397m7.5 0v-.916c0-1.18-.91-2.164-2.09-2.201a51.964 51.964 0 00-3.32 0c-1.18.037-2.09 1.022-2.09 2.201v.916m7.5 0a48.667 48.667 0 00-7.5 0" />\n' +
'                  </svg>\n' +
'                </button>\n' +
'              </div>\n' +
'            </div>\n' +
'            <div class="history-footer">\n' +
'              <span>Created: ' + new Date(item.created_at).toLocaleDateString() + '</span>\n' +
'              <div class="footer-badges">\n' +
'                <span class="badge ' + badgeClass + '">' + (isExpired ? "Expired" : "Expires: " + statusText) + '</span>\n' +
'                <span class="badge badge-visits">' + item.visits + ' visits</span>\n' +
'              </div>\n' +
'            </div>\n';
          historyList.appendChild(card);
        });

        document.querySelectorAll(".delete-btn").forEach(btn => {
          btn.addEventListener("click", () => {
            deleteHistoryItem(btn.dataset.code);
          });
        });
      }

      async function refreshHistoryStats() {
        const history = getHistory();
        if (history.length === 0) return;

        let updated = false;
        const promises = history.map(async (item) => {
          try {
            const res = await fetch("/api/v1/urls/" + item.code);
            if (res.ok) {
              const data = await res.json();
              if (item.visits !== data.visits || item.expires_at !== data.expires_at) {
                item.visits = data.visits;
                item.expires_at = data.expires_at;
                updated = true;
              }
            }
          } catch (e) {
            console.error("error fetching stats for " + item.code, e);
          }
        });

        await Promise.all(promises);
        if (updated) {
          saveHistory(history);
          renderHistory();
        }
      }

      renderHistory();
      refreshHistoryStats();

      form.addEventListener("submit", async (event) => {
        event.preventDefault();
        resultCard.style.display = "block";
        resultCard.innerHTML = '<h3 style="color:var(--text-secondary)">Shortening your link...</h3>';

        const payload = { url: document.querySelector("#url").value };
        const alias = document.querySelector("#alias").value.trim();
        if (alias) payload.custom_alias = alias;
        if (selectedExpiresIn > 0) payload.expires_in_seconds = selectedExpiresIn;

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

          resultCard.innerHTML = 
            '<h3>Link Created Successfully!</h3>\n' +
            '<div class="result-body">\n' +
            '  <a id="result-link" class="result-link" href="' + data.short_url + '" target="_blank" rel="noopener">' + data.short_url + '</a>\n' +
            '  <button id="main-copy-btn" class="copy-btn" onclick="copyText(\'' + data.short_url + '\', \'main-copy-btn\')">\n' +
            '    <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" stroke-width="2" stroke="currentColor" style="width:14px;height:14px;">\n' +
            '      <path stroke-linecap="round" stroke-linejoin="round" d="M15.75 17.25v3.375c0 .621-.504 1.125-1.125 1.125h-9.75a1.125 1.125 0 01-1.125-1.125V7.875c0-.621.504-1.125 1.125-1.125H5.25m9.9 9.9l3.89-3.89a.75.75 0 000-1.06l-3.89-3.89m3.89 3.89H11.25M9 11.25h.008v.008H9v-.008z" />\n' +
            '    </svg> Copy Link\n' +
            '  </button>\n' +
            '</div>';

          addToHistory(data);
          form.reset();
          
          pillOptions.forEach(p => p.classList.remove("active"));
          document.querySelector('[data-value="0"]').classList.add("active");
          selectedExpiresIn = 0;

        } catch (error) {
          resultCard.innerHTML = 
            '<div style="color: #fca5a5; font-weight:600; margin-bottom:8px;">Error: ' + error.message + '</div>';
          if (error.shortURL) {
            resultCard.innerHTML += 
              '<div class="result-body">\n' +
              '  <span style="font-size:0.85rem;color:var(--text-secondary)">Existing URL:</span>\n' +
              '  <a class="result-link" href="' + error.shortURL + '" target="_blank" rel="noopener">' + error.shortURL + '</a>\n' +
              '  <button id="err-copy-btn" class="copy-btn" onclick="copyText(\'' + error.shortURL + '\', \'err-copy-btn\')">\n' +
              '    <svg xmlns="http://www.w3.org/2000/svg" fill="none" viewBox="0 0 24 24" stroke-width="2" stroke="currentColor" style="width:14px;height:14px;">\n' +
              '      <path stroke-linecap="round" stroke-linejoin="round" d="M15.75 17.25v3.375c0 .621-.504 1.125-1.125 1.125h-9.75a1.125 1.125 0 01-1.125-1.125V7.875c0-.621.504-1.125 1.125-1.125H5.25m9.9 9.9l3.89-3.89a.75.75 0 000-1.06l-3.89-3.89m3.89 3.89H11.25M9 11.25h.008v.008H9v-.008z" />\n' +
              '    </svg> Copy\n' +
              '  </button>\n' +
              '</div>';
          }
        }
      });
    });
  </script>
</body>
</html>`
