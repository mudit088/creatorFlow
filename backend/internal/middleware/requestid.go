package middleware

import (
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

const (
	HeaderRequestID = "X-Request-ID"
	ContextKeyReqID = "request_id"
)

// RequestID attaches a stable identifier to every request. It is echoed in the
// response header and included in every log line, which is what turns "the site
// broke at 3pm" into a single greppable trace across API, worker and webhook
// logs. An inbound ID is trusted only for correlation, never for authorization.
func RequestID() fiber.Handler {
	return func(c *fiber.Ctx) error {
		id := c.Get(HeaderRequestID)
		if id == "" || len(id) > 64 {
			id = uuid.NewString()
		}
		c.Locals(ContextKeyReqID, id)
		c.Set(HeaderRequestID, id)
		return c.Next()
	}
}

// FromContext returns the request ID for logging.
func FromContext(c *fiber.Ctx) string {
	if v, ok := c.Locals(ContextKeyReqID).(string); ok {
		return v
	}
	return ""
}
