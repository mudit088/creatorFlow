package storage

import (
	"errors"
	"fmt"
	"mime"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

func errorsAs(err error, target any) bool { return errors.As(err, target) }

// unsafeFilenameChars is everything we refuse to carry into an object key.
var unsafeFilenameChars = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

// ProductFileKey decides where an object lives in the bucket.
//
// The key is server-generated and contains a fresh uuid, never the client's
// filename alone. If the client picked the key it could write to another
// product's prefix, or overwrite an existing file by reusing its key, and the
// presigned URL would faithfully authorise it. Prefixing by product id also
// means deleting a product is a prefix delete rather than a hunt.
//
// The original extension is preserved because it is harmless and makes the
// bucket browsable when something goes wrong at 2am.
func ProductFileKey(productID uuid.UUID, filename string) string {
	ext := strings.ToLower(filepath.Ext(SanitizeFilename(filename)))
	if len(ext) > 12 {
		ext = ""
	}
	return fmt.Sprintf("products/%s/%s%s", productID, uuid.NewString(), ext)
}

// SanitizeFilename strips anything that could change the meaning of a path or a
// header. "../../etc/passwd" becomes "etcpasswd"; a newline that could inject a
// second header is simply not in the allowed set.
func SanitizeFilename(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, `\`, "/"))
	name = unsafeFilenameChars.ReplaceAllString(name, "_")
	name = strings.Trim(name, "._-")
	if name == "" {
		return "download"
	}
	if len(name) > 255 {
		name = name[:255]
	}
	return name
}

// contentDisposition builds the header that names the downloaded file.
//
// The name is sanitised to a conservative ASCII set first, so FormatMediaType
// emits a plain filename parameter and quotes it correctly. The cost is that a
// creator uploading a file with non-Latin characters gets underscores in the
// saved name; carrying those through would mean the RFC 5987 filename* form,
// which is worth adding the day someone actually asks for it.
func contentDisposition(name string) string {
	safe := SanitizeFilename(name)
	return mime.FormatMediaType("attachment", map[string]string{"filename": safe})
}
