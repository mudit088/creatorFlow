package httpx

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/gofiber/fiber/v2"
)

// contextKeyRequestID duplicates the key set by middleware.RequestID rather
// than importing it. httpx must stay at the bottom of the dependency graph so
// middleware can return httpx errors; importing middleware here would make that
// a cycle. One duplicated string constant is the cheaper of the two costs.
const contextKeyRequestID = "request_id"

// APIError is the single error shape this API ever returns. Clients can switch
// on Code; humans read Message. Internal detail never crosses this boundary —
// a database error becomes "internal_error", and the real cause goes to the log
// with the request ID attached so support can correlate the two.
type APIError struct {
	Code    string            `json:"code"`
	Message string            `json:"message"`
	Fields  map[string]string `json:"fields,omitempty"`

	status int
	cause  error
}

func (e *APIError) Error() string { return e.Message }
func (e *APIError) Unwrap() error { return e.cause }
func (e *APIError) Status() int   { return e.status }

func (e *APIError) WithCause(err error) *APIError {
	clone := *e
	clone.cause = err
	return &clone
}

func New(status int, code, message string) *APIError {
	return &APIError{Code: code, Message: message, status: status}
}

var (
	ErrUnauthorized = New(http.StatusUnauthorized, "unauthorized", "Authentication is required.")
	ErrForbidden    = New(http.StatusForbidden, "forbidden", "You do not have access to this resource.")
	ErrNotFound     = New(http.StatusNotFound, "not_found", "The requested resource does not exist.")
	ErrConflict     = New(http.StatusConflict, "conflict", "That resource already exists.")
	ErrRateLimited  = New(http.StatusTooManyRequests, "rate_limited", "Too many requests. Try again shortly.")
	ErrInternal     = New(http.StatusInternalServerError, "internal_error", "Something went wrong on our end.")
)

func Validation(fields map[string]string) *APIError {
	return &APIError{
		Code:    "validation_failed",
		Message: "Some fields are invalid.",
		Fields:  fields,
		status:  http.StatusUnprocessableEntity,
	}
}

// ErrorHandler is wired into Fiber once, so handlers can simply `return
// httpx.ErrNotFound` and the response shape stays consistent everywhere.
//
// It is also the only place a 500 is allowed to be born, which makes it the
// only place that has to log. The client is told nothing beyond
// "internal_error"; the real cause goes to the log tagged with the request ID
// that was echoed in the response header, so a user reporting a failure hands
// you the exact key to find it.
func ErrorHandler(c *fiber.Ctx, err error) error {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		if apiErr.status >= http.StatusInternalServerError {
			logCause(c, err)
		}
		return c.Status(apiErr.status).JSON(fiber.Map{"error": apiErr})
	}

	var fiberErr *fiber.Error
	if errors.As(err, &fiberErr) {
		return c.Status(fiberErr.Code).JSON(fiber.Map{
			"error": New(fiberErr.Code, "http_error", fiberErr.Message),
		})
	}

	// An error that reached here is one nobody translated — a bug by definition.
	logCause(c, err)
	return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": ErrInternal})
}

func logCause(c *fiber.Ctx, err error) {
	// APIError.Error() returns only the safe, user-facing message, so logging
	// the wrapper would log exactly the sanitised string we already sent to the
	// client and none of the detail we actually need.
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.cause != nil {
		err = apiErr.cause
	}

	requestID, _ := c.Locals(contextKeyRequestID).(string)
	slog.Error("request failed",
		"error", err,
		"request_id", requestID,
		"method", c.Method(),
		"path", c.Path(),
	)
}
