package model

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchOpenRouterCatalogMergesZDR(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			io.WriteString(w, `{"data":[
			 {"id":"b/open","name":"Open","context_length":8000,"pricing":{"prompt":"0.000001","completion":"0.000002"}},
			 {"id":"a/private","name":"Priv","context_length":1000000,"pricing":{"prompt":"-1","completion":"x"}}]}`)
		case "/endpoints/zdr":
			io.WriteString(w, `{"data":[{"model_id":"a/private"},{"model_id":"a/private"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	got, err := FetchOpenRouterCatalog(context.Background(), srv.URL, srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "a/private" || got[1].ID != "b/open" {
		t.Fatalf("not sorted/complete: %+v", got)
	}
	if !got[0].ZDR || got[1].ZDR {
		t.Errorf("zdr flags wrong: %+v", got)
	}
	if got[0].GatewayID() != "private/a/private" || got[1].GatewayID() != "openrouter/b/open" {
		t.Errorf("gateway ids: %q %q", got[0].GatewayID(), got[1].GatewayID())
	}
	if got[1].PromptPerM != 1 || got[1].CompletionPerM != 2 {
		t.Errorf("price per million: %+v", got[1])
	}
	if got[0].PromptPerM != 0 || got[0].CompletionPerM != 0 {
		t.Errorf("bad prices should be 0: %+v", got[0])
	}
}

func TestFetchOpenRouterCatalogErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer srv.Close()
	if _, err := FetchOpenRouterCatalog(context.Background(), srv.URL, srv.Client()); err == nil {
		t.Fatal("want error")
	}
}

func TestPrivacyOfRoute(t *testing.T) {
	cases := map[string]Privacy{
		"local/qwen-7b": PrivacyLocal, "private/x/y": PrivacyZDR,
		"openrouter/x/y": PrivacyMayRetain, "cloud/glm-5.2": PrivacyUnknown,
	}
	for id, want := range cases {
		if got := PrivacyOfRoute(id); got != want {
			t.Errorf("%s: %v want %v", id, got, want)
		}
	}
}
