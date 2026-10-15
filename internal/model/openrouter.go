package model

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// openrouter.go: the public OpenRouter catalogue, used by the models editor to
// show what exists upstream (the gateway only lists what its config declares).
//
// Both endpoints are public and need no key. Per-provider privacy flags are NOT
// in the catalogue; the one structured signal OpenRouter publishes is the
// zero-data-retention (ZDR) endpoint list, so that is what Privacy is built on.

// Gateway route prefixes. They name wildcard entries in the gateway's config
// (model-council's litellm_config.yaml): the first routes any OpenRouter model
// with default provider selection, the second restricts routing to
// zero-data-retention endpoints that do not collect prompts.
const (
	RouteOpenRouter = "openrouter/"
	RoutePrivate    = "private/"
	RouteLocal      = "local/"
)

// Privacy says what happens to prompts sent to a model.
type Privacy int

const (
	// PrivacyUnknown: the route does not say (an explicit gateway alias such as
	// cloud/x can point anywhere).
	PrivacyUnknown Privacy = iota
	// PrivacyLocal: runs on this machine; nothing leaves it.
	PrivacyLocal
	// PrivacyZDR: routed only to zero-data-retention endpoints.
	PrivacyZDR
	// PrivacyMayRetain: the serving provider may retain or train on prompts.
	PrivacyMayRetain
)

// PrivacyOfRoute classifies a gateway model ID by its route prefix alone.
func PrivacyOfRoute(id string) Privacy {
	switch {
	case strings.HasPrefix(id, RouteLocal):
		return PrivacyLocal
	case strings.HasPrefix(id, RoutePrivate):
		return PrivacyZDR
	case strings.HasPrefix(id, RouteOpenRouter):
		return PrivacyMayRetain
	}
	return PrivacyUnknown
}

// CatalogModel is one OpenRouter model with the fields the picker shows.
type CatalogModel struct {
	ID             string
	Name           string
	ContextLength  int
	PromptPerM     float64 // USD per million input tokens
	CompletionPerM float64 // USD per million output tokens
	// ZDR is true when at least one endpoint serving this model is
	// zero-data-retention, so the private route can serve it.
	ZDR bool
}

// GatewayID is the ID to register for this model: the private route when a
// zero-data-retention endpoint exists, the default OpenRouter route otherwise.
func (c CatalogModel) GatewayID() string {
	if c.ZDR {
		return RoutePrivate + c.ID
	}
	return RouteOpenRouter + c.ID
}

// OpenRouterBaseURL is the public API root.
const OpenRouterBaseURL = "https://openrouter.ai/api/v1"

// FetchOpenRouterCatalog downloads the model list and the ZDR endpoint list and
// merges them. Results are sorted by ID. hc may be nil.
func FetchOpenRouterCatalog(ctx context.Context, baseURL string, hc *http.Client) ([]CatalogModel, error) {
	if hc == nil {
		hc = http.DefaultClient
	}
	var models struct {
		Data []struct {
			ID            string `json:"id"`
			Name          string `json:"name"`
			ContextLength int    `json:"context_length"`
			Pricing       struct {
				Prompt     string `json:"prompt"`
				Completion string `json:"completion"`
			} `json:"pricing"`
		} `json:"data"`
	}
	if err := getJSON(ctx, hc, baseURL+"/models", &models); err != nil {
		return nil, fmt.Errorf("openrouter models: %w", err)
	}
	var zdr struct {
		Data []struct {
			ModelID string `json:"model_id"`
		} `json:"data"`
	}
	if err := getJSON(ctx, hc, baseURL+"/endpoints/zdr", &zdr); err != nil {
		return nil, fmt.Errorf("openrouter zdr list: %w", err)
	}
	zdrModels := make(map[string]bool, len(zdr.Data))
	for _, e := range zdr.Data {
		zdrModels[e.ModelID] = true
	}
	out := make([]CatalogModel, 0, len(models.Data))
	for _, m := range models.Data {
		if m.ID == "" {
			continue
		}
		out = append(out, CatalogModel{
			ID:             m.ID,
			Name:           m.Name,
			ContextLength:  m.ContextLength,
			PromptPerM:     perMillion(m.Pricing.Prompt),
			CompletionPerM: perMillion(m.Pricing.Completion),
			ZDR:            zdrModels[m.ID],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// perMillion converts OpenRouter's per-token price string to USD per million
// tokens. Unparseable or negative (dynamic-priced) values come back as 0.
func perMillion(perToken string) float64 {
	v, err := strconv.ParseFloat(perToken, 64)
	if err != nil || v < 0 {
		return 0
	}
	return v * 1e6
}

func getJSON(ctx context.Context, hc *http.Client, url string, into any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	res, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return err
	}
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", res.StatusCode, strings.TrimSpace(string(raw[:min(len(raw), 200)])))
	}
	return json.Unmarshal(raw, into)
}
