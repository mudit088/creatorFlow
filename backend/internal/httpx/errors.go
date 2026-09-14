package httpx

import (
	"errors"
	"net/http"

	"github.com/gofiber/fiber/v2"
)

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
func ErrorHandler(c *fiber.Ctx, err error) error {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return c.Status(apiErr.status).JSON(fiber.Map{"error": apiErr})
	}

	var fiberErr *fiber.Error
	if errors.As(err, &fiberErr) {
		return c.Status(fiberErr.Code).JSON(fiber.Map{
			"error": New(fiberErr.Code, "http_error", fiberErr.Message),
		})
	}

	// Unknown error: log the real thing, tell the client nothing.
	return c.Status(http.StatusInternalServerError).JSON(fiber.Map{"error": ErrInternal})
}
