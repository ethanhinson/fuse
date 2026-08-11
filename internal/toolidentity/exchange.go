package toolidentity

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ethanhinson/fuse/internal/event"
)

// ExchangeRequest carries the RFC 8693 delegation inputs: the loop initiator's
// identity (Subject, in the minted token's `sub`), fuse as the actor (Actor, in
// the `act` claim), the tenant isolation boundary, and the audience/scopes the
// minted token is bound to. Every field originates from the authenticated
// loop-start context and the tool declaration — never from model output.
type ExchangeRequest struct {
	Tenant   event.TenantID
	Subject  string   // the loop initiator; becomes `sub`
	Actor    string   // fuse; becomes `act.sub` (delegation, not impersonation)
	Audience string   // RFC 8707 resource id; becomes `aud`
	Scopes   []string // becomes the space-delimited `scope`
}

// ExchangeResult is a minted downstream token plus its expiry and a descriptor of
// the delegation chain (for audit). The Token is a credential secret — callers
// wrap it in a Credential (which redacts) before it leaves this package.
type ExchangeResult struct {
	Token    string
	Expiry   time.Time
	ActChain string // e.g. "fuse->alice"; safe to audit (no secret)
}

// TokenExchanger performs the RFC 8693 token exchange, converting an
// intermediary-held identity into a downstream credential per call. BuiltinSTS is
// the zero-config default; an external-AS exchanger slots in behind this seam.
type TokenExchanger interface {
	Exchange(ctx context.Context, req ExchangeRequest) (ExchangeResult, error)
}

// BuiltinSTSConfig configures the minimal built-in STS. TenantKeys maps each
// tenant to its own signing key so a tenant's minted tokens are never verifiable
// under another tenant's key (per-tenant credential isolation, D4).
type BuiltinSTSConfig struct {
	Issuer     string
	TTL        time.Duration
	TenantKeys map[event.TenantID][]byte
}

// BuiltinSTS is fuse's minimal built-in Security Token Service: it mints
// short-lived, audience-bound HS256 delegation tokens (initiator in `sub`, fuse
// in `act`) that fuse's own downstreams verify with the shared per-tenant key. It
// keeps a zero-config local story where no external AS is available. Richer
// exchange (external AS, cross-issuer) rides the TokenExchanger seam instead.
type BuiltinSTS struct {
	issuer string
	ttl    time.Duration
	keys   map[event.TenantID][]byte
}

// ErrTenantNotConfigured is returned when a tenant has no signing key — the STS
// fails closed rather than minting with an empty or shared key.
var ErrTenantNotConfigured = errors.New("toolidentity: tenant has no configured signing key")

// NewBuiltinSTS builds the STS. TTL defaults to 5 minutes when unset; at least
// one tenant key is required.
func NewBuiltinSTS(cfg BuiltinSTSConfig) (*BuiltinSTS, error) {
	if len(cfg.TenantKeys) == 0 {
		return nil, errors.New("toolidentity: BuiltinSTS needs at least one tenant signing key")
	}
	ttl := cfg.TTL
	if ttl <= 0 {
		ttl = 5 * time.Minute
	}
	issuer := cfg.Issuer
	if issuer == "" {
		issuer = "fuse"
	}
	// Copy the key map so a caller mutating theirs afterward cannot swap a key.
	keys := make(map[event.TenantID][]byte, len(cfg.TenantKeys))
	for k, v := range cfg.TenantKeys {
		kc := make([]byte, len(v))
		copy(kc, v)
		keys[event.NormalizeTenant(k)] = kc
	}
	return &BuiltinSTS{issuer: issuer, ttl: ttl, keys: keys}, nil
}

// Exchange mints a delegation token. It fails closed for an unconfigured tenant.
func (s *BuiltinSTS) Exchange(_ context.Context, req ExchangeRequest) (ExchangeResult, error) {
	tenant := event.NormalizeTenant(req.Tenant)
	key, ok := s.keys[tenant]
	if !ok {
		return ExchangeResult{}, fmt.Errorf("%w: %q", ErrTenantNotConfigured, tenant)
	}
	if req.Subject == "" {
		return ExchangeResult{}, errors.New("toolidentity: exchange requires a subject")
	}
	actor := req.Actor
	if actor == "" {
		actor = "fuse"
	}

	now := time.Now()
	exp := now.Add(s.ttl)
	claims := map[string]any{
		"iss": s.issuer,
		"sub": req.Subject,
		"aud": req.Audience,
		"iat": now.Unix(),
		"exp": exp.Unix(),
		// Delegation, not impersonation: fuse is the actor acting for the subject.
		"act": map[string]any{"sub": actor},
	}
	if len(req.Scopes) > 0 {
		claims["scope"] = strings.Join(req.Scopes, " ")
	}

	token, err := signHS256(claims, key)
	if err != nil {
		return ExchangeResult{}, err
	}
	return ExchangeResult{
		Token:    token,
		Expiry:   exp,
		ActChain: actor + "->" + req.Subject,
	}, nil
}

// Verify checks the compact JWS signature under the tenant's key and that it is
// unexpired. It is used by fuse's own downstream verification and by tests; a
// real external resource server verifies with the shared key out of band.
func (s *BuiltinSTS) Verify(tenant event.TenantID, token string) error {
	key, ok := s.keys[event.NormalizeTenant(tenant)]
	if !ok {
		return ErrTenantNotConfigured
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return errors.New("toolidentity: malformed token")
	}
	signingInput := parts[0] + "." + parts[1]
	want := hmacSHA256(signingInput, key)
	got, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return errors.New("toolidentity: malformed signature")
	}
	if subtle.ConstantTimeCompare(want, got) != 1 {
		return errors.New("toolidentity: signature mismatch")
	}
	// Expiry check.
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return errors.New("toolidentity: malformed payload")
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return errors.New("toolidentity: malformed claims")
	}
	if claims.Exp != 0 && time.Now().Unix() >= claims.Exp {
		return errors.New("toolidentity: token expired")
	}
	return nil
}

// signHS256 produces a compact HS256 JWS over the claims.
func signHS256(claims map[string]any, key []byte) (string, error) {
	header := map[string]string{"alg": "HS256", "typ": "JWT"}
	hb, err := json.Marshal(header)
	if err != nil {
		return "", err
	}
	pb, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	segs := base64.RawURLEncoding.EncodeToString(hb) + "." + base64.RawURLEncoding.EncodeToString(pb)
	sig := hmacSHA256(segs, key)
	return segs + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func hmacSHA256(signingInput string, key []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(signingInput))
	return m.Sum(nil)
}
