package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/config"
	"github.com/ethanhinson/fuse/internal/loopauth"
)

func loggingConfig(output, path string) config.Config {
	return config.Config{Observability: config.ObservabilityConfig{InstanceID: "instance-a", Logging: config.LoggingObservabilityConfig{Enabled: true, Output: output, File: path, Level: "info", MaxOverrideTTL: "10m"}}}
}

func observabilityVerifier() loopauth.Verifier {
	return loopauth.NewStaticVerifier(map[string]loopauth.Principal{
		"acme":     {Tenant: "acme", Subject: "alice"},
		"globex":   {Tenant: "globex", Subject: "bob"},
		"operator": {Tenant: "ops", Subject: "operator", ObservabilityOperator: true},
	})
}

func TestObservabilityDisabledIsNoop(t *testing.T) {
	s, err := newObservability(context.Background(), config.Config{}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if s.metrics != nil || s.logger != nil || s.provider != nil || s.projector != nil {
		t.Fatal("disabled observability constructed an adapter")
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestObservabilityExplicitZeroTraceSampleRatioDropsRootSpans(t *testing.T) {
	cfg := config.Config{Observability: config.ObservabilityConfig{Traces: config.TracesObservabilityConfig{
		Enabled: true, Endpoint: "127.0.0.1:1", Protocol: "grpc", Insecure: true, SampleRatio: 0,
	}}}
	s, err := newObservability(context.Background(), cfg, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	_, span := s.provider.Tracer("test").Start(context.Background(), "root")
	defer span.End()
	if span.IsRecording() {
		t.Fatal("explicit zero trace sample ratio recorded a root span")
	}
}

func TestLoggingAdminRequiresAuthenticationAndTenant(t *testing.T) {
	var out bytes.Buffer
	s, err := newObservability(context.Background(), loggingConfig("stdout", ""), &out)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	h := s.adminHandler(observabilityVerifier())

	request := func(token, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPut, observabilityAdminPath, strings.NewReader(body))
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if got := request("", `{}`).Code; got != http.StatusUnauthorized {
		t.Fatalf("unauthorized status=%d", got)
	}
	if got := request("acme", `{"scope":"tenant","tenant_id":"globex","level":"debug","ttl":"1m"}`).Code; got != http.StatusForbidden {
		t.Fatalf("cross-tenant status=%d", got)
	}
	if got := request("acme", `{"scope":"tenant","tenant_id":"acme","level":"debug","ttl":"1m"}`).Code; got != http.StatusNoContent {
		t.Fatalf("own-tenant status=%d", got)
	}
	if !strings.Contains(out.String(), `"kind":"audit"`) || !strings.Contains(out.String(), `"actor":"alice"`) {
		t.Fatalf("missing audit entry: %s", out.String())
	}
}

func TestLoggingAdminReportsInstanceAndFiltersTenant(t *testing.T) {
	s, err := newObservability(context.Background(), loggingConfig("stdout", ""), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	_ = s.levels.SetTenant("globex", 0, time.Now().Add(time.Minute))
	r := httptest.NewRequest(http.MethodGet, observabilityAdminPath, nil)
	r.Header.Set("Authorization", "Bearer acme")
	w := httptest.NewRecorder()
	s.adminHandler(observabilityVerifier()).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d", w.Code)
	}
	var body struct {
		InstanceID string `json:"instance_id"`
		State      struct {
			Overrides []json.RawMessage `json:"overrides"`
		} `json:"state"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.InstanceID != "instance-a" {
		t.Fatalf("instance=%q", body.InstanceID)
	}
	if len(body.State.Overrides) != 0 {
		t.Fatal("cross-tenant override leaked")
	}
}

func TestLoggingReloadAndReopenPreserveValidatedState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fuse.log")
	cfg := loggingConfig("file", path)
	s, err := newObservability(context.Background(), cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	invalid := cfg.Observability.Logging
	invalid.Level = "verbose"
	if err := s.Reload(context.Background(), invalid, "tester"); err == nil {
		t.Fatal("invalid replacement accepted")
	}
	if got := s.levels.Inspect().Default.String(); got != "info" {
		t.Fatalf("invalid reload published %q", got)
	}
	valid := cfg.Observability.Logging
	valid.Level = "debug"
	rotated := path + ".1"
	if err := os.Rename(path, rotated); err != nil {
		t.Fatal(err)
	}
	s.handleSIGHUP(context.Background(), valid, &bytes.Buffer{})
	if got := s.levels.Inspect().Default.String(); got != "debug" {
		t.Fatalf("SIGHUP reload level=%q", got)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("replacement file: %v", err)
	}
}

func TestLoggingReloadAndReopenRequireOperator(t *testing.T) {
	s, err := newObservability(context.Background(), loggingConfig("stdout", ""), &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })

	request := func(handler http.Handler, token string, body string) int {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w.Code
	}
	next := loggingConfig("stdout", "").Observability.Logging
	next.Level = "debug"
	body, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}

	if got := request(s.reloadHandler(observabilityVerifier()), "acme", string(body)); got != http.StatusForbidden {
		t.Fatalf("tenant reload status=%d, want %d", got, http.StatusForbidden)
	}
	if got := s.levels.Inspect().Default.String(); got != "info" {
		t.Fatalf("tenant reload mutated global level to %q", got)
	}
	if got := request(s.reopenHandler(observabilityVerifier()), "acme", ""); got != http.StatusForbidden {
		t.Fatalf("tenant reopen status=%d, want %d", got, http.StatusForbidden)
	}
	if got := request(s.reloadHandler(observabilityVerifier()), "operator", string(body)); got != http.StatusNoContent {
		t.Fatalf("operator reload status=%d, want %d", got, http.StatusNoContent)
	}
	if got := s.levels.Inspect().Default.String(); got != "debug" {
		t.Fatalf("operator reload level=%q, want debug", got)
	}
}

func TestBuildLoopVerifierPropagatesOperatorCapability(t *testing.T) {
	v, usedDefault := buildLoopVerifier(config.Config{LoopServer: config.LoopServerConfig{Auth: []config.AuthTokenConfig{
		{Token: "tenant", Tenant: "acme", Subject: "alice"},
		{Token: "operator", Tenant: "ops", Subject: "olivia", ObservabilityOperator: true},
	}}})
	if usedDefault {
		t.Fatal("configured verifier unexpectedly used default")
	}
	tenant, err := v.Verify(context.Background(), "tenant")
	if err != nil {
		t.Fatal(err)
	}
	operator, err := v.Verify(context.Background(), "operator")
	if err != nil {
		t.Fatal(err)
	}
	if tenant.ObservabilityOperator {
		t.Fatal("ordinary tenant received operator capability")
	}
	if !operator.ObservabilityOperator {
		t.Fatal("configured operator capability was not propagated")
	}
}

func TestAuthenticatedMetricsPolicy(t *testing.T) {
	v := observabilityVerifier()
	h := authenticatedHTTP(v, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	for _, tc := range []struct {
		token string
		want  int
	}{{"", 401}, {"bad", 401}, {"acme", 204}} {
		r := httptest.NewRequest(http.MethodGet, "/metrics", nil)
		if tc.token != "" {
			r.Header.Set("Authorization", "Bearer "+tc.token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Errorf("token %q: got %d want %d", tc.token, w.Code, tc.want)
		}
	}
}

func TestSeparateMetricsBindEnforcesAccessAndCloses(t *testing.T) {
	cfg := config.Config{Observability: config.ObservabilityConfig{
		Metrics:     config.MetricsObservabilityConfig{Enabled: true, Path: "/metrics", Bind: "127.0.0.1:0", Access: "authenticated"},
		Cardinality: config.CardinalityObservabilityConfig{HashVersion: "sha256-64-v1", Tenant: config.CardinalityDimensionConfig{Budget: 1}, Model: config.CardinalityDimensionConfig{Budget: 1}, Tool: config.CardinalityDimensionConfig{Budget: 1}},
	}}
	s, err := newObservability(context.Background(), cfg, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := s.startMetricsEndpoint(ctx, observabilityVerifier()); err != nil {
		t.Fatal(err)
	}
	url := "http://" + s.metricsListener.Addr().String() + "/metrics"
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	cancel()
	shutdown, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	if err := s.Close(shutdown); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(shutdown); err != nil {
		t.Fatal(err)
	}
}
