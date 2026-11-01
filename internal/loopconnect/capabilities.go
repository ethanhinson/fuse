package loopconnect

import (
	"context"
	"errors"
	"math"
	"slices"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/ethanhinson/fuse/internal/capabilities"
	"github.com/ethanhinson/fuse/internal/event"
	loopv1 "github.com/ethanhinson/fuse/internal/loopwire/v1"
	"github.com/ethanhinson/fuse/internal/observe"
	observeotel "github.com/ethanhinson/fuse/internal/observe/otel"
)

// CapabilitySource answers GetCapabilities for one tenant. The tenant is
// always the authenticated one; implementations filter the tenant-scoped parts
// (secret refs) by it.
type CapabilitySource interface {
	Capabilities(ctx context.Context, tenant event.TenantID) (capabilities.Capabilities, error)
}

// WithCapabilities injects the source; nil keeps Unimplemented. Chainable.
func (h *Handler) WithCapabilities(s CapabilitySource) *Handler {
	if s != nil {
		h.caps = s
	}
	return h
}

// GetCapabilities is the read-only discovery RPC. The interceptor has already
// refused a missing or invalid token (Unauthenticated); a wire tenant that
// mismatches the principal is a spoof (PermissionDenied), exactly as for
// StartLoop. The source is asked only after both checks pass, and always for
// the authenticated tenant. Until a source is wired it answers Unimplemented,
// so a client never mistakes an empty answer for a server that accepts nothing.
func (h *Handler) GetCapabilities(ctx context.Context, req *connect.Request[loopv1.GetCapabilitiesRequest]) (*connect.Response[loopv1.GetCapabilitiesResponse], error) {
	ctx, apiSpan := h.observer.Start(ctx, observe.Descriptor{Kind: observe.OperationAPIRequest, Name: "get_capabilities"})
	apiOutcome := observe.OutcomeSuccess
	defer func() { apiSpan.End(apiOutcome) }()
	fail := func(err *connect.Error) (*connect.Response[loopv1.GetCapabilitiesResponse], error) {
		apiOutcome = observeotel.OutcomeFromError(ctx, err)
		return nil, err
	}
	tenant, _, _, aerr := h.effectiveTenant(ctx, req.Msg.Tenant)
	if aerr != nil {
		return fail(aerr)
	}
	if h.caps == nil {
		return fail(connect.NewError(connect.CodeUnimplemented, errors.New("loopconnect: capabilities are not advertised by this server")))
	}
	c, err := h.caps.Capabilities(ctx, tenant)
	if err != nil {
		return fail(connect.NewError(connect.CodeInternal, err))
	}
	resp := connect.NewResponse(toProtoCapabilities(c))
	if id := observeotel.TraceID(ctx); id != "" {
		resp.Header().Set("Fuse-Trace-Id", id)
	}
	return resp, nil
}

// toProtoCapabilities is the only capabilities-to-proto mapping. It copies
// every slice, so the response never aliases the caller's answer.
func toProtoCapabilities(c capabilities.Capabilities) *loopv1.GetCapabilitiesResponse {
	out := &loopv1.GetCapabilitiesResponse{
		SecretRefs:       slices.Clone(c.SecretRefs),
		SpecMcpAuthKinds: slices.Clone(c.SpecMCPAuthKinds),
		SpecPolicy: &loopv1.SpecPolicy{
			McpHosts:            slices.Clone(c.SpecPolicy.MCPHosts),
			DenyPrivateNetworks: c.SpecPolicy.DenyPrivateNetworks,
			ModelsRegistryOnly:  c.SpecPolicy.ModelsRegistryOnly,
		},
		Server: &loopv1.ServerInfo{Version: c.Server.Version, DigestAlgorithm: c.Server.DigestAlgorithm},
	}
	for _, m := range c.Models {
		out.Models = append(out.Models, &loopv1.ModelInfo{
			Alias:                  m.Alias,
			GatewayId:              m.GatewayID,
			MaxOutputTokens:        clampUint32(m.MaxOutputTokens),
			ContextWindow:          clampUint32(m.ContextWindow),
			ContextWindowIsDefault: m.ContextWindowIsDefault,
			Persona:                m.Persona,
			IsDefault:              m.IsDefault,
			Label:                  m.Label,
			Description:            m.Description,
			Tags:                   slices.Clone(m.Tags),
		})
	}
	for _, s := range c.Skills {
		out.Skills = append(out.Skills, &loopv1.SkillInfo{Name: s.Name, Description: s.Description})
	}
	for _, cl := range c.Ceilings {
		pc := &loopv1.Ceiling{
			FieldPath:   cl.FieldPath,
			OperatorSet: cl.OperatorSet,
			Direction:   toProtoDirection(cl.Direction),
		}
		// An unset ceiling carries no value: a spec cannot set that field at
		// all, and a zero would read as a real limit.
		if cl.OperatorSet {
			pc.Value = structpb.NewNumberValue(cl.Value)
		}
		out.Ceilings = append(out.Ceilings, pc)
	}
	return out
}

func toProtoDirection(d capabilities.Direction) loopv1.CeilingDirection {
	switch d {
	case capabilities.DirectionMax:
		return loopv1.CeilingDirection_CEILING_DIRECTION_MAX
	case capabilities.DirectionMin:
		return loopv1.CeilingDirection_CEILING_DIRECTION_MIN
	default:
		return loopv1.CeilingDirection_CEILING_DIRECTION_UNSPECIFIED
	}
}

// clampUint32 maps n into the wire's uint32: negative becomes 0 and anything
// above the maximum becomes the maximum.
func clampUint32(n int) uint32 {
	if n <= 0 {
		return 0
	}
	if uint64(n) > math.MaxUint32 {
		return math.MaxUint32
	}
	return uint32(n)
}
