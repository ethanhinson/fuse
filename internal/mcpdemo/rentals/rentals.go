// Package rentals is a genuinely real Wander-domain MCP server: a vacation-rental
// "rentals" server fuse's client talks to over the real MCP wire, with real
// per-principal token adjudication and real per-principal mutable state. It exists to
// ground change #59's acceptance lane in an actual service scenario (a concierge loop
// querying a rentals server AS the authenticated user), and change #60 shipped it as the
// runnable Wander demo's backend: cmd/rentals-mcp serves this package's NewHandler on a
// real port, and examples/wander drives it. The demo swaps only the read tool's data
// source (LiveData behind the DataSource seam) and the favorites store (the durable
// filesystem store behind the FavoritesStore seam) — the wire, the token adjudication,
// and the per-principal isolation are the same code the CI lane runs.
//
// It is real on three axes: the MCP wire + handshake (initialize → tools/list →
// tools/call over HTTP/SSE), per-principal token adjudication (it verifies the
// delegated HS256 token, extracts sub/aud, 403s an unauthorized principal, rejects a
// wrong audience), and per-principal state isolation (favorite_listing writes into the
// CALLING principal's favorites, keyed by the token identity — never a client-supplied
// arg — so tenant A's writes are invisible to tenant B). Only the read tool's listing
// DATA is pluggable: CannedData is the hermetic default the CI lane runs on, and LiveData
// (live_data.go) backs the demo with real web-search results.
package rentals

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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
// lane is green-able forever. LiveData (live_data.go) implements this same interface with
// real web-search results and is swapped in for the runnable Wander demo, not the CI lane.
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

// Server is the rentals MCP HTTP/SSE server. It owns a per-principal favorites store
// (by default in-memory, so reset per NewServer, i.e. per test run) and adjudicates
// every tools/call by the delegated token's identity.
type Server struct {
	httptest *httptest.Server
	data     DataSource
	audience string // the audience this server accepts (RFC 8707 resource id)

	// favorites is the per-principal favorites store, keyed by the compound
	// PrincipalKey derived from the VERIFIED token. Never nil (see newServer).
	favorites FavoritesStore

	// tenantKeys is the per-tenant HS256 verification key map — the SAME keys fuse's
	// built-in STS mints under, shared out of band (as a real resource server shares a
	// verification key). A token is authorized under the tenant whose key verifies it.
	tenantKeys map[event.TenantID][]byte

	mu sync.Mutex
	// sseConns maps a SESSION ID to that session's SSE delivery channel. A JSON-RPC
	// response is routed to exactly one entry — the session whose POST carried the id —
	// never fanned out. Broadcasting would put one principal's response frame on another
	// principal's stream, and since fuse's MCP client numbers request ids PER CLIENT
	// (internal/mcp/http_client.go), the two id spaces collide by construction, so a
	// stray frame can resolve the OTHER principal's pending call. An entry is removed
	// when its handleSSE returns, so a long-lived process does not accumulate dead
	// streams. Never nil (see newServer).
	sseConns  map[string]chan string
	sseSeq    uint64   // monotonic fallback discriminator for session ids
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
	// Favorites is the per-principal favorites store; nil ⇒ NewMemoryFavorites() (the
	// CI-lane default). Swapping in a durable store changes only WHERE a set lives,
	// never WHOSE set a write lands in — the key stays the verified token identity.
	Favorites FavoritesStore
}

// newServer builds a rentals Server with no listener of any kind, defaulting the data
// source and the favorites store. Both constructors go through it.
func newServer(cfg Config) *Server {
	s := &Server{
		data:       cfg.Data,
		audience:   cfg.Audience,
		tenantKeys: map[event.TenantID][]byte{},
		favorites:  cfg.Favorites,
		sseConns:   map[string]chan string{},
	}
	if s.data == nil {
		s.data = CannedData{}
	}
	if s.favorites == nil {
		s.favorites = NewMemoryFavorites()
	}
	for k, v := range cfg.TenantKeys {
		kc := make([]byte, len(v))
		copy(kc, v)
		s.tenantKeys[event.NormalizeTenant(k)] = kc
	}
	return s
}

// newMux builds the routed mux (the complete MCP surface: /sse + /messages) for this
// Server. It is the single definition of the routing table, shared by the servable
// constructor (NewHandler/Handler) and the test constructor (NewServer).
func (s *Server) newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", s.handleSSE)
	mux.HandleFunc("/messages", s.handleMessages)
	return mux
}

// NewHandler is the SERVABLE constructor: it builds a rentals Server and returns its
// routed http.Handler WITHOUT binding a listener, so a caller (cmd/rentals-mcp, or a
// test that wants to own the listener) can serve it on a real address via
// http.Server.ListenAndServe or httptest.NewServer. The returned *Server is the state
// owner (favorites, captured wire headers); the returned Server has no listener, so
// URL() and Close() are NOT valid on it — those belong to NewServer.
func NewHandler(cfg Config) (*Server, http.Handler) {
	s := newServer(cfg)
	return s, s.newMux()
}

// Handler returns this Server's routed http.Handler. It binds no listener; serving it
// is the caller's job. A Server from NewServer is already serving its own handler on
// its httptest listener, so this is mainly the accessor for the NewHandler path.
func (s *Server) Handler() http.Handler { return s.newMux() }

// NewServer is the TEST constructor: it builds the rentals MCP server and self-hosts it
// on an httptest.Server, exposing URL() / Close(). Use it from tests that want a dialable
// server with no listener bookkeeping. For anything that must serve on a real port — the
// runnable demo, cmd/rentals-mcp — use NewHandler (or Handler()), which returns the same
// routed handler with no listener bound. With the default (in-memory) favorites store
// the favorites start empty, reset per call; a Config.Favorites store keeps whatever it
// already holds. Call Close() to stop it.
//
// Teardown note: handleSSE blocks on r.Context().Done(), so Close() does not return until
// connected clients disconnect. Register Close and any MCP client's teardown with
// t.Cleanup (server first, client second, so LIFO stops the client first); never
// `defer srv.Close()` ahead of the client's stop.
func NewServer(cfg Config) *Server {
	s := newServer(cfg)
	s.httptest = httptest.NewServer(s.newMux())
	return s
}

// URL returns the base URL an MCP client dials (the /sse + /messages routes live
// under it). Only valid on a Server from NewServer.
func (s *Server) URL() string { return s.httptest.URL }

// Close stops the server. Only valid on a Server from NewServer; a NewHandler Server
// owns no listener and needs no close.
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

// sseConnCount reports how many SSE streams are currently registered. It exists so a
// test can assert the registry is pruned when a client disconnects: a long-lived rentals
// process (cmd/rentals-mcp, one MCP manager per loop) must not accumulate dead streams.
func (s *Server) sseConnCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sseConns)
}

// sessionParam is the query parameter naming the SSE session a POST belongs to. The
// server advertises it on the endpoint event; the client (per the MCP HTTP/SSE
// transport) POSTs to the advertised URL verbatim, so the parameter comes back on every
// message and the response can be routed to the stream that asked for it.
const sessionParam = "sessionId"

// newSessionID mints an unguessable session id. A session id is a routing capability —
// holding it is what lets a POST address a stream — so it comes from crypto/rand, with a
// monotonic counter suffix so ids stay distinct even if the entropy source ever repeats.
func (s *Server) newSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is not survivable for a capability, but this is a demo
		// server: degrade to the counter rather than kill the stream. The counter alone
		// still guarantees DISTINCTNESS (the isolation property); only unguessability
		// is lost.
		b = [16]byte{}
	}
	s.mu.Lock()
	s.sseSeq++
	n := s.sseSeq
	s.mu.Unlock()
	return fmt.Sprintf("%s-%d", hex.EncodeToString(b[:]), n)
}

// registerSSE installs ch under a fresh session id and returns that id.
func (s *Server) registerSSE(ch chan string) string {
	id := s.newSessionID()
	s.mu.Lock()
	s.sseConns[id] = ch
	s.mu.Unlock()
	return id
}

// unregisterSSE removes a session's channel. It is deliberately by ID, not by channel
// value: identity is what crosses the lock boundary, never a live handle to state
// another goroutine may be using. The channel is never closed — a departed session's
// buffered channel is simply dropped, so no delivery can ever race a close.
func (s *Server) unregisterSSE(id string) {
	s.mu.Lock()
	delete(s.sseConns, id)
	s.mu.Unlock()
}

// deliver routes one frame to a single session. The membership lookup and the
// non-blocking send happen under ONE lock hold, so a session that unregistered
// concurrently simply receives nothing — there is no window in which delivery targets a
// stream that has already gone away. An unknown or empty session id delivers nowhere:
// this boundary fails closed, because guessing wrong means writing one principal's data
// onto another principal's stream.
func (s *Server) deliver(sessionID, frame string) {
	if sessionID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ch, ok := s.sseConns[sessionID]
	if !ok {
		return
	}
	select {
	case ch <- frame:
	default: // slow or wedged reader: drop rather than block the POST handler
	}
}

// handleSSE holds one long-lived SSE stream per client, announcing the /messages
// endpoint — carrying THIS stream's session id — then relaying the JSON-RPC responses
// addressed to that session.
func (s *Server) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "no flush", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	ch := make(chan string, 16)
	sessionID := s.registerSSE(ch)
	// Pruned on EVERY exit path, so a long-lived server (cmd/rentals-mcp, one MCP
	// manager per loop) does not accumulate the streams of loops that have gone away.
	defer s.unregisterSSE(sessionID)
	endpoint := url.URL{
		Scheme:   "http",
		Host:     r.Host,
		Path:     "/messages",
		RawQuery: url.Values{sessionParam: []string{sessionID}}.Encode(),
	}
	fmt.Fprintf(w, "event: endpoint\ndata: %s\n\n", endpoint.String())
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
	// Route to the ONE session that asked, identified by the query parameter the
	// endpoint event advertised on that session's stream.
	s.deliver(r.URL.Query().Get(sessionParam), string(b))
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
		// pk comes from the VERIFIED token (authorize), never from args.
		if err := s.addFavorite(pk, args.ListingID); err != nil {
			return errResult(fmt.Sprintf("favorite_listing failed: %v", err)), nil
		}
		return textResult(fmt.Sprintf("favorited %s", args.ListingID)), nil
	case "list_favorites":
		favs, err := s.listFavorites(pk)
		if err != nil {
			return errResult(fmt.Sprintf("list_favorites failed: %v", err)), nil
		}
		if favs == nil {
			favs = []string{} // an empty set serialises as [], never null
		}
		return textResult(mustJSON(favs)), nil
	default:
		return errResult(fmt.Sprintf("unknown tool %q", params.Name)), nil
	}
}

// authorize verifies the delegated token and returns the CALLING principal's store
// key (tenant + sub). It fails closed: missing/invalid token, unknown tenant, or a
// wrong-audience binding are all authorization denials (CodeUnauthorized).
func (s *Server) authorize(r *http.Request) (PrincipalKey, *jsonrpcErr) {
	auth := r.Header.Get("Authorization")
	const pfx = "Bearer "
	if len(auth) <= len(pfx) || !strings.EqualFold(auth[:len(pfx)], pfx) {
		return PrincipalKey{}, &jsonrpcErr{Code: CodeUnauthorized, Message: "missing bearer credential"}
	}
	token := strings.TrimSpace(auth[len(pfx):])

	// Wrong-audience rejection: the token is bound (RFC 8707) to the resource in the
	// MCP-Resource header; it must match this server's audience.
	if s.audience != "" {
		if res := r.Header.Get("MCP-Resource"); res != s.audience {
			return PrincipalKey{}, &jsonrpcErr{Code: CodeUnauthorized, Message: fmt.Sprintf("wrong audience: token bound to %q, server is %q", res, s.audience)}
		}
	}

	// Find the tenant whose key verifies this token (per-tenant credential isolation:
	// a token minted under tenant A never verifies under tenant B's key).
	tenant, claims, ok := s.verify(token)
	if !ok {
		return PrincipalKey{}, &jsonrpcErr{Code: CodeUnauthorized, Message: "token does not verify under any known tenant key"}
	}
	if claims.Subject == "" {
		return PrincipalKey{}, &jsonrpcErr{Code: CodeUnauthorized, Message: "token carries no subject"}
	}
	// Audience claim must also match (defense in depth alongside the header check).
	if s.audience != "" && claims.Audience != s.audience {
		return PrincipalKey{}, &jsonrpcErr{Code: CodeUnauthorized, Message: fmt.Sprintf("token aud %q != server audience %q", claims.Audience, s.audience)}
	}
	return PrincipalKey{Tenant: tenant, Subject: claims.Subject}, nil
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

// addFavorite delegates to the configured store. pk is the CALLING token's identity.
func (s *Server) addFavorite(pk PrincipalKey, id string) error {
	return s.favorites.Add(pk, id)
}

// listFavorites delegates to the configured store. pk is the CALLING token's identity.
func (s *Server) listFavorites(pk PrincipalKey) ([]string, error) {
	return s.favorites.List(pk)
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
