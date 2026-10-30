package harness_test

import (
	"sort"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
)

// fieldUI reads the (fuse.harness.v1.ui) option of a field, nil when absent.
func fieldUI(fd protoreflect.FieldDescriptor) *harnessv1.FieldUI {
	opts := fd.Options()
	if opts == nil || !proto.HasExtension(opts, harnessv1.E_Ui) {
		return nil
	}
	return proto.GetExtension(opts, harnessv1.E_Ui).(*harnessv1.FieldUI)
}

// walkSpec visits every field reachable from HarnessSpec, keyed by a dotted
// path through field names (map and list values are entered without an index).
func walkSpec(visit func(path string, fd protoreflect.FieldDescriptor)) {
	seen := map[protoreflect.FullName]bool{}
	var walk func(prefix string, md protoreflect.MessageDescriptor)
	walk = func(prefix string, md protoreflect.MessageDescriptor) {
		if seen[md.FullName()] {
			return
		}
		seen[md.FullName()] = true
		fields := md.Fields()
		for i := 0; i < fields.Len(); i++ {
			fd := fields.Get(i)
			path := prefix + string(fd.Name())
			visit(path, fd)
			switch {
			case fd.IsMap():
				if m := fd.MapValue().Message(); m != nil {
					walk(path+".", m)
				}
			case fd.Message() != nil:
				walk(path+".", fd.Message())
			}
		}
	}
	walk("", (&harnessv1.HarnessSpec{}).ProtoReflect().Descriptor())
}

// TestEveryFieldHasALabel: a form renderer can label every field from the
// descriptors alone.
func TestEveryFieldHasALabel(t *testing.T) {
	walkSpec(func(path string, fd protoreflect.FieldDescriptor) {
		if ui := fieldUI(fd); ui == nil || strings.TrimSpace(ui.GetLabel()) == "" {
			t.Errorf("%s has no ui.label", path)
		}
	})
}

// TestSecretRefMarkers: exactly the secret-name fields carry secret_ref, and
// each uses the secret picker widget.
func TestSecretRefMarkers(t *testing.T) {
	var got []string
	walkSpec(func(path string, fd protoreflect.FieldDescriptor) {
		ui := fieldUI(fd)
		if ui.GetSecretRef() {
			got = append(got, path)
			if ui.GetWidget() != harnessv1.Widget_WIDGET_SECRET_REF {
				t.Errorf("%s is secret_ref but widget is %v", path, ui.GetWidget())
			}
		}
	})
	want := []string{"mcp_servers.auth.client_secret_ref", "research.custom.headers.secret_ref"}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("secret_ref fields = %v, want %v", got, want)
	}
}

// TestTightenOnlyMarkers mirrors the ADR-0006 tighten-only axes the loader
// enforces for .fuse.local.yml (internal/config: tightenNumeric, mergeWorkflows,
// mergeThroughput, mergePipeline). A new tighten-only axis must be marked here
// and in harness.proto together.
func TestTightenOnlyMarkers(t *testing.T) {
	var got []string
	walkSpec(func(path string, fd protoreflect.FieldDescriptor) {
		if fieldUI(fd).GetTightenOnly() {
			got = append(got, path)
		}
	})
	want := []string{
		"agents.queue_bound", "agents.tool_timeout_seconds",
		"pipeline.synthesis.max_attempts", "pipeline.synthesis.max_depth", "pipeline.synthesis.max_fanout", "pipeline.synthesis.max_steps",
		"throughput.requests_per_minute", "throughput.session_tokens", "throughput.tokens_per_minute",
		"workflows.pool.concurrent", "workflows.pool.max_depth", "workflows.pool.tokens", "workflows.pool.total",
	}
	sort.Strings(got)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("tighten_only fields =\n %v\nwant\n %v", got, want)
	}
}

// TestEnumValuesHaveDisplayNames: every selectable enum value has a display
// name. A zero value is exempt only when validation rejects it (it is then
// never a valid choice).
func TestEnumValuesHaveDisplayNames(t *testing.T) {
	exemptZero := map[protoreflect.FullName]bool{"fuse.harness.v1.McpTransport": true}
	walkSpec(func(path string, fd protoreflect.FieldDescriptor) {
		ed := fd.Enum()
		if ed == nil || ed.FullName() == "fuse.harness.v1.Widget" {
			return
		}
		vals := ed.Values()
		for i := 0; i < vals.Len(); i++ {
			v := vals.Get(i)
			if v.Number() == 0 && exemptZero[ed.FullName()] {
				continue
			}
			opts := v.Options()
			if opts == nil || !proto.HasExtension(opts, harnessv1.E_ValueUi) ||
				proto.GetExtension(opts, harnessv1.E_ValueUi).(*harnessv1.EnumValueUI).GetDisplayName() == "" {
				t.Errorf("%s: enum value %s has no display_name", path, v.FullName())
			}
		}
	})
}
