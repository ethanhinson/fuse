package harness

// SpecFeatureNoTools names HarnessSpec.no_tools (and Role.no_tools,
// Worker.no_tools) in GetCapabilitiesResponse.spec_features. proto3 drops a
// field a server does not know, so a caller checks for it before StartLoop.
const SpecFeatureNoTools = "no_tools"

// SpecFeatures lists the HarnessSpec features this runtime implements beyond
// the fields every server accepts. It returns a fresh slice.
func SpecFeatures() []string { return []string{SpecFeatureNoTools} }
