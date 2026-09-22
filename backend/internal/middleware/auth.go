package middleware

import (
	"strings"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
)

const (
	ContextKeyUserID = "user_id"
	ContextKeyRole   = "role"
)

// ClaimsParser is satisfied by auth.TokenIssuer. Declaring the interface here,
// where it is consumed, keeps middleware from importing auth and auth from
// importing middleware.
type ClaimsParser interface {
	ParseUserID(raw string) (uuid.UUID, string, error)
}

func RequireAuth(parser ClaimsParser, unauthorized error) fiber.Handler {
	return func(c *fiber.Ctx) error {
		header := c.Get("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			return unauthorized
		}

		userID, role, err := parser.ParseUserID(strings.TrimPrefix(header, "Bearer "))
		if err != nil {
			return unauthorized
		}

		c.Locals(ContextKeyUserID, userID)
		c.Locals(ContextKeyRole, role)
		return c.Next()
	}
}

func UserID(c *fiber.Ctx) (uuid.UUID, bool) {
	id, ok := c.Locals(ContextKeyUserID).(uuid.UUID)
	return id, ok
}

func RequireRole(role string, forbidden error) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if r, _ := c.Locals(ContextKeyRole).(string); r != role {
			return forbidden
		}
		return c.Next()
	}
}
