package tools

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash/crc32"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/artifact"
	"github.com/ethanhinson/fuse/internal/model"
)

func writePNG(t *testing.T, dir, name string) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p, buf.Bytes()
}

// pngHeader is a PNG signature plus a valid IHDR claiming w x h, then padding.
func pngHeader(w, h uint32, pad int) []byte {
	var b bytes.Buffer
	b.WriteString("\x89PNG\r\n\x1a\n")
	ihdr := make([]byte, 13)
	binary.BigEndian.PutUint32(ihdr[0:], w)
	binary.BigEndian.PutUint32(ihdr[4:], h)
	ihdr[8], ihdr[9] = 8, 6
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], 13)
	b.Write(l[:])
	body := append([]byte("IHDR"), ihdr...)
	b.Write(body)
	binary.BigEndian.PutUint32(l[:], crc32.ChecksumIEEE(body))
	b.Write(l[:])
	b.Write(make([]byte, pad))
	return b.Bytes()
}

func readArgsJSON(p string) string { return `{"path":"` + p + `"}` }

func TestReadFileAttachesPNG(t *testing.T) {
	p, data := writePNG(t, t.TempDir(), "sq.png")
	res := NewReadFile().Execute(WithAttachments(t.Context(), true), readArgsJSON(p))
	if res.IsError || len(res.Attachments) != 1 {
		t.Fatalf("%+v", res)
	}
	a := res.Attachments[0]
	sum := sha256.Sum256(data)
	if a.MediaType != "image/png" || a.Width != 2 || a.Height != 2 || a.SHA256 != hex.EncodeToString(sum[:]) || !bytes.Equal(a.Data, data) {
		t.Errorf("attachment = %+v", a)
	}
	want := p + ": PNG image, 2x2 pixels, " + formatSize(len(data)) + ". Attached for you to view."
	if res.Output != want {
		t.Errorf("output = %q, want %q", res.Output, want)
	}
}

func TestReadFileImageFallbackWhenNotAllowed(t *testing.T) {
	p, _ := writePNG(t, t.TempDir(), "sq.png")
	res := NewReadFile().Execute(t.Context(), readArgsJSON(p))
	if !res.IsError || len(res.Attachments) != 0 || !strings.Contains(res.Output, "cannot view images") || !strings.Contains(res.Output, "2x2 pixels") {
		t.Errorf("%+v", res)
	}
}

func TestReadFileImageTooLarge(t *testing.T) {
	p := filepath.Join(t.TempDir(), "big.png")
	os.WriteFile(p, pngHeader(10, 10, MaxAttachmentBytes), 0o644)
	res := NewReadFile().Execute(WithAttachments(t.Context(), true), readArgsJSON(p))
	if !res.IsError || !strings.Contains(res.Output, "3.5 MB") {
		t.Errorf("%+v", res)
	}
}

func TestReadFileImageTooWide(t *testing.T) {
	p := filepath.Join(t.TempDir(), "wide.png")
	os.WriteFile(p, pngHeader(9000, 10, 64), 0o644)
	res := NewReadFile().Execute(WithAttachments(t.Context(), true), readArgsJSON(p))
	if !res.IsError || !strings.Contains(res.Output, "8000 pixels") {
		t.Errorf("%+v", res)
	}
}

func TestReadFileUnsupportedImageFormat(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.bmp")
	os.WriteFile(p, append([]byte("BM"), make([]byte, 64)...), 0o644)
	res := NewReadFile().Execute(WithAttachments(t.Context(), true), readArgsJSON(p))
	if !res.IsError || !strings.Contains(res.Output, "cannot be attached") {
		t.Errorf("%+v", res)
	}
}

func TestReadFileNonImageBinaryStillRefused(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.bin")
	os.WriteFile(p, []byte("abc\x00def"), 0o644)
	res := NewReadFile().Execute(WithAttachments(t.Context(), true), readArgsJSON(p))
	if !res.IsError || !strings.Contains(res.Output, "appears to be a binary file") {
		t.Errorf("%+v", res)
	}
}

func TestReadFileSVGIsText(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x.svg")
	os.WriteFile(p, []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"></svg>`), 0o644)
	res := NewReadFile().Execute(WithAttachments(t.Context(), true), readArgsJSON(p))
	if res.IsError || len(res.Attachments) != 0 || !strings.Contains(res.Output, "<svg") {
		t.Errorf("%+v", res)
	}
}

func TestReadFileAttachmentStoredAsArtifact(t *testing.T) {
	store := artifact.NewMemStore()
	reg := NewRegistry().WithArtifacts(store, "acme", "loop1")
	reg.Register(NewReadFile())
	p, data := writePNG(t, t.TempDir(), "sq.png")
	ctx := WithAttachments(t.Context(), true)
	res := reg.Execute(ctx, "read_file", readArgsJSON(p))
	if res.IsError || len(res.Attachments) != 1 {
		t.Fatalf("%+v", res)
	}
	ref := res.Attachments[0].Ref
	if !strings.Contains(ref, "/attachment/") || !strings.Contains(res.Output, "Saved as "+ref) {
		t.Fatalf("ref = %q output = %q", ref, res.Output)
	}
	parsed, err := artifact.ParseRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	rc, _, err := store.Open(ctx, parsed)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(got, data) {
		t.Error("stored bytes differ")
	}
	again := reg.Execute(ctx, "read_file", readArgsJSON(ref))
	if again.IsError || len(again.Attachments) != 1 || again.Attachments[0].SHA256 != res.Attachments[0].SHA256 {
		t.Errorf("re-read: %+v", again)
	}
}

func TestRegistryExecutePassesAttachmentsThrough(t *testing.T) {
	reg := NewRegistry()
	reg.Register(NewReadFile())
	p, _ := writePNG(t, t.TempDir(), "sq.png")
	res := reg.Execute(WithAttachments(t.Context(), true), "read_file", readArgsJSON(p))
	if len(res.Attachments) != 1 || len(res.Attachments[0].Data) == 0 {
		t.Errorf("%+v", res)
	}
}

func TestReadFileImageMarksFileTracker(t *testing.T) {
	ft := NewFileTracker()
	reg := NewRegistry().WithFileTracker(ft)
	reg.Register(NewReadFile())
	p, _ := writePNG(t, t.TempDir(), "sq.png")
	reg.Execute(WithAttachments(t.Context(), true), "read_file", readArgsJSON(p))
	if !ft.HasState(p) || !ft.AuthorizeOverwrite(p) {
		t.Error("image read not tracked as a full read")
	}
}

func TestLoadAttachmentFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := LoadAttachmentFile(filepath.Join(dir, "none.png")); err == nil {
		t.Error("missing file: no error")
	}
	txt := filepath.Join(dir, "a.txt")
	os.WriteFile(txt, []byte("hello"), 0o644)
	if _, err := LoadAttachmentFile(txt); err == nil || !strings.Contains(err.Error(), "not a PNG") {
		t.Errorf("text file: %v", err)
	}
	big := filepath.Join(dir, "big.png")
	os.WriteFile(big, pngHeader(10, 10, MaxAttachmentBytes), 0o644)
	if _, err := LoadAttachmentFile(big); err == nil || !strings.Contains(err.Error(), "3.5 MB") {
		t.Errorf("oversize: %v", err)
	}
	ok, _ := writePNG(t, dir, "ok.png")
	att, err := LoadAttachmentFile(ok)
	if err != nil || att.Name != ok || att.Width != 2 {
		t.Errorf("ok: %+v %v", att, err)
	}
}

func TestLoadAttachmentBytes(t *testing.T) {
	_, data := writePNG(t, t.TempDir(), "sq.png")
	att, err := LoadAttachmentBytes("clipboard", data)
	if err != nil || att.Name != "clipboard" || att.MediaType != "image/png" || att.Width != 2 {
		t.Fatalf("%+v %v", att, err)
	}
	_, err = LoadAttachmentBytes("clipboard", []byte("just text"))
	var ni *NotImageError
	if !errors.As(err, &ni) {
		t.Fatalf("err = %v, want NotImageError", err)
	}
	_, err = LoadAttachmentBytes("clipboard", append(pngHeader(2, 2, 0), make([]byte, MaxAttachmentBytes)...))
	if err == nil || !strings.Contains(err.Error(), "3.5 MB") {
		t.Fatalf("err = %v", err)
	}
}

func TestNotImageErrorText(t *testing.T) {
	p := filepath.Join(t.TempDir(), "n.txt")
	os.WriteFile(p, []byte("hello"), 0o644)
	_, err := LoadAttachmentFile(p)
	want := p + " is not a PNG, JPEG, GIF or WebP image"
	if err == nil || err.Error() != want {
		t.Fatalf("err = %v, want %q", err, want)
	}
	var ni *NotImageError
	if !errors.As(err, &ni) || ni.Path != p {
		t.Fatalf("not a NotImageError: %v", err)
	}
}

func TestAttachmentSummary(t *testing.T) {
	_, data := writePNG(t, t.TempDir(), "sq.png")
	att, _ := LoadAttachmentBytes("x", data)
	if got, want := AttachmentSummary(att), "PNG 2x2, "+formatSize(len(data)); got != want {
		t.Errorf("got %q want %q", got, want)
	}
	w := model.Attachment{MediaType: "image/webp", Size: 12 << 10}
	if got := AttachmentSummary(w); got != "WebP, 12.0 KB" {
		t.Errorf("got %q", got)
	}
}
