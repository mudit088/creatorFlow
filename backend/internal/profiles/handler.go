package profiles

import (
	"errors"
	"net/http"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"

	"github.com/mudit/creatorflow/backend/internal/httpx"
	"github.com/mudit/creatorflow/backend/internal/middleware"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

type createProfileRequest struct {
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
}

// updateProfileRequest uses pointers so that "field absent" and "field set to an
// empty value" are different things. With plain strings, a PATCH that only
// changes the bio would also silently blank the display name, because the zero
// value is indistinguishable from an intentional "".
type updateProfileRequest struct {
	DisplayName *string `json:"display_name"`
	Bio         *string `json:"bio"`
	IsPublished *bool   `json:"is_published"`
}

type createLinkRequest struct {
	Title string `json:"title"`
	URL   string `json:"url"`
}

type updateLinkRequest struct {
	Title    *string `json:"title"`
	URL      *string `json:"url"`
	IsActive *bool   `json:"is_active"`
}

type reorderRequest struct {
	LinkIDs []string `json:"link_ids"`
}

type profileResponse struct {
	ID          string  `json:"id"`
	Username    string  `json:"username"`
	DisplayName string  `json:"display_name"`
	Bio         *string `json:"bio"`
	AvatarKey   *string `json:"avatar_key"`
	IsPublished bool    `json:"is_published"`
}

// profileWithLinksResponse is a separate shape rather than an omitempty field on
// profileResponse. An omitted "links" key and an empty one mean different things
// to a client: "not loaded" versus "you have none". Embedding flattens into the
// same flat JSON object while keeping the two cases honest.
type profileWithLinksResponse struct {
	profileResponse
	Links []linkResponse `json:"links"`
}

type linkResponse struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	Position int    `json:"position"`
	IsActive bool   `json:"is_active"`
}

func (h *Handler) Create(c *fiber.Ctx) error {
	userID, ok := middleware.UserID(c)
	if !ok {
		return httpx.ErrUnauthorized
	}

	var req createProfileRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.New(http.StatusBadRequest, "invalid_body", "Request body must be valid JSON.")
	}

	profile, err := h.svc.Create(c.Context(), userID, req.Username, req.DisplayName)
	if err != nil {
		return mapError(err)
	}

	return c.Status(http.StatusCreated).JSON(newProfileResponse(profile))
}

func (h *Handler) Own(c *fiber.Ctx) error {
	userID, ok := middleware.UserID(c)
	if !ok {
		return httpx.ErrUnauthorized
	}

	profile, links, err := h.svc.Own(c.Context(), userID)
	if err != nil {
		return mapError(err)
	}

	return c.JSON(profileWithLinksResponse{
		profileResponse: newProfileResponse(profile),
		Links:           newLinkResponses(links),
	})
}

func (h *Handler) Update(c *fiber.Ctx) error {
	userID, ok := middleware.UserID(c)
	if !ok {
		return httpx.ErrUnauthorized
	}

	var req updateProfileRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.New(http.StatusBadRequest, "invalid_body", "Request body must be valid JSON.")
	}

	profile, err := h.svc.Update(c.Context(), userID, req.DisplayName, req.Bio, req.IsPublished)
	if err != nil {
		return mapError(err)
	}

	return c.JSON(newProfileResponse(profile))
}

func (h *Handler) AddLink(c *fiber.Ctx) error {
	userID, ok := middleware.UserID(c)
	if !ok {
		return httpx.ErrUnauthorized
	}

	var req createLinkRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.New(http.StatusBadRequest, "invalid_body", "Request body must be valid JSON.")
	}

	link, err := h.svc.AddLink(c.Context(), userID, req.Title, req.URL)
	if err != nil {
		return mapError(err)
	}

	return c.Status(http.StatusCreated).JSON(newLinkResponse(*link))
}

func (h *Handler) UpdateLink(c *fiber.Ctx) error {
	userID, ok := middleware.UserID(c)
	if !ok {
		return httpx.ErrUnauthorized
	}

	linkID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return httpx.New(http.StatusBadRequest, "invalid_link_id", "That link id is not a valid identifier.")
	}

	var req updateLinkRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.New(http.StatusBadRequest, "invalid_body", "Request body must be valid JSON.")
	}

	link, err := h.svc.UpdateLink(c.Context(), userID, linkID, req.Title, req.URL, req.IsActive)
	if err != nil {
		return mapError(err)
	}

	return c.JSON(newLinkResponse(*link))
}

func (h *Handler) DeleteLink(c *fiber.Ctx) error {
	userID, ok := middleware.UserID(c)
	if !ok {
		return httpx.ErrUnauthorized
	}

	linkID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return httpx.New(http.StatusBadRequest, "invalid_link_id", "That link id is not a valid identifier.")
	}

	if err := h.svc.DeleteLink(c.Context(), userID, linkID); err != nil {
		return mapError(err)
	}

	return c.SendStatus(http.StatusNoContent)
}

func (h *Handler) Reorder(c *fiber.Ctx) error {
	userID, ok := middleware.UserID(c)
	if !ok {
		return httpx.ErrUnauthorized
	}

	var req reorderRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.New(http.StatusBadRequest, "invalid_body", "Request body must be valid JSON.")
	}

	ids := make([]uuid.UUID, 0, len(req.LinkIDs))
	for _, raw := range req.LinkIDs {
		id, err := uuid.Parse(raw)
		if err != nil {
			return httpx.Validation(map[string]string{"link_ids": "One of the link ids is not a valid identifier."})
		}
		ids = append(ids, id)
	}

	links, err := h.svc.Reorder(c.Context(), userID, ids)
	if err != nil {
		return mapError(err)
	}

	return c.JSON(fiber.Map{"links": newLinkResponses(links)})
}

// mapError is the single place domain errors become HTTP. Every handler routes
// through it so a new domain error cannot be given one status in one handler and
// a different one three functions later.
func mapError(err error) error {
	var ve *ValidationError
	switch {
	case errors.As(err, &ve):
		return httpx.Validation(map[string]string{ve.Field: ve.Message})
	case errors.Is(err, ErrProfileExists):
		return httpx.New(http.StatusConflict, "profile_exists", "You already have a profile.")
	case errors.Is(err, ErrUsernameTaken):
		return httpx.New(http.StatusConflict, "username_taken", "That username is already taken.")
	case errors.Is(err, ErrUsernameInvalid):
		return httpx.Validation(map[string]string{"username": "That username is not allowed."})
	case errors.Is(err, ErrProfileNotFound):
		return httpx.New(http.StatusNotFound, "profile_not_found", "No profile found.")
	case errors.Is(err, ErrLinkNotFound):
		return httpx.New(http.StatusNotFound, "link_not_found", "That link does not exist.")
	case errors.Is(err, ErrTooManyLinks):
		return httpx.New(http.StatusConflict, "too_many_links", "You have reached the maximum number of links.")
	case errors.Is(err, ErrOrderIncomplete):
		return httpx.Validation(map[string]string{"link_ids": "List every one of your links exactly once."})
	default:
		return httpx.ErrInternal.WithCause(err)
	}
}

func newProfileResponse(p *Profile) profileResponse {
	return profileResponse{
		ID:          p.ID.String(),
		Username:    p.Username,
		DisplayName: p.DisplayName,
		Bio:         p.Bio,
		AvatarKey:   p.AvatarKey,
		IsPublished: p.IsPublished,
	}
}

// newLinkResponses never returns nil, so an empty page serialises as [] and not
// as JSON null, which is a frontend crash rather than a design choice.
func newLinkResponses(links []Link) []linkResponse {
	out := make([]linkResponse, 0, len(links))
	for _, l := range links {
		out = append(out, newLinkResponse(l))
	}
	return out
}

func newLinkResponse(l Link) linkResponse {
	return linkResponse{
		ID:       l.ID.String(),
		Title:    l.Title,
		URL:      l.URL,
		Position: l.Position,
		IsActive: l.IsActive,
	}
}
