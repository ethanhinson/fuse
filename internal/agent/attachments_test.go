package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ethanhinson/fuse/internal/event"
	"github.com/ethanhinson/fuse/internal/model"
	"github.com/ethanhinson/fuse/internal/segment"
	"github.com/ethanhinson/fuse/internal/tools"
)

func attPNG(t *testing.T) (string, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "sq.png")
	if err := os.WriteFile(p, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	return p, buf.Bytes()
}

// reqRecorder scripts a read_file call, then a final answer, keeping each request.
type reqRecorder struct {
	path string
	reqs []model.CompletionReq
}

func (r *reqRecorder) Complete(_ context.Context, req model.CompletionReq) (model.CompletionResp, error) {
	r.reqs = append(r.reqs, req)
	if len(r.reqs) == 1 {
		return model.CompletionResp{ToolCalls: []model.ToolCall{{ID: "c1", Name: "read_file", Arguments: `{"path":"` + r.path + `"}`}}}, nil
	}
	return model.CompletionResp{Content: "done"}, nil
}

func runImageRead(t *testing.T, accepts bool) (*reqRecorder, *recordingStore, []byte) {
	t.Helper()
	p, data := attPNG(t)
	reg := tools.NewRegistry()
	reg.Register(tools.NewReadFile())
	comp := &reqRecorder{path: p}
	rec := &recordingStore{}
	a := New(comp, reg, nopRenderer{}, "m", "", 10, 100)
	a.Attachments = accepts
	a.SetEventSink(rec)
	if _, err := a.Run(context.Background(), []model.Message{{Role: "user", Content: "look"}}); err != nil {
		t.Fatal(err)
	}
	if len(comp.reqs) != 2 {
		t.Fatalf("requests = %d", len(comp.reqs))
	}
	return comp, rec, data
}

func lastTool(req model.CompletionReq) model.Message {
	for i := len(req.Messages) - 1; i >= 0; i-- {
		if req.Messages[i].Role == "tool" {
			return req.Messages[i]
		}
	}
	return model.Message{}
}

func TestToolAttachmentEntersHistory(t *testing.T) {
	comp, rec, data := runImageRead(t, true)
	req := comp.reqs[1]
	if !req.Attachments {
		t.Error("CompletionReq.Attachments = false")
	}
	tm := lastTool(req)
	if len(tm.Attachments) != 1 || !bytes.Equal(tm.Attachments[0].Data, data) {
		t.Fatalf("tool message attachments = %+v", tm.Attachments)
	}
	e, ok := rec.first(event.KindToolResult)
	if !ok {
		t.Fatal("no tool.result event")
	}
	if strings.Contains(string(e.Payload), base64.StdEncoding.EncodeToString(data)[:20]) {
		t.Error("event carries base64")
	}
	var pl map[string]any
	json.Unmarshal(e.Payload, &pl)
	atts, _ := pl["attachments"].([]any)
	if len(atts) != 1 || atts[0].(map[string]any)["sha256"] == "" {
		t.Errorf("payload = %s", e.Payload)
	}
}

func TestNoAttachmentsWhenModelCannotSee(t *testing.T) {
	comp, _, _ := runImageRead(t, false)
	req := comp.reqs[1]
	tm := lastTool(req)
	if req.Attachments || len(tm.Attachments) != 0 || !strings.Contains(tm.Content, "cannot view images") {
		t.Errorf("req.Attachments=%v tool=%+v", req.Attachments, tm)
	}
}

func atts(n int, withRef bool) []model.Message {
	var msgs []model.Message
	for i := 0; i < n; i++ {
		a := model.Attachment{Name: "f" + string(rune('0'+i)) + ".png", Data: []byte{1}}
		if withRef {
			a.Ref = "art://t/l/attachment/x" + string(rune('0'+i))
		}
		msgs = append(msgs, model.Message{Role: "tool", Content: "r", Attachments: []model.Attachment{a}})
	}
	return msgs
}

func TestEvictOldAttachmentsKeepsNewestFour(t *testing.T) {
	msgs := atts(6, false)
	msgs[1].Attachments[0].Ref = "art://t/l/attachment/x1"
	if n := evictOldAttachments(msgs, 4); n != 2 {
		t.Fatalf("removed %d", n)
	}
	for i, m := range msgs {
		if (i < 2) != (len(m.Attachments) == 0) {
			t.Errorf("msg %d attachments = %d", i, len(m.Attachments))
		}
	}
	if !strings.Contains(msgs[0].Content, "f0.png") || !strings.Contains(msgs[1].Content, "art://t/l/attachment/x1") {
		t.Errorf("stubs: %q / %q", msgs[0].Content, msgs[1].Content)
	}
	if n := evictOldAttachments(msgs, 4); n != 0 {
		t.Errorf("second call removed %d", n)
	}
}

func TestEvictSplitsWithinMessage(t *testing.T) {
	m := model.Message{Role: "user", Content: "x", Attachments: []model.Attachment{{Name: "a"}, {Name: "b"}, {Name: "c"}}}
	if n := evictOldAttachments([]model.Message{m}, 2); n != 1 {
		t.Fatalf("removed %d", n)
	}
}

func TestPruneClearsAttachments(t *testing.T) {
	big := strings.Repeat("x", 8000)
	msgs := []model.Message{
		{Role: "user", Content: "task"},
		{Role: "tool", ToolCallID: "1", Name: "read_file", Content: "img", Attachments: []model.Attachment{{Name: "a.png", Data: []byte{1}}}},
		{Role: "tool", ToolCallID: "2", Name: "read_file", Content: big},
		{Role: "tool", ToolCallID: "3", Name: "read_file", Content: big},
	}
	pruneOldToolResults(msgs, 1000, nil, 100)
	if msgs[1].Content != prunedStub || msgs[1].Attachments != nil {
		t.Errorf("msg1 = %+v", msgs[1])
	}
}

func TestMessagesSizeCountsAttachments(t *testing.T) {
	m := model.Message{Role: "tool", Content: "abcd", Attachments: []model.Attachment{{Data: []byte{1}}, {Data: []byte{1}}}}
	if got, want := messagesSize([]model.Message{m}), 4+2*model.AttachmentTokenEstimate*bytesPerToken; got != want {
		t.Errorf("messagesSize = %d, want %d", got, want)
	}
	if got, want := toolResultTokens(m), 1+2*model.AttachmentTokenEstimate; got != want {
		t.Errorf("toolResultTokens = %d, want %d", got, want)
	}
	// An image-bearing result is counted by prune and by isProtected alike.
	msgs := []model.Message{m, {Role: "tool", Content: strings.Repeat("x", 40)}}
	if !isProtected(msgs, 1, 5) {
		t.Error("newest result should be protected")
	}
	if isProtected(msgs, 0, 5) {
		t.Error("image result should have spent the protected budget")
	}
}

func TestSeedAttachmentsInUserInputEvent(t *testing.T) {
	rec := &recordingStore{}
	a := New(&scriptedCompleter{responses: []model.CompletionResp{{Content: "ok"}}}, &fakeExec{}, nopRenderer{}, "m", "", 10, 100)
	a.SetEventSink(rec)
	seed := model.Message{Role: "user", Content: "look", Attachments: []model.Attachment{{Name: "a.png", MediaType: "image/png", Data: []byte("RAWBYTES"), Size: 8, SHA256: "ab"}}}
	if _, err := a.Run(context.Background(), []model.Message{seed}); err != nil {
		t.Fatal(err)
	}
	e, _ := rec.first(event.KindUserInput)
	var pl event.UserInputPayload
	json.Unmarshal(e.Payload, &pl)
	if len(pl.Attachments) != 1 || pl.Attachments[0].SHA256 != "ab" || strings.Contains(string(e.Payload), "UkFXQllURVM") {
		t.Errorf("payload = %s", e.Payload)
	}
}

func TestSegmentRenderExcludesAttachmentBytes(t *testing.T) {
	data := bytes.Repeat([]byte{0xfa}, 64)
	out := segment.RenderRawRegion([]model.Message{{Role: "tool", Name: "read_file", Content: "sq.png: PNG image. Attached.",
		Attachments: []model.Attachment{{Name: "sq.png", Data: data}}}})
	if !strings.Contains(out, "sq.png") || strings.Contains(out, base64.StdEncoding.EncodeToString(data)[:16]) || strings.Contains(out, string(data[:8])) {
		t.Errorf("render = %q", out)
	}
}
