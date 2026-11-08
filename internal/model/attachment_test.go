package model

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/png"
	"reflect"
	"strings"
	"testing"
)

func fixturePNG(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

const sqText = "sq.png: PNG image, 2x2 pixels, 75 B. Attached for you to view."

func attachHistory(t *testing.T) []Message {
	return []Message{
		{Role: "system", Content: "be useful"},
		{Role: "user", Content: "look"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "call_1", Name: "read_file", Arguments: `{"path":"sq.png"}`}}},
		{Role: "tool", ToolCallID: "call_1", Name: "read_file", Content: sqText,
			Attachments: []Attachment{{MediaType: "image/png", Data: fixturePNG(t), Name: "sq.png"}}},
	}
}

func imagePart(data []byte) map[string]any {
	return map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(data)}}
}

func TestAttachmentToolMessageWireShape(t *testing.T) {
	msgs := sent(t, CompletionReq{Model: "cloud/deepseek-v4-flash", Dialect: DialectOpenAI, Attachments: true, Messages: attachHistory(t)})
	want := map[string]any{"role": "tool", "tool_call_id": "call_1", "name": "read_file",
		"content": []any{map[string]any{"type": "text", "text": sqText}, imagePart(fixturePNG(t))}}
	if !reflect.DeepEqual(msgs[3], want) {
		t.Errorf("tool message = %v\nwant %v", msgs[3], want)
	}
	for _, i := range []int{0, 1, 2} {
		if _, ok := msgs[i].(map[string]any)["content"].(string); !ok {
			t.Errorf("message %d content is not a plain string", i)
		}
	}
	if b, _ := jsonOf(msgs); strings.Contains(b, "cache_control") {
		t.Error("cache_control present")
	}
}

func TestAttachmentUserMessageWireShape(t *testing.T) {
	msgs := sent(t, CompletionReq{Model: "cloud/deepseek-v4-flash", Dialect: DialectOpenAI, Attachments: true, Messages: []Message{
		{Role: "user", Content: "what colour", Attachments: []Attachment{{MediaType: "image/png", Data: fixturePNG(t), Name: "sq.png"}}},
	}})
	want := map[string]any{"role": "user",
		"content": []any{map[string]any{"type": "text", "text": "what colour"}, imagePart(fixturePNG(t))}}
	if !reflect.DeepEqual(msgs[0], want) {
		t.Errorf("user message = %v\nwant %v", msgs[0], want)
	}
}

func TestAnthropicMarksLastPartOfAttachmentMessage(t *testing.T) {
	msgs := sent(t, CompletionReq{Model: "cloud/claude-haiku-5.5", Attachments: true, Messages: attachHistory(t)})
	parts := msgs[3].(map[string]any)["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("parts = %v", parts)
	}
	if _, ok := parts[0].(map[string]any)["cache_control"]; ok {
		t.Error("text part marked")
	}
	cc, _ := parts[1].(map[string]any)["cache_control"].(map[string]any)
	if cc["type"] != "ephemeral" {
		t.Errorf("image part not marked: %v", parts[1])
	}
	sys := msgs[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if sys["cache_control"] == nil {
		t.Error("system not marked")
	}
}

func TestAttachmentsBecomeTextWhenModelRefusesThem(t *testing.T) {
	msgs := sent(t, CompletionReq{Model: "cloud/deepseek-v4-flash", Attachments: false, Messages: attachHistory(t)})
	got, ok := msgs[3].(map[string]any)["content"].(string)
	if !ok || !strings.HasSuffix(got, "[image sq.png not sent: this model does not accept attachments]") {
		t.Errorf("content = %v", msgs[3])
	}
	if b, _ := jsonOf(msgs); strings.Contains(b, "base64,") {
		t.Error("base64 sent to a text-only model")
	}
}

func TestAttachmentWithoutDataNotSent(t *testing.T) {
	h := attachHistory(t)
	h[3].Attachments[0].Data = nil
	msgs := sent(t, CompletionReq{Model: "cloud/deepseek-v4-flash", Attachments: true, Messages: h})
	got, ok := msgs[3].(map[string]any)["content"].(string)
	if !ok || !strings.HasSuffix(got, "[image sq.png not available]") {
		t.Errorf("content = %v", msgs[3])
	}
}

func TestTraceRedactsAttachmentBytes(t *testing.T) {
	var bodies []map[string]any
	srv := fixedServer(t, &bodies, true, sse(`{"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}`))
	var trace bytes.Buffer
	_, err := NewAdapter(srv.URL, "k", nil).WithTrace(&trace).Complete(context.Background(),
		CompletionReq{Model: "cloud/claude-haiku-5.5", Attachments: true, Messages: attachHistory(t)})
	if err != nil {
		t.Fatal(err)
	}
	b64 := base64.StdEncoding.EncodeToString(fixturePNG(t))
	if !strings.Contains(trace.String(), "data:image/png;base64,<elided ") || strings.Contains(trace.String(), b64) {
		t.Errorf("trace not redacted:\n%s", trace.String())
	}
	parts := bodies[0]["messages"].([]any)[3].(map[string]any)["content"].([]any)
	if !strings.Contains(parts[1].(map[string]any)["image_url"].(map[string]any)["url"].(string), b64) {
		t.Error("server did not receive the full base64")
	}
}

func TestRateGateEstimateExcludesBase64(t *testing.T) {
	srv := usageServer(t, 10, 5)
	g := &fakeGate{}
	big := bytes.Repeat([]byte{0x7f}, 1<<20)
	h := []Message{{Role: "user", Content: "look", Attachments: []Attachment{{MediaType: "image/png", Data: big, Name: "big.png"}}}}
	a := NewAdapter(srv.URL, "k", srv.Client()).WithRateGate(g)
	if _, err := a.Complete(context.Background(), CompletionReq{Model: "m", Attachments: true, Messages: h}); err != nil {
		t.Fatal(err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if max := 100/4 + AttachmentTokenEstimate + 100; g.waitTokens > max || g.waitTokens < AttachmentTokenEstimate {
		t.Errorf("estTokens = %d, want within [%d, %d]", g.waitTokens, AttachmentTokenEstimate, max)
	}
}

func TestAcceptsAttachments(t *testing.T) {
	yes, no := true, false
	for _, c := range []struct {
		name string
		mc   ModelConfig
		want bool
	}{
		{"haiku", ModelConfig{ID: "cloud/claude-haiku-5.5"}, true},
		{"vision prefix alone is not a signal", ModelConfig{ID: "vision/kimi-k3"}, false},
		{"vision prefix tagged", ModelConfig{ID: "vision/kimi-k3", Tags: []string{"vision"}}, true},
		{"deepseek", ModelConfig{ID: "cloud/deepseek-v4-flash"}, false},
		{"deepseek tagged vision", ModelConfig{ID: "cloud/deepseek-v4-flash", Tags: []string{"vision"}}, true},
		{"deepseek explicit", ModelConfig{ID: "cloud/deepseek-v4-flash", Attachments: &yes}, true},
		{"haiku explicit off", ModelConfig{ID: "cloud/claude-haiku-5.5", Attachments: &no}, false},
		{"cli never", ModelConfig{ID: "cli/claude-max", Attachments: &yes}, false},
		{"anthropic dialect", ModelConfig{ID: "cloud/foo", Dialect: DialectAnthropic}, true},
	} {
		if got := c.mc.AcceptsAttachments(); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func jsonOf(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}
