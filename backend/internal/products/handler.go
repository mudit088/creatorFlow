package products

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

func (h *Handler) RegisterRoutes(r fiber.Router, requireAuth fiber.Handler) {
	g := r.Group("/products", requireAuth)
	g.Post("/", h.create)
	g.Get("/", h.list)
	g.Get("/:id", h.get)
	g.Patch("/:id", h.update)

	// The upload endpoints. Note that none of them carry a file body: the API
	// hands out a signed URL and later asks S3 what happened. A 2 GB product
	// never touches this process, which is what keeps the API's memory flat and
	// its BodyLimit at 2 MB.
	g.Post("/:id/upload-url", h.requestUpload)
	g.Post("/:id/files/:fileId/confirm", h.confirmUpload)
	g.Delete("/:id/files/:fileId", h.deleteFile)
}

type createProductRequest struct {
	Slug        string  `json:"slug"`
	Title       string  `json:"title"`
	Description *string `json:"description"`
	PriceMinor  int64   `json:"price_minor"`
}

type updateProductRequest struct {
	Slug        *string `json:"slug"`
	Title       *string `json:"title"`
	Description *string `json:"description"`
	PriceMinor  *int64  `json:"price_minor"`
	Status      *string `json:"status"`
}

type uploadURLRequest struct {
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
}

type productResponse struct {
	ID          string  `json:"id"`
	Slug        string  `json:"slug"`
	Title       string  `json:"title"`
	Description *string `json:"description"`
	PriceMinor  int64   `json:"price_minor"`
	Currency    string  `json:"currency"`
	Status      string  `json:"status"`
	PublishedAt *string `json:"published_at"`
}

type productWithFilesResponse struct {
	productResponse
	Files []fileResponse `json:"files"`
}

// fileResponse deliberately omits s3_key. The key is an internal storage
// address; a client that knows it can do nothing useful with it, and publishing
// the bucket's layout only helps someone probing for objects.
type fileResponse struct {
	ID           string  `json:"id"`
	OriginalName string  `json:"original_name"`
	ContentType  string  `json:"content_type"`
	SizeBytes    int64   `json:"size_bytes"`
	Checksum     *string `json:"checksum"`
	Uploaded     bool    `json:"uploaded"`
}

type uploadIntentResponse struct {
	File      fileResponse `json:"file"`
	UploadURL string       `json:"upload_url"`
	ExpiresIn int          `json:"expires_in"`
	// Echoed back so the client sends exactly what was signed. Getting either of
	// these wrong produces a SignatureDoesNotMatch from S3, which is a confusing
	// error to debug if the client had to guess them.
	RequiredHeaders map[string]string `json:"required_headers"`
	Method          string            `json:"method"`
}

func (h *Handler) create(c *fiber.Ctx) error {
	userID, ok := middleware.UserID(c)
	if !ok {
		return httpx.ErrUnauthorized
	}

	var req createProductRequest
	if err := c.BodyParser(&req); err != nil {
		return httpx.New(http.StatusBadRequest, "invalid_body", "Request body must be valid JSON.")
	}

	product, err := h.svc.Create(c.Context(), userID, req.Slug, req.Title, req.Description, req.PriceMinor)
	if err != nil {
		return mapError(err)
	}

	return c.Status(http.StatusCreated).JSON(newProductResponse(product))
}

func (h *Handler) list(c *fiber.Ctx) error {
	userID, ok := middleware.UserID(c)
	if !ok {
		return httpx.ErrUnauthorized
	}

	items, err := h.svc.List(c.Context(), userID)
	if err != nil {
		return mapError(err)
	}

	out := make([]productResponse, 0, len(items))
	for i := range items {
		out = append(out, newProductResponse(&items[i]))
	}
	return c.JSON(fiber.Map{"products": out})
}

func (h *Handler) get(c *fiber.Ctx) error {
	userID, productID, err := h.scope(c)
	if err != nil {
		return err
	}

	product, files, svcErr := h.svc.Get(c.Context(), userID, productID)
	if svcErr != nil {
		return mapError(svcErr)
	}

	return c.JSON(productWithFilesResponse{
		productResponse: newProductResponse(product),
		Files:           newFileResponses(files),
	})
}

func (h *Handler) update(c *fiber.Ctx) error {
	userID, productID, err := h.scope(c)
	if err != nil {
		return err
	}

	var req updateProductRequest
	if parseErr := c.BodyParser(&req); parseErr != nil {
		return httpx.New(http.StatusBadRequest, "invalid_body", "Request body must be valid JSON.")
	}

	product, svcErr := h.svc.Update(c.Context(), userID, productID, req.Title, req.Description, req.Slug, req.PriceMinor, req.Status)
	if svcErr != nil {
		return mapError(svcErr)
	}

	return c.JSON(newProductResponse(product))
}

func (h *Handler) requestUpload(c *fiber.Ctx) error {
	userID, productID, err := h.scope(c)
	if err != nil {
		return err
	}

	var req uploadURLRequest
	if parseErr := c.BodyParser(&req); parseErr != nil {
		return httpx.New(http.StatusBadRequest, "invalid_body", "Request body must be valid JSON.")
	}

	intent, svcErr := h.svc.RequestUpload(c.Context(), userID, productID, req.Filename, req.ContentType, req.SizeBytes)
	if svcErr != nil {
		return mapError(svcErr)
	}

	return c.Status(http.StatusCreated).JSON(uploadIntentResponse{
		File:      newFileResponse(*intent.File),
		UploadURL: intent.UploadURL,
		ExpiresIn: int(intent.ExpiresIn.Seconds()),
		Method:    http.MethodPut,
		RequiredHeaders: map[string]string{
			"Content-Type": intent.File.ContentType,
		},
	})
}

func (h *Handler) confirmUpload(c *fiber.Ctx) error {
	userID, productID, err := h.scope(c)
	if err != nil {
		return err
	}

	fileID, parseErr := uuid.Parse(c.Params("fileId"))
	if parseErr != nil {
		return httpx.New(http.StatusBadRequest, "invalid_file_id", "That file id is not a valid identifier.")
	}

	file, svcErr := h.svc.ConfirmUpload(c.Context(), userID, productID, fileID)
	if svcErr != nil {
		return mapError(svcErr)
	}

	return c.JSON(newFileResponse(*file))
}

func (h *Handler) deleteFile(c *fiber.Ctx) error {
	userID, productID, err := h.scope(c)
	if err != nil {
		return err
	}

	fileID, parseErr := uuid.Parse(c.Params("fileId"))
	if parseErr != nil {
		return httpx.New(http.StatusBadRequest, "invalid_file_id", "That file id is not a valid identifier.")
	}

	if svcErr := h.svc.DeleteFile(c.Context(), userID, productID, fileID); svcErr != nil {
		return mapError(svcErr)
	}

	return c.SendStatus(http.StatusNoContent)
}

// scope pulls the two things every product route needs: who is asking, and about
// what. Both are validated here so no handler proceeds with a half-parsed
// request.
func (h *Handler) scope(c *fiber.Ctx) (uuid.UUID, uuid.UUID, error) {
	userID, ok := middleware.UserID(c)
	if !ok {
		return uuid.Nil, uuid.Nil, httpx.ErrUnauthorized
	}

	productID, err := uuid.Parse(c.Params("id"))
	if err != nil {
		return uuid.Nil, uuid.Nil, httpx.New(http.StatusBadRequest, "invalid_product_id", "That product id is not a valid identifier.")
	}
	return userID, productID, nil
}

func mapError(err error) error {
	var ve *ValidationError
	switch {
	case errors.As(err, &ve):
		return httpx.Validation(map[string]string{ve.Field: ve.Message})
	case errors.Is(err, ErrProductNotFound):
		return httpx.New(http.StatusNotFound, "product_not_found", "That product does not exist.")
	case errors.Is(err, ErrFileNotFound):
		return httpx.New(http.StatusNotFound, "file_not_found", "That file does not exist.")
	case errors.Is(err, ErrProfileRequired):
		return httpx.New(http.StatusConflict, "profile_required", "Create your profile before adding products.")
	case errors.Is(err, ErrSlugTaken):
		return httpx.New(http.StatusConflict, "slug_taken", "You already have a product with that slug.")
	case errors.Is(err, ErrTooManyFiles):
		return httpx.New(http.StatusConflict, "too_many_files", "This product already has the maximum number of files.")
	case errors.Is(err, ErrNoDeliverable):
		return httpx.New(http.StatusConflict, "no_deliverable", "Upload and confirm at least one file before publishing.")
	case errors.Is(err, ErrUploadMissing):
		return httpx.New(http.StatusConflict, "upload_missing", "No uploaded file was found. Complete the upload and try again.")
	case errors.Is(err, ErrUploadMismatch):
		return httpx.New(http.StatusConflict, "upload_mismatch", "The uploaded file does not match what was declared.")
	case errors.Is(err, ErrAlreadyUploaded):
		return httpx.New(http.StatusConflict, "already_confirmed", "That upload was already confirmed.")
	default:
		return httpx.ErrInternal.WithCause(err)
	}
}

func newProductResponse(p *Product) productResponse {
	var publishedAt *string
	if p.PublishedAt != nil {
		formatted := p.PublishedAt.UTC().Format("2006-01-02T15:04:05Z")
		publishedAt = &formatted
	}

	return productResponse{
		ID:          p.ID.String(),
		Slug:        p.Slug,
		Title:       p.Title,
		Description: p.Description,
		PriceMinor:  p.PriceMinor,
		Currency:    p.Currency,
		Status:      p.Status,
		PublishedAt: publishedAt,
	}
}

func newFileResponses(files []File) []fileResponse {
	out := make([]fileResponse, 0, len(files))
	for _, f := range files {
		out = append(out, newFileResponse(f))
	}
	return out
}

func newFileResponse(f File) fileResponse {
	return fileResponse{
		ID:           f.ID.String(),
		OriginalName: f.OriginalName,
		ContentType:  f.ContentType,
		SizeBytes:    f.SizeBytes,
		Checksum:     f.Checksum,
		Uploaded:     f.IsConfirmed(),
	}
}
