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
	userService  *service.UserService
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

type authRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func New(service *service.LinkService, userService *service.UserService, db *sql.DB, baseURL string, maxBodyBytes int64) *Handler {
	return &Handler{
		service:      service,
		userService:  userService,
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

	// Auth Endpoints
	mux.HandleFunc("POST /api/v1/signup", h.signUp)
	mux.HandleFunc("POST /api/v1/login", h.logIn)
	mux.HandleFunc("POST /api/v1/logout", h.logOut)
	mux.HandleFunc("GET /api/v1/me", h.me)
	mux.HandleFunc("GET /api/v1/user/urls", h.userURLs)

	return mux
}

func (h *Handler) home(w http.ResponseWriter, _ *http.Request) {
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

func (h *Handler) getSessionUser(r *http.Request) (*model.User, error) {
	cookie, err := r.Cookie("session_id")
	if err != nil {
		return nil, err
	}
	return h.userService.Authenticate(r.Context(), cookie.Value)
}

func (h *Handler) signUp(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBodyBytes)
	defer r.Body.Close()

	var req authRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid request payload")
		return
	}

	user, err := h.userService.SignUp(r.Context(), req.Email, req.Password)
	if err != nil {
		if errors.Is(err, service.ErrUserAlreadyExists) {
			response.Error(w, http.StatusConflict, err.Error())
			return
		}
		if errors.Is(err, service.ErrInvalidInput) {
			response.Error(w, http.StatusBadRequest, err.Error())
			return
		}
		slog.Error("signup failed", "error", err)
		response.Error(w, http.StatusInternalServerError, "internal server error")
		return
	}

	response.JSON(w, http.StatusCreated, map[string]any{
		"id":    user.ID,
		"email": user.Email,
	})
}

func (h *Handler) logIn(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, h.maxBodyBytes)
	defer r.Body.Close()

	var req authRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		response.Error(w, http.StatusBadRequest, "invalid request payload")
		return
	}

	session, err := h.userService.LogIn(r.Context(), req.Email, req.Password)
	if err != nil {
		if errors.Is(err, service.ErrInvalidCredentials) {
			response.Error(w, http.StatusUnauthorized, err.Error())
			return
		}
		slog.Error("login failed", "error", err)
		response.Error(w, http.StatusInternalServerError, "internal server error")
		return
	}

	isHTTPS := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
	http.SetCookie(w, &http.Cookie{
		Name:     "session_id",
		Value:    session.ID,
		Path:     "/",
		Expires:  session.ExpiresAt,
		HttpOnly: true,
		Secure:   isHTTPS,
		SameSite: http.SameSiteLaxMode,
	})

	response.JSON(w, http.StatusOK, map[string]string{"status": "authenticated"})
}

func (h *Handler) logOut(w http.ResponseWriter, r *http.Request) {
	cookie, err := r.Cookie("session_id")
	if err == nil {
		_ = h.userService.LogOut(r.Context(), cookie.Value)
	}

	isHTTPS := r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https"
	http.SetCookie(w, &http.Cookie{
		Name:     "session_id",
		Value:    "",
		Path:     "/",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   isHTTPS,
		SameSite: http.SameSiteLaxMode,
	})

	response.JSON(w, http.StatusOK, map[string]string{"status": "logged_out"})
}

func (h *Handler) me(w http.ResponseWriter, r *http.Request) {
	user, err := h.getSessionUser(r)
	if err != nil {
		response.JSON(w, http.StatusOK, map[string]any{"authenticated": false})
		return
	}

	response.JSON(w, http.StatusOK, map[string]any{
		"authenticated": true,
		"id":            user.ID,
		"email":         user.Email,
	})
}

func (h *Handler) userURLs(w http.ResponseWriter, r *http.Request) {
	user, err := h.getSessionUser(r)
	if err != nil {
		response.Error(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	links, err := h.service.GetByUserID(r.Context(), user.ID)
	if err != nil {
		slog.Error("failed to get user urls", "error", err)
		response.Error(w, http.StatusInternalServerError, "internal server error")
		return
	}

	res := make([]linkResponse, len(links))
	for i, l := range links {
		res[i] = h.toResponse(&l)
	}

	response.JSON(w, http.StatusOK, res)
}

func (h *Handler) create(w http.ResponseWriter, r *http.Request) {
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

	var userID *int64
	user, err := h.getSessionUser(r)
	if err == nil {
		userID = &user.ID
	}

	link, err := h.service.Create(r.Context(), service.CreateInput{
		OriginalURL: request.URL,
		CustomAlias: request.CustomAlias,
		ExpiresIn:   time.Duration(request.ExpiresInSeconds) * time.Second,
		UserID:      userID,
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
	var userID *int64
	user, err := h.getSessionUser(r)
	if err == nil {
		userID = &user.ID
	}

	if err := h.service.Delete(r.Context(), r.PathValue("code"), userID); err != nil {
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
	case errors.Is(err, service.ErrUnauthorizedLink):
		response.Error(w, http.StatusForbidden, err.Error())
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
  <title>ZipLink | Modern URL Shortener</title>
  <link rel="preconnect" href="https://fonts.googleapis.com">
  <link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
  <link href="https://fonts.googleapis.com/css2?family=Outfit:wght@300;400;600;700&display=swap" rel="stylesheet">
  <style>
    :root {
      --bg-gradient-start: #0f172a;
      --bg-gradient-end: #020617;
      --primary-color: #f43f5e;
      --secondary-color: #6366f1;
      --text-main: #f8fafc;
      --text-muted: #94a3b8;
      --glass-bg: rgba(30, 41, 59, 0.45);
      --glass-border: rgba(255, 255, 255, 0.08);
      --input-bg: rgba(15, 23, 42, 0.7);
    }

    * {
      box-sizing: border-box;
      transition: all 0.25s cubic-bezier(0.4, 0, 0.2, 1);
    }

    body {
      margin: 0;
      min-height: 100vh;
      display: flex;
      flex-direction: column;
      font-family: "Outfit", sans-serif;
      background: linear-gradient(135deg, var(--bg-gradient-start), var(--bg-gradient-end));
      color: var(--text-main);
      overflow-x: hidden;
      position: relative;
    }

    /* Glow blobs */
    .blob {
      position: absolute;
      width: 400px;
      height: 400px;
      border-radius: 50%;
      background: radial-gradient(circle, var(--primary-color) 0%, transparent 70%);
      opacity: 0.15;
      filter: blur(80px);
      z-index: 0;
      pointer-events: none;
    }
    .blob-1 { top: -100px; left: -100px; }
    .blob-2 { bottom: -150px; right: -100px; background: radial-gradient(circle, var(--secondary-color) 0%, transparent 70%); }

    /* Container */
    .app-container {
      width: min(1000px, calc(100% - 32px));
      margin: 40px auto;
      z-index: 10;
      display: flex;
      flex-direction: column;
      gap: 30px;
      flex-grow: 1;
    }

    /* Header Nav */
    header {
      display: flex;
      justify-content: space-between;
      align-items: center;
      padding: 16px 24px;
      background: var(--glass-bg);
      backdrop-filter: blur(12px);
      border: 1px solid var(--glass-border);
      border-radius: 16px;
      box-shadow: 0 8px 32px 0 rgba(0, 0, 0, 0.3);
    }

    .brand {
      display: flex;
      align-items: center;
      gap: 10px;
      font-size: 24px;
      font-weight: 700;
      background: linear-gradient(to right, var(--primary-color), var(--secondary-color));
      -webkit-background-clip: text;
      -webkit-text-fill-color: transparent;
      letter-spacing: -0.5px;
    }

    .user-badge {
      display: flex;
      align-items: center;
      gap: 12px;
      font-size: 14px;
      color: var(--text-muted);
    }

    .btn {
      padding: 10px 20px;
      border-radius: 10px;
      font-weight: 600;
      font-size: 14px;
      cursor: pointer;
      border: none;
      display: flex;
      align-items: center;
      justify-content: center;
      gap: 8px;
    }

    .btn-primary {
      background: linear-gradient(135deg, var(--primary-color), #e11d48);
      color: white;
      box-shadow: 0 4px 14px rgba(244, 63, 94, 0.4);
    }
    .btn-primary:hover {
      transform: translateY(-2px);
      box-shadow: 0 6px 20px rgba(244, 63, 94, 0.6);
    }

    .btn-secondary {
      background: rgba(255, 255, 255, 0.08);
      color: var(--text-main);
      border: 1px solid var(--glass-border);
    }
    .btn-secondary:hover {
      background: rgba(255, 255, 255, 0.15);
      transform: translateY(-2px);
    }

    .btn-danger {
      background: rgba(239, 68, 68, 0.2);
      color: #fca5a5;
      border: 1px solid rgba(239, 68, 68, 0.3);
    }
    .btn-danger:hover {
      background: rgba(239, 68, 68, 0.4);
      transform: translateY(-2px);
    }

    /* Cards */
    .glass-card {
      background: var(--glass-bg);
      backdrop-filter: blur(12px);
      border: 1px solid var(--glass-border);
      border-radius: 20px;
      padding: 30px;
      box-shadow: 0 8px 32px 0 rgba(0, 0, 0, 0.3);
    }

    /* Welcome/Guest View */
    .welcome-panel {
      text-align: center;
      padding: 60px 40px;
      display: flex;
      flex-direction: column;
      align-items: center;
      gap: 24px;
    }

    .welcome-panel h1 {
      font-size: 40px;
      margin: 0;
      font-weight: 700;
      letter-spacing: -1px;
    }

    .welcome-panel p {
      font-size: 18px;
      color: var(--text-muted);
      max-width: 600px;
      margin: 0;
    }

    .welcome-buttons {
      display: flex;
      gap: 16px;
      margin-top: 10px;
    }

    /* Auth modal/card */
    .auth-container {
      max-width: 450px;
      margin: 40px auto;
      display: none;
    }

    .auth-container h2 {
      margin-top: 0;
      font-size: 26px;
      text-align: center;
    }

    .form-group {
      margin-bottom: 20px;
    }

    .form-group label {
      display: block;
      margin-bottom: 8px;
      font-weight: 600;
      font-size: 14px;
      color: var(--text-muted);
    }

    .form-control {
      width: 100%;
      padding: 12px 16px;
      border-radius: 10px;
      background: var(--input-bg);
      border: 1px solid var(--glass-border);
      color: white;
      font-family: inherit;
      font-size: 15px;
    }

    .form-control:focus {
      outline: none;
      border-color: var(--secondary-color);
      box-shadow: 0 0 10px rgba(99, 102, 241, 0.3);
    }

    .auth-toggle {
      text-align: center;
      margin-top: 20px;
      font-size: 14px;
      color: var(--text-muted);
    }

    .auth-toggle a {
      color: var(--secondary-color);
      text-decoration: none;
      font-weight: 600;
    }

    .auth-toggle a:hover {
      text-decoration: underline;
    }

    /* Main Shorten Card */
    .shorten-container {
      display: none;
    }

    .shorten-form {
      display: grid;
      grid-template-columns: 1fr 1fr;
      gap: 20px;
    }

    .shorten-form .full-width {
      grid-column: span 2;
    }

    @media (max-width: 640px) {
      .shorten-form {
        grid-template-columns: 1fr;
      }
      .shorten-form .full-width {
        grid-column: span 1;
      }
    }

    /* Result Box */
    .result-box {
      margin-top: 24px;
      padding: 20px;
      border-radius: 12px;
      background: rgba(15, 23, 42, 0.8);
      border-left: 4px solid var(--secondary-color);
      display: none;
      animation: fadeIn 0.3s ease;
    }

    .result-box.success {
      border-left-color: #10b981;
    }
    .result-box.error {
      border-left-color: #ef4444;
    }

    .result-content {
      display: flex;
      justify-content: space-between;
      align-items: center;
      gap: 15px;
    }

    .result-link {
      font-size: 16px;
      font-weight: 700;
      color: #38bdf8;
      text-decoration: none;
      word-break: break-all;
    }

    /* Dashboard & History */
    .dashboard-container {
      display: none;
      flex-direction: column;
      gap: 20px;
    }

    .dashboard-header {
      display: flex;
      justify-content: space-between;
      align-items: center;
      gap: 20px;
      flex-wrap: wrap;
    }

    .dashboard-header h3 {
      margin: 0;
      font-size: 22px;
      font-weight: 700;
    }

    .search-bar {
      max-width: 300px;
      width: 100%;
    }

    /* History Cards */
    .history-list {
      display: flex;
      flex-direction: column;
      gap: 14px;
    }

    .history-item {
      display: grid;
      grid-template-columns: 2fr 3fr 1.2fr 1fr auto;
      align-items: center;
      gap: 20px;
      padding: 16px 20px;
      background: rgba(30, 41, 59, 0.25);
      border: 1px solid var(--glass-border);
      border-radius: 12px;
    }

    @media (max-width: 768px) {
      .history-item {
        grid-template-columns: 1fr;
        gap: 10px;
      }
    }

    .url-col {
      overflow: hidden;
    }

    .url-title {
      font-size: 12px;
      font-weight: 600;
      color: var(--text-muted);
      text-transform: uppercase;
      letter-spacing: 0.5px;
      margin-bottom: 4px;
    }

    .url-val {
      font-size: 15px;
      font-weight: 600;
      white-space: nowrap;
      text-overflow: ellipsis;
      overflow: hidden;
    }

    .url-original {
      color: var(--text-muted);
      font-size: 14px;
    }

    .url-short {
      color: #38bdf8;
      text-decoration: none;
      font-weight: 700;
    }

    .stat-badge {
      display: inline-flex;
      align-items: center;
      gap: 6px;
      padding: 4px 10px;
      border-radius: 20px;
      background: rgba(99, 102, 241, 0.15);
      color: #a5b4fc;
      font-size: 13px;
      font-weight: 600;
      justify-self: start;
    }

    .date-val {
      font-size: 13px;
      color: var(--text-muted);
    }

    .history-actions {
      display: flex;
      gap: 10px;
      justify-content: flex-end;
    }

    .action-btn {
      width: 36px;
      height: 36px;
      border-radius: 8px;
      display: flex;
      align-items: center;
      justify-content: center;
      cursor: pointer;
      border: none;
      background: rgba(255, 255, 255, 0.05);
      color: var(--text-main);
    }

    .action-btn:hover {
      background: rgba(255, 255, 255, 0.15);
    }

    .action-btn.btn-delete:hover {
      background: rgba(239, 68, 68, 0.2);
      color: #f87171;
    }

    /* Toast Notification */
    .toast {
      position: fixed;
      bottom: 24px;
      right: 24px;
      background: #1e293b;
      border: 1px solid var(--secondary-color);
      color: white;
      padding: 12px 24px;
      border-radius: 10px;
      box-shadow: 0 10px 25px rgba(0, 0, 0, 0.5);
      z-index: 100;
      opacity: 0;
      transform: translateY(20px);
      pointer-events: none;
      font-weight: 600;
    }

    .toast.show {
      opacity: 1;
      transform: translateY(0);
    }

    @keyframes fadeIn {
      from { opacity: 0; transform: translateY(10px); }
      to { opacity: 1; transform: translateY(0); }
    }

    .empty-state {
      text-align: center;
      padding: 40px 20px;
      color: var(--text-muted);
      font-size: 16px;
    }
  </style>
</head>
<body>

  <div class="blob blob-1"></div>
  <div class="blob blob-2"></div>

  <div class="app-container">
    <!-- Navigation Header -->
    <header>
      <div class="brand">
        <svg width="24" height="24" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round" style="color: var(--primary-color)"><path d="M10 13a5 5 0 0 0 7.54.54l3-3a5 5 0 0 0-7.07-7.07l-1.72 1.71"></path><path d="M14 11a5 5 0 0 0-7.54-.54l-3 3a5 5 0 0 0 7.07 7.07l1.71-1.71"></path></svg>
        ZipLink
      </div>
      <div class="user-badge" id="nav-user-info">
        <!-- Dyn contents -->
      </div>
    </header>

    <!-- Welcome / Choice Panel -->
    <div class="glass-card welcome-panel" id="welcome-view">
      <h1>Shorten Links. Analyze Clicks.</h1>
      <p>Transform long, cumbersome URLs into compact, high-performing links. Create an account to log history, track visits, and manage aliases, or start instantly as a guest.</p>
      <div class="welcome-buttons">
        <button class="btn btn-primary" onclick="showGuestMode()">Continue as Guest</button>
        <button class="btn btn-secondary" onclick="showAuthView(false)">Login / Signup</button>
      </div>
    </div>

    <!-- Login/Signup Panel -->
    <div class="glass-card auth-container" id="auth-view">
      <h2 id="auth-title">Welcome Back</h2>
      <form id="auth-form">
        <div class="form-group">
          <label for="auth-email">Email Address</label>
          <input type="email" id="auth-email" class="form-control" placeholder="name@domain.com" required>
        </div>
        <div class="form-group">
          <label for="auth-password">Password</label>
          <input type="password" id="auth-password" class="form-control" placeholder="••••••••" required>
        </div>
        <button type="submit" class="btn btn-primary" style="width: 100%; margin-top: 10px;" id="auth-submit-btn">Login</button>
      </form>
      <div class="auth-toggle">
        <span id="auth-toggle-text">Don't have an account?</span>
        <a href="#" onclick="toggleAuthMode()" id="auth-toggle-link">Sign Up</a>
      </div>
      <div class="auth-toggle" style="margin-top: 10px;">
        <a href="#" onclick="backToWelcome()" style="color: var(--text-muted)">← Back</a>
      </div>
    </div>

    <!-- Main Shortener Panel -->
    <div class="glass-card shorten-container" id="shorten-view">
      <form id="shorten-form" class="shorten-form">
        <div class="form-group full-width">
          <label for="url">Long URL</label>
          <input id="url" type="url" class="form-control" placeholder="https://example.com/long/path/here" required>
        </div>
        <div class="form-group">
          <label for="alias">Custom Alias (Optional)</label>
          <input id="alias" type="text" class="form-control" placeholder="my-custom-code">
        </div>
        <div class="form-group">
          <label for="expiry">Expiry Duration</label>
          <select id="expiry" class="form-control">
            <option value="0">Never Expire</option>
            <option value="3600">1 Hour</option>
            <option value="86400">1 Day</option>
            <option value="604800">1 Week</option>
            <option value="2592000">30 Days</option>
          </select>
        </div>
        <button type="submit" class="btn btn-primary full-width">Create Short URL</button>
      </form>

      <div id="result-box" class="result-box">
        <div class="result-content" id="result-box-content">
          <!-- Dyn content -->
        </div>
      </div>
    </div>

    <!-- Dashboard/History List -->
    <div class="glass-card dashboard-container" id="dashboard-view">
      <div class="dashboard-header">
        <h3>My Mapped Links</h3>
        <input type="text" id="search-input" class="form-control search-bar" placeholder="Search link codes or destinations...">
      </div>
      <div class="history-list" id="history-list">
        <!-- Dyn items -->
      </div>
    </div>
  </div>

  <div class="toast" id="toast">Copied to Clipboard!</div>

  <script>
    var isSignupMode = false;
    var currentUser = null;
    var linkHistory = [];

    // Initialize Page
    document.addEventListener("DOMContentLoaded", function() {
      checkAuthStatus().then(function() {
        setupFormListeners();
      });
    });

    // API Status Check
    function checkAuthStatus() {
      return fetch("/api/v1/me")
        .then(function(res) { return res.json(); })
        .then(function(data) {
          if (data.authenticated) {
            currentUser = { email: data.email, id: data.id };
            updateHeader(true);
            showShortenerAndDashboard();
            return loadHistory();
          } else {
            currentUser = null;
            updateHeader(false);
            showWelcome();
          }
        })
        .catch(function(err) {
          console.error("Auth initialization failed:", err);
        });
    }

    function updateHeader(loggedIn) {
      var info = document.getElementById("nav-user-info");
      if (loggedIn && currentUser) {
        info.innerHTML = "<span>👤 " + currentUser.email + "</span> <button class='btn btn-secondary' style='padding: 6px 12px; font-size: 12px;' onclick='handleLogout()'>Logout</button>";
      } else {
        info.innerHTML = "<button class='btn btn-secondary' style='padding: 6px 12px; font-size: 12px;' onclick='showAuthView(false)'>Sign In</button>";
      }
    }

    function showWelcome() {
      document.getElementById("welcome-view").style.display = "flex";
      document.getElementById("auth-view").style.display = "none";
      document.getElementById("shorten-view").style.display = "none";
      document.getElementById("dashboard-view").style.display = "none";
    }

    function showGuestMode() {
      document.getElementById("welcome-view").style.display = "none";
      document.getElementById("shorten-view").style.display = "block";
      document.getElementById("dashboard-view").style.display = "none";
      var info = document.getElementById("nav-user-info");
      info.innerHTML = "<span style='font-style: italic;'>👤 Guest Mode</span> <button class='btn btn-primary' style='padding: 6px 12px; font-size: 12px;' onclick='showAuthView(false)'>Register / Log In</button>";
    }

    function showAuthView(signup) {
      isSignupMode = signup;
      document.getElementById("welcome-view").style.display = "none";
      document.getElementById("shorten-view").style.display = "none";
      document.getElementById("dashboard-view").style.display = "none";
      document.getElementById("auth-view").style.display = "block";

      var title = document.getElementById("auth-title");
      var btn = document.getElementById("auth-submit-btn");
      var txt = document.getElementById("auth-toggle-text");
      var link = document.getElementById("auth-toggle-link");

      if (isSignupMode) {
        title.textContent = "Create Account";
        btn.textContent = "Register";
        txt.textContent = "Already have an account?";
        link.textContent = "Log In";
      } else {
        title.textContent = "Welcome Back";
        btn.textContent = "Log In";
        txt.textContent = "Don't have an account?";
        link.textContent = "Sign Up";
      }
    }

    function toggleAuthMode() {
      showAuthView(!isSignupMode);
    }

    function backToWelcome() {
      showWelcome();
    }

    function showShortenerAndDashboard() {
      document.getElementById("welcome-view").style.display = "none";
      document.getElementById("auth-view").style.display = "none";
      document.getElementById("shorten-view").style.display = "block";
      document.getElementById("dashboard-view").style.display = "flex";
    }

    // Auth actions
    function handleLogout() {
      fetch("/api/v1/logout", { method: "POST" })
        .then(function() {
          currentUser = null;
          updateHeader(false);
          showWelcome();
          linkHistory = [];
          showToast("Logged Out Successfully");
        })
        .catch(function(err) {
          console.error("Logout failed:", err);
        });
    }

    function setupFormListeners() {
      // Auth Form
      var authForm = document.getElementById("auth-form");
      authForm.addEventListener("submit", function(e) {
        e.preventDefault();
        var email = document.getElementById("auth-email").value;
        var password = document.getElementById("auth-password").value;
        var url = isSignupMode ? "/api/v1/signup" : "/api/v1/login";

        fetch(url, {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ email: email, password: password })
        })
        .then(function(res) {
          return res.json().then(function(data) {
            return { ok: res.ok, data: data };
          });
        })
        .then(function(result) {
          if (!result.ok) {
            alert(result.data.error || "Authentication failed");
            return;
          }

          if (isSignupMode) {
            isSignupMode = false;
            fetch("/api/v1/login", {
              method: "POST",
              headers: { "Content-Type": "application/json" },
              body: JSON.stringify({ email: email, password: password })
            })
            .then(function(loginRes) {
              if (loginRes.ok) {
                showToast("Registration Successful!");
                checkAuthStatus();
              } else {
                showAuthView(false);
              }
            });
          } else {
            showToast("Welcome Back!");
            checkAuthStatus();
          }
        })
        .catch(function(err) {
          console.error("Authentication error:", err);
          alert("Network or server error during authentication");
        });
      });

      // Shorten Form
      var shortenForm = document.getElementById("shorten-form");
      shortenForm.addEventListener("submit", function(e) {
        e.preventDefault();
        var urlInput = document.getElementById("url").value;
        var aliasInput = document.getElementById("alias").value.trim();
        var expiryInput = parseInt(document.getElementById("expiry").value);

        var payload = { url: urlInput };
        if (aliasInput) payload.custom_alias = aliasInput;
        if (expiryInput > 0) payload.expires_in_seconds = expiryInput;

        var resultBox = document.getElementById("result-box");
        var resultContent = document.getElementById("result-box-content");

        resultBox.style.display = "block";
        resultBox.className = "result-box";
        resultContent.textContent = "Generating short link...";

        fetch("/api/v1/urls", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(payload)
        })
        .then(function(res) {
          return res.json().then(function(data) {
            return { ok: res.ok, data: data };
          });
        })
        .then(function(result) {
          if (!result.ok) {
            resultBox.classList.add("error");
            if (result.data.short_url) {
              resultContent.innerHTML = "<div><span style='font-weight: 600;'>Error: " + result.data.error + "</span><br/>Existing short URL: <a href='" + result.data.short_url + "' class='result-link' target='_blank'>" + result.data.short_url + "</a></div>";
            } else {
              resultContent.textContent = result.data.error || "Generation failed";
            }
            return;
          }

          resultBox.classList.add("success");
          resultContent.innerHTML = "<div><span style='font-size: 13px; color: var(--text-muted); font-weight: 600;'>ZIPLINK READY:</span><br/><a href='" + result.data.short_url + "' id='new-short-link' class='result-link' target='_blank'>" + result.data.short_url + "</a></div><button class='btn btn-secondary' style='padding: 6px 12px; font-size: 12px;' onclick=\"copyText('" + result.data.short_url + "')\">Copy</button>";

          shortenForm.reset();

          if (currentUser) {
            loadHistory();
          }
        })
        .catch(function(err) {
          console.error("Shorten error:", err);
          resultBox.classList.add("error");
          resultContent.textContent = "Server error occurred.";
        });
      });

      // Realtime search
      document.getElementById("search-input").addEventListener("input", function(e) {
        renderHistory(e.target.value);
      });
    }

    // Load History
    function loadHistory() {
      return fetch("/api/v1/user/urls")
        .then(function(res) {
          if (res.ok) {
            return res.json().then(function(data) {
              linkHistory = data;
              renderHistory();
            });
          }
        })
        .catch(function(err) {
          console.error("Failed to load link history:", err);
        });
    }

    // Render History
    function renderHistory(query) {
      var q = (query || "").toLowerCase();
      var container = document.getElementById("history-list");
      container.innerHTML = "";

      var filtered = linkHistory.filter(function(item) {
        return item.code.toLowerCase().indexOf(q) !== -1 || item.original_url.toLowerCase().indexOf(q) !== -1;
      });

      if (filtered.length === 0) {
        container.innerHTML = "<div class='empty-state'>No links matches your filter. Get shorten'n!</div>";
        return;
      }

      filtered.forEach(function(item) {
        var itemEl = document.createElement("div");
        itemEl.className = "history-item";

        var expireLabel = "Never Expired";
        if (item.expires_at) {
          var expiryTime = new Date(item.expires_at);
          if (expiryTime < new Date()) {
            expireLabel = "Expired";
          } else {
            expireLabel = "Expires " + expiryTime.toLocaleDateString();
          }
        }

        itemEl.innerHTML = '<div class="url-col">' +
          '<div class="url-title">Short Code</div>' +
          '<div class="url-val"><a href="' + item.short_url + '" class="url-short" target="_blank">' + item.code + '</a></div>' +
          '</div>' +
          '<div class="url-col">' +
          '<div class="url-title">Original URL</div>' +
          '<div class="url-val url-original" title="' + item.original_url + '">' + item.original_url + '</div>' +
          '</div>' +
          '<div>' +
          '<div class="url-title">Activity</div>' +
          '<div class="stat-badge">🔥 ' + item.visits + ' clicks</div>' +
          '</div>' +
          '<div>' +
          '<div class="url-title">Lifespan</div>' +
          '<div class="date-val">' + expireLabel + '</div>' +
          '</div>' +
          '<div class="history-actions">' +
          '<button class="action-btn" title="Copy to Clipboard" onclick="copyText(\'' + item.short_url + '\')">' +
          '<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="9" y="9" width="13" height="13" rx="2" ry="2"></rect><path d="M5 15H4a2 2 0 0 1-2-2V4a2 2 0 0 1 2-2h9a2 2 0 0 1 2 2v1"></path></svg>' +
          '</button>' +
          '<button class="action-btn btn-delete" title="Delete Link" onclick="deleteLink(\'' + item.code + '\')">' +
          '<svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="3 6 5 6 21 6"></polyline><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"></path></svg>' +
          '</button>' +
          '</div>';

        container.appendChild(itemEl);
      });
    }

    // Delete Link
    function deleteLink(code) {
      if (!confirm("Are you sure you want to delete short code: " + code + "?")) {
        return;
      }
      fetch("/api/v1/urls/" + code, { method: "DELETE" })
        .then(function(res) {
          if (res.ok) {
            showToast("Link Deleted Successfully");
            loadHistory();
          } else {
            res.json().then(function(errData) {
              alert(errData.error || "Failed to delete link");
            });
          }
        })
        .catch(function(err) {
          console.error("Delete failed:", err);
        });
    }

    // Toast & Helpers
    function copyText(text) {
      navigator.clipboard.writeText(text).then(function() {
        showToast("Copied to Clipboard!");
      }).catch(function(err) {
        console.error("Clipboard copy failed:", err);
      });
    }

    function showToast(message) {
      var toast = document.getElementById("toast");
      toast.textContent = message;
      toast.classList.add("show");
      setTimeout(function() {
        toast.classList.remove("show");
      }, 2500);
    }
  </script>
</body>
</html>`
