package auth

import (
	"errors"
	"net/http"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/mudit/creatorflow/backend/internal/httpx"
	"github.com/mudit/creatorflow/backend/internal/middleware"
)

// refreshCookieName is scoped to the auth routes by CookieConfig.Path, so the
// 30-day credential is not attached to every storefront, product or payment
// request. A cookie that is only sent where it is needed is a cookie that only
// leaks where it is needed.
const refreshCookieName = "refresh_token"

// CookieConfig is the deployment-dependent half of the cookie. Secure must be
// off for http://localhost or the browser silently drops the cookie and refresh
// looks broken for reasons nothing logs.
type CookieConfig struct {
	Secure bool
	Path   string
	MaxAge time.Duration
}

type Handler struct {
	svc    *Service
	cookie CookieConfig
}

func NewHandler(svc *Service, cookie CookieConfig) *Handler {
	return &Handler{svc: svc, cookie: cookie}
}

// RegisterRoutes takes the auth middleware rather than building it, so main.go
// remains the single place where you can read which routes are protected.
func (h *Handler) RegisterRoutes(r fiber.Router, requireAuth fiber.Handler) {
	g := r.Group("/auth")
	g.Post("/register", h.register)
	g.Post("/login", h.login)
	// Refresh and logout are authenticated by the cookie, not by the access
	// token — demanding a valid access token to refresh would defeat the point,
	// since the whole reason to refresh is that it expired.
	g.Post("/refresh", h.refresh)
	g.Post("/logout", h.logout)

	r.Get("/me", requireAuth, h.me)
}

type credentialsRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type userResponse struct {
	ID            string `json:"id"`
	Email         string `json:"email"`
	Role          string `json:"role"`
	EmailVerified bool   `json:"email_verified"`
}

// sessionResponse carries the access token only. The refresh token leaves in a
// Set-Cookie header the browser will not let JavaScript read, so an XSS bug can
// at worst steal 15 minutes of access rather than 30 days of account.
type sessionResponse struct {
	AccessToken string       `json:"access_token"`
	TokenType   string       `json:"token_type"`
	ExpiresIn   int          `json:"expires_in"`
	User        userResponse `json:"user"`
}

func (h *Handler) register(c *fiber.Ctx) error {
	req, err := parseCredentials(c)
	if err != nil {
		return err
	}

	session, err := h.svc.Register(c.Context(), req.Email, req.Password)
	if err != nil {
		var ve *ValidationError
		switch {
		case errors.As(err, &ve):
			return httpx.Validation(map[string]string{ve.Field: ve.Message})
		case errors.Is(err, ErrEmailTaken):
			return httpx.New(http.StatusConflict, "email_taken", "That email is already registered.")
		default:
			return httpx.ErrInternal.WithCause(err)
		}
	}

	h.setRefreshCookie(c, session)
	return c.Status(http.StatusCreated).JSON(newSessionResponse(session))
}

func (h *Handler) login(c *fiber.Ctx) error {
	req, err := parseCredentials(c)
	if err != nil {
		return err
	}

	// Not validated the way register validates. Rejecting a short password here
	// with "must be 12 characters" would tell an attacker their guess failed the
	// length rule rather than the hash — and would lock out any account whose
	// password predates a future rule change.
	session, err := h.svc.Login(c.Context(), req.Email, req.Password)
	if err != nil {
		if errors.Is(err, ErrInvalidCredentials) {
			return httpx.New(http.StatusUnauthorized, "invalid_credentials", "Email or password is incorrect.")
		}
		return httpx.ErrInternal.WithCause(err)
	}

	h.setRefreshCookie(c, session)
	return c.JSON(newSessionResponse(session))
}

func (h *Handler) refresh(c *fiber.Ctx) error {
	raw := c.Cookies(refreshCookieName)
	if raw == "" {
		return httpx.New(http.StatusUnauthorized, "no_refresh_token", "No session cookie was sent.")
	}

	session, err := h.svc.Refresh(c.Context(), raw)
	if err != nil {
		// Whatever went wrong, the cookie the browser holds is now worthless.
		// Clearing it stops the client retrying a token that can never work.
		h.clearRefreshCookie(c)

		switch {
		case errors.Is(err, ErrTokenReused):
			return httpx.New(http.StatusUnauthorized, "session_revoked",
				"This session was already used and has been revoked. Sign in again.")
		case errors.Is(err, ErrInvalidToken):
			return httpx.New(http.StatusUnauthorized, "invalid_refresh_token", "Your session has expired. Sign in again.")
		default:
			return httpx.ErrInternal.WithCause(err)
		}
	}

	h.setRefreshCookie(c, session)
	return c.JSON(newSessionResponse(session))
}

func (h *Handler) logout(c *fiber.Ctx) error {
	if err := h.svc.Logout(c.Context(), c.Cookies(refreshCookieName)); err != nil {
		return httpx.ErrInternal.WithCause(err)
	}

	h.clearRefreshCookie(c)
	return c.SendStatus(http.StatusNoContent)
}

func (h *Handler) me(c *fiber.Ctx) error {
	userID, ok := middleware.UserID(c)
	if !ok {
		return httpx.ErrUnauthorized
	}

	// The claims already carry the id and role, but this reads the row: a user
	// deleted or demoted two minutes ago still holds a valid access token, and
	// /me is where the client discovers that.
	user, err := h.svc.UserByID(c.Context(), userID)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			return httpx.ErrUnauthorized
		}
		return httpx.ErrInternal.WithCause(err)
	}

	return c.JSON(newUserResponse(user))
}

func parseCredentials(c *fiber.Ctx) (*credentialsRequest, error) {
	var req credentialsRequest
	if err := c.BodyParser(&req); err != nil {
		return nil, httpx.New(http.StatusBadRequest, "invalid_body", "Request body must be valid JSON.")
	}
	return &req, nil
}

func (h *Handler) setRefreshCookie(c *fiber.Ctx, s *Session) {
	c.Cookie(&fiber.Cookie{
		Name:  refreshCookieName,
		Value: s.RefreshToken,
		Path:  h.cookie.Path,
		// Expires tracks the database row, so the browser stops sending a cookie
		// the server would reject anyway.
		Expires: s.RefreshExpiresAt,
		// HTTPOnly is the entire reason for using a cookie here.
		HTTPOnly: true,
		// Secure is off locally because localhost is plain http; in production a
		// refresh token must never be allowed onto an unencrypted connection.
		Secure: h.cookie.Secure,
		// Strict, not Lax: this cookie is only ever needed on same-site XHR from
		// our own frontend, never on a link followed from elsewhere. That also
		// removes most of the CSRF surface on /refresh without a CSRF token.
		SameSite: fiber.CookieSameSiteStrictMode,
	})
}

// clearRefreshCookie must repeat Path exactly. A browser treats a cookie as
// keyed by (name, domain, path), so an expiry sent on the wrong path creates a
// second cookie instead of removing the first.
func (h *Handler) clearRefreshCookie(c *fiber.Ctx) {
	c.Cookie(&fiber.Cookie{
		Name:     refreshCookieName,
		Value:    "",
		Path:     h.cookie.Path,
		Expires:  time.Now().Add(-time.Hour),
		MaxAge:   -1,
		HTTPOnly: true,
		Secure:   h.cookie.Secure,
		SameSite: fiber.CookieSameSiteStrictMode,
	})
}

func newSessionResponse(s *Session) sessionResponse {
	return sessionResponse{
		AccessToken: s.AccessToken,
		TokenType:   "Bearer",
		ExpiresIn:   int(s.AccessExpiresIn.Seconds()),
		User:        newUserResponse(s.User),
	}
}

// newUserResponse is a separate struct from the domain User on purpose. If the
// handler serialised *User directly, the day someone adds a field to User is the
// day PasswordHash appears in a JSON response.
func newUserResponse(u *User) userResponse {
	return userResponse{
		ID:            u.ID.String(),
		Email:         u.Email,
		Role:          u.Role,
		EmailVerified: u.EmailVerified,
	}
}
