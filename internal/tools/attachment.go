package tools

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"image"
	_ "image/gif"  // register the GIF decoder for image.DecodeConfig
	_ "image/jpeg" // register the JPEG decoder for image.DecodeConfig
	_ "image/png"  // register the PNG decoder for image.DecodeConfig
	"io"
	"net/http"
	"os"
	"time"

	"github.com/ethanhinson/fuse/internal/artifact"
	"github.com/ethanhinson/fuse/internal/model"
)

const (
	// MaxAttachmentBytes is 3.5 MiB of raw bytes, so the base64 form stays under
	// Anthropic's 5 MiB per-image cap.
	MaxAttachmentBytes = 7 << 19
	// MaxAttachmentSide is Anthropic's hard per-side limit in pixels.
	MaxAttachmentSide = 8000
	attachPutTimeout  = 10 * time.Second

	shrinkHint = "Shrink it with bash (e.g. ImageMagick: convert IN -resize 1568x1568 OUT.png, or python3 with PIL), then read_file the smaller file."
)

type attachmentsKey struct{}

// WithAttachments reports on ctx whether the calling model accepts image
// attachments. Without it, tools behave as for a text-only model.
func WithAttachments(ctx context.Context, ok bool) context.Context {
	return context.WithValue(ctx, attachmentsKey{}, ok)
}

// AttachmentsAllowed reports whether the model behind ctx accepts attachments;
// false when unset.
func AttachmentsAllowed(ctx context.Context) bool {
	ok, _ := ctx.Value(attachmentsKey{}).(bool)
	return ok
}

// imageType sniffs head (never the extension). mime is "" when head is not an
// image at all; otherwise it is the sniffed type and supported says whether fuse
// can attach that format (PNG, JPEG, GIF, WebP).
func imageType(head []byte) (mime string, supported bool) {
	if len(head) > 512 {
		head = head[:512]
	}
	t := http.DetectContentType(head)
	switch t {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return t, true
	}
	if len(t) > 6 && t[:6] == "image/" {
		return t, false
	}
	return "", false
}

// imageKind is the display word for a supported media type.
func imageKind(mime string) string {
	switch mime {
	case "image/png":
		return "PNG"
	case "image/jpeg":
		return "JPEG"
	case "image/gif":
		return "GIF"
	case "image/webp":
		return "WebP"
	}
	return mime
}

func unsupportedImageErr(name, mime string) error {
	return fmt.Errorf("%s is a %s image, which cannot be attached. Convert it to PNG with bash first.", name, mime)
}

// LoadAttachment reads one image of media type mime from r. name is used in
// messages. The read is capped, so an endless source cannot be slurped.
func LoadAttachment(name string, r io.Reader, mime string) (model.Attachment, error) {
	if _, ok := imageSupported(mime); !ok {
		return model.Attachment{}, unsupportedImageErr(name, mime)
	}
	data, err := io.ReadAll(io.LimitReader(r, MaxAttachmentBytes+1))
	if err != nil {
		return model.Attachment{}, fmt.Errorf("%s: %v", name, err)
	}
	if len(data) > MaxAttachmentBytes {
		// Not read further just to report a size: the source may be huge or endless.
		return model.Attachment{}, fmt.Errorf("%s is larger than 3.5 MB, the limit for attachments. %s", name, shrinkHint)
	}
	att := model.Attachment{MediaType: mime, Data: data, Name: name, Size: len(data)}
	sum := sha256.Sum256(data)
	att.SHA256 = hex.EncodeToString(sum[:])
	if mime != "image/webp" { // no stdlib WebP decoder: the side check is skipped
		cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			return model.Attachment{}, fmt.Errorf("%s: cannot read the image header: %v", name, err)
		}
		if cfg.Width > MaxAttachmentSide || cfg.Height > MaxAttachmentSide {
			return model.Attachment{}, fmt.Errorf("%s is %dx%d pixels; attachments are limited to 8000 pixels per side. %s",
				name, cfg.Width, cfg.Height, shrinkHint)
		}
		att.Width, att.Height = cfg.Width, cfg.Height
	}
	return att, nil
}

func imageSupported(mime string) (string, bool) {
	switch mime {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
		return mime, true
	}
	return mime, false
}

// LoadAttachmentFile opens path, sniffs it and loads it as an attachment. It is
// what --attach uses.
func LoadAttachmentFile(path string) (model.Attachment, error) {
	f, err := os.Open(path)
	if err != nil {
		return model.Attachment{}, err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 4096)
	head, err := br.Peek(512)
	if err != nil && err != io.EOF {
		return model.Attachment{}, fmt.Errorf("%s: %v", path, err)
	}
	mime, _ := imageType(head)
	if mime == "" {
		return model.Attachment{}, fmt.Errorf("%s is not a PNG, JPEG, GIF or WebP image", path)
	}
	return LoadAttachment(path, br, mime)
}

// storeAttachment puts att.Data in the loop's artifact store so an evicted image
// can be reopened by ref, and sets att.Ref on success. A failure leaves Ref empty:
// the attachment still goes to the model, it just cannot be reopened by ref.
func storeAttachment(ctx context.Context, b artifact.Binding, att *model.Attachment) {
	if !b.Valid() {
		return
	}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), attachPutTimeout)
	defer cancel()
	meta, err := b.Put(pctx, artifact.KindAttachment, bytes.NewReader(att.Data), artifact.Meta{Tool: "read_file"})
	if err != nil {
		return
	}
	att.Ref = meta.Ref.String()
}

// formatSize renders a byte count for the model.
func formatSize(n int) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
}
