// Package rentals is a genuinely real Wander-domain MCP server: a vacation-rental
// "rentals" server fuse's client talks to over the real MCP wire, with real
// per-principal token adjudication and real per-principal mutable state. It exists to
// ground change #59's acceptance lane in an actual service scenario (a concierge loop
// querying a rentals server AS the authenticated user) and is importable by the Wander
// demo (#60), which swaps only the read tool's data source behind the DataSource seam.
//
// It is real on three axes: the MCP wire + handshake (initialize → tools/list →
// tools/call over HTTP/SSE), per-principal token adjudication (it verifies the
// delegated HS256 token, extracts sub/aud, 403s an unauthorized principal, rejects a
// wrong audience), and per-principal state isolation (favorite_listing writes into the
// CALLING principal's favorites, keyed by the token identity — never a client-supplied
// arg — so tenant A's writes are invisible to tenant B). Only the read tool's listing
// DATA is canned (DataSource); the live Tavily backend is #60, out of scope here.
package rentals

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"

	"github.com/ethanhinson/fuse/internal/event"
)

// Listing is one rental listing returned by the read tool.
type Listing struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	City  string `json:"city"`
	Price int    `json:"price_per_night"`
}

// DataSource is the read-tool backend seam. The default (CannedData) returns fixed,
// deterministic in-repo listings — hermetic, no network, no key — so the acceptance
// lane is green-able forever. The live Tavily-style backend (#60) implements this same
// interface and is swapped in for the runnable Wander demo, not the CI lane.
type DataSource interface {
	Search(query string) []Listing
}

// CannedData is the deterministic in-repo backend (the permanent CI lane default).
type CannedData struct{}

// Search returns a fixed set of listings, optionally narrowed by a case-insensitive
// substring match on city/title so a query still meaningfully filters.
func (CannedData) Search(query string) []Listing {
	all := []Listing{
		{ID: "L1", Title: "Beach Cottage", City: "Santa Cruz", Price: 220},
		{ID: "L2", Title: "Mountain Cabin", City: "Tahoe", Price: 180},
		{ID: "L3", Title: "City Loft", City: "San Francisco", Price: 300},
	}
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		return all
	}
	var out []Listing
	for _, l := range all {
		if strings.Contains(strings.ToLower(l.City), q) || strings.Contains(strings.ToLower(l.Title), q) {
			out = append(out, l)
		}
	}
	return out
}

// principalKey is the per-principal favorites-store key: the token identity
// (tenant + subject), NEVER a client-supplied argument. It is the confused-deputy
// boundary — a write always lands in the CALLING principal's set.
type principalKey struct {
	tenant  event.TenantID
	subject string
}

// Server is the rentals MCP HTTP/SSE server. It owns a per-principal favorites store
// (reset per NewServer, i.e. per test run) and adjudicates every tools/call by the
// delegated token's identity.
type Server struct {
	httptest *httptest.Server
	data     DataSource
	audience string // the audience this server accepts (RFC 8707 resource id)

	// tenantKeys is the per-tenant HS256 verification key map — the SAME keys fuse's
	// built-in STS mints under, shared out of band (as a real resource server shares a
	// verification key). A token is authorized under the tenant whose key verifies it.
	tenantKeys map[event.TenantID][]byte

	mu        sync.Mutex
	favs      map[principalKey]map[string]bool // principal -> set of listing ids
	sseConns  []chan string
	authSeen  []string // Authorization header per tools/call, in arrival order
	resources []string // MCP-Resource header per tools/call, in arrival order
}

// Config configures a rentals Server.
type Config struct {
	// Data is the read-tool backend; nil ⇒ CannedData (the CI-lane default).
	Data DataSource
	// Audience is the RFC 8707 resource id this server accepts; a token whose bound
	// audience (MCP-Resource header) differs is rejected (wrong-audience).
	Audience string
	// TenantKeys are the per-tenant HS256 verification keys (the STS's per-tenant keys,
	// shared out of band). A caller under a tenant with no key here is unauthorized.
	TenantKeys map[event.TenantID][]byte
}

// NewServer starts the rentals MCP server on an httptest.Server and returns it. The
// favorites store starts empty (reset per call). Call Close() to stop it.
func NewServer(cfg Config) *Server {
	s := &Server{
		data:       cfg.Data,
		audience:   cfg.Audience,
		tenantKeys: map[event.TenantID][]byte{},
		favs:       map[principalKey]map[string]bool{},
	}
	if s.data == nil {
		s.data = CannedData{}
	}
	for k, v := range cfg.TenantKeys {
		kc := make([]byte, len(v))
		copy(kc, v)
		s.tenantKeys[event.NormalizeTenant(k)] = kc
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", s.handleSSE)
	mux.HandleFunc("/messages", s.handleMessages)
	s.httptest = httptest.NewServer(mux)
	return s
}

// URL returns the base URL an MCP client dials (the /sse + /messages routes live
// under it).
func (s *Server) URL() string { return s.httptest.URL }

// Close stops the server.
func (s *Server) Close() { s.httptest.Close() }

// CapturedAuths returns the Authorization header of every tools/call received, in
// arrival order — so an acceptance test can assert per-principal tokens are distinct
// and Bearer-scheme (spec checklist point 2), read from the REAL wire.
func (s *Server) CapturedAuths() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.authSeen...)
}

// CapturedResources returns the MCP-Resource (RFC 8707 audience) header of every
// tools/call received, in arrival order.
func (s *Server) CapturedResources() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.resources...)
}

// handleSSE holds one long-lived SSE stream per client, announcing the /messages
// endpoint then relaying JSON-RPC responses.
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flush", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	ch := make(chan string, 16)
	s.mu.Lock()
	s.sseConns = append(s.sseConns, ch)
	s.mu.Unlock()
	fmt.Fprintf(w, "event: endpoint\ndata: http://%s/messages\n\n", r.Host)
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case msg := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		}
	}
}

// jsonrpcErr is the error object surfaced to the client. Code -32001 is used for an
// authorization denial so MCPTool.Execute surfaces it as a distinguishable tool error.
type jsonrpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// CodeUnauthorized is the JSON-RPC error code the server returns for an authorization
// denial (unknown/unauthorized principal, wrong audience). It surfaces to the loop as
// a distinguishable "[code -32001] ..." tool error rather than a generic failure.
const CodeUnauthorized = -32001

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	var result any
	var rpcErr *jsonrpcErr
	switch req.Method {
	case "initialize":
		result = map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{},
			"serverInfo":      map[string]any{"name": "wander-rentals", "version": "1.0.0"},
		}
	case "tools/list":
		result = map[string]any{"tools": toolDefs()}
	case "tools/call":
		// Record the raw credential + audience that arrived on the wire so a test can
		// assert per-principal distinctness (point 2) directly from the server side.
		s.mu.Lock()
		s.authSeen = append(s.authSeen, r.Header.Get("Authorization"))
		s.resources = append(s.resources, r.Header.Get("MCP-Resource"))
		s.mu.Unlock()
		result, rpcErr = s.handleToolCall(r, req.Params)
	default:
		// notifications/initialized and other notifications: 202, no reply frame.
		w.WriteHeader(http.StatusAccepted)
		return
	}

	resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
	if rpcErr != nil {
		resp["error"] = rpcErr
	} else {
		resp["result"] = result
	}
	b, _ := json.Marshal(resp)
	s.mu.Lock()
	conns := append([]chan string(nil), s.sseConns...)
	s.mu.Unlock()
	for _, c := range conns {
		select {
		case c <- string(b):
		default:
		}
	}
	w.WriteHeader(http.StatusAccepted)
}

// handleToolCall adjudicates by the delegated token identity, then dispatches. It
// returns either a result envelope or a jsonrpcErr (authorization denial).
func (s *Server) handleToolCall(r *http.Request, rawParams json.RawMessage) (any, *jsonrpcErr) {
	pk, aerr := s.authorize(r)
	if aerr != nil {
		return nil, aerr
	}

	var params struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	_ = json.Unmarshal(rawParams, &params)

	switch params.Name {
	case "search_rentals":
		var args struct {
			Query string `json:"query"`
		}
		_ = json.Unmarshal(params.Arguments, &args)
		listings := s.data.Search(args.Query)
		return textResult(mustJSON(listings)), nil
	case "favorite_listing":
		var args struct {
			ListingID string `json:"listing_id"`
		}
		_ = json.Unmarshal(params.Arguments, &args)
		if args.ListingID == "" {
			return errResult("favorite_listing requires a listing_id"), nil
		}
		s.addFavorite(pk, args.ListingID)
		return textResult(fmt.Sprintf("favorited %s", args.ListingID)), nil
	case "list_favorites":
		return textResult(mustJSON(s.listFavorites(pk))), nil
	default:
		return errResult(fmt.Sprintf("unknown tool %q", params.Name)), nil
	}
}

// authorize verifies the delegated token and returns the CALLING principal's store
// key (tenant + sub). It fails closed: missing/invalid token, unknown tenant, or a
// wrong-audience binding are all authorization denials (CodeUnauthorized).
func (s *Server) authorize(r *http.Request) (principalKey, *jsonrpcErr) {
	auth := r.Header.Get("Authorization")
	const pfx = "Bearer "
	if len(auth) <= len(pfx) || !strings.EqualFold(auth[:len(pfx)], pfx) {
		return principalKey{}, &jsonrpcErr{Code: CodeUnauthorized, Message: "missing bearer credential"}
	}
	token := strings.TrimSpace(auth[len(pfx):])

	// Wrong-audience rejection: the token is bound (RFC 8707) to the resource in the
	// MCP-Resource header; it must match this server's audience.
	if s.audience != "" {
		if res := r.Header.Get("MCP-Resource"); res != s.audience {
			return principalKey{}, &jsonrpcErr{Code: CodeUnauthorized, Message: fmt.Sprintf("wrong audience: token bound to %q, server is %q", res, s.audience)}
		}
	}

	// Find the tenant whose key verifies this token (per-tenant credential isolation:
	// a token minted under tenant A never verifies under tenant B's key).
	tenant, claims, ok := s.verify(token)
	if !ok {
		return principalKey{}, &jsonrpcErr{Code: CodeUnauthorized, Message: "token does not verify under any known tenant key"}
	}
	if claims.Subject == "" {
		return principalKey{}, &jsonrpcErr{Code: CodeUnauthorized, Message: "token carries no subject"}
	}
	// Audience claim must also match (defense in depth alongside the header check).
	if s.audience != "" && claims.Audience != s.audience {
		return principalKey{}, &jsonrpcErr{Code: CodeUnauthorized, Message: fmt.Sprintf("token aud %q != server audience %q", claims.Audience, s.audience)}
	}
	return principalKey{tenant: tenant, subject: claims.Subject}, nil
}

// tokenClaims is the subset of the delegation token this server adjudicates on.
type tokenClaims struct {
	Subject  string `json:"sub"`
	Audience string `json:"aud"`
}

// verify tries each known tenant key against the compact HS256 JWS. It returns the
// tenant whose key matches, the decoded claims, and ok. A token that verifies under no
// key is unauthorized.
func (s *Server) verify(token string) (event.TenantID, tokenClaims, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", tokenClaims{}, false
	}
	signingInput := parts[0] + "." + parts[1]
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return "", tokenClaims{}, false
	}
	for tenant, key := range s.tenantKeys {
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(signingInput))
		if subtle.ConstantTimeCompare(mac.Sum(nil), sig) == 1 {
			payload, perr := base64.RawURLEncoding.DecodeString(parts[1])
			if perr != nil {
				return "", tokenClaims{}, false
			}
			var claims tokenClaims
			if json.Unmarshal(payload, &claims) != nil {
				return "", tokenClaims{}, false
			}
			return tenant, claims, true
		}
	}
	return "", tokenClaims{}, false
}

func (s *Server) addFavorite(pk principalKey, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := s.favs[pk]
	if set == nil {
		set = map[string]bool{}
		s.favs[pk] = set
	}
	set[id] = true // idempotent per principal
}

func (s *Server) listFavorites(pk principalKey) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	set := s.favs[pk]
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	return out
}

// toolDefs is the advertised tool surface.
func toolDefs() []map[string]any {
	obj := func(props map[string]any) map[string]any {
		return map[string]any{"type": "object", "properties": props}
	}
	return []map[string]any{
		{"name": "search_rentals", "description": "search rental listings", "inputSchema": obj(map[string]any{"query": map[string]any{"type": "string"}})},
		{"name": "favorite_listing", "description": "favorite a listing for the calling user", "inputSchema": obj(map[string]any{"listing_id": map[string]any{"type": "string"}})},
		{"name": "list_favorites", "description": "list the calling user's favorites", "inputSchema": obj(map[string]any{})},
	}
}

func textResult(text string) map[string]any {
	return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}
}

func errResult(text string) map[string]any {
	return map[string]any{"isError": true, "content": []map[string]any{{"type": "text", "text": text}}}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "[]"
	}
	return string(b)
}
