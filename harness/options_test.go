package harness_test

import (
	"sort"
	"strings"
	"testing"

	"buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate"
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

// TestInstructionsFieldContract pins the wire contract of the free-text task
// brief: field number, kind, form metadata and the length rule.
func TestInstructionsFieldContract(t *testing.T) {
	fd := (&harnessv1.HarnessSpec{}).ProtoReflect().Descriptor().Fields().ByName("instructions")
	if fd == nil {
		t.Fatal("HarnessSpec has no instructions field")
	}
	if fd.Number() != 14 {
		t.Errorf("number = %d, want 14", fd.Number())
	}
	if fd.Kind() != protoreflect.StringKind {
		t.Errorf("kind = %v, want string", fd.Kind())
	}
	ui := fieldUI(fd)
	if ui == nil {
		t.Fatal("instructions has no ui option")
	}
	if ui.GetLabel() != "Instructions" {
		t.Errorf("label = %q", ui.GetLabel())
	}
	if ui.GetWidget() != harnessv1.Widget_WIDGET_TEXTAREA {
		t.Errorf("widget = %v, want WIDGET_TEXTAREA", ui.GetWidget())
	}
	const help = "What this agent is for and how it should work. Added to the system prompt after the persona. Applies to the main agent only; subagents keep their own prompts."
	if ui.GetHelp() != help {
		t.Errorf("help = %q, want %q", ui.GetHelp(), help)
	}
	rules, ok := proto.GetExtension(fd.Options(), validate.E_Field).(*validate.FieldRules)
	if !ok || rules == nil {
		t.Fatal("instructions has no buf.validate field rules")
	}
	if got := rules.GetString().GetMaxLen(); got != 16384 {
		t.Errorf("string.max_len = %d, want 16384", got)
	}
}

// TestRolesFieldContract pins the wire contract of named roles: the map on
// HarnessSpec, the Role message's fields and Workflow.roles.
func TestRolesFieldContract(t *testing.T) {
	rulesOf := func(fd protoreflect.FieldDescriptor) *validate.FieldRules {
		t.Helper()
		rules, ok := proto.GetExtension(fd.Options(), validate.E_Field).(*validate.FieldRules)
		if !ok || rules == nil {
			t.Fatalf("%s has no buf.validate field rules", fd.FullName())
		}
		return rules
	}
	field := func(md protoreflect.MessageDescriptor, name string, number protoreflect.FieldNumber) protoreflect.FieldDescriptor {
		t.Helper()
		fd := md.Fields().ByName(protoreflect.Name(name))
		if fd == nil {
			t.Fatalf("%s has no %s field", md.FullName(), name)
		}
		if fd.Number() != number {
			t.Errorf("%s number = %d, want %d", fd.FullName(), fd.Number(), number)
		}
		if fieldUI(fd) == nil {
			t.Fatalf("%s has no ui option", fd.FullName())
		}
		return fd
	}

	roles := field((&harnessv1.HarnessSpec{}).ProtoReflect().Descriptor(), "roles", 15)
	if !roles.IsMap() {
		t.Fatal("HarnessSpec.roles is not a map")
	}
	if got := roles.MapValue().Message().FullName(); got != "fuse.harness.v1.Role" {
		t.Errorf("roles value = %s, want fuse.harness.v1.Role", got)
	}
	if got := fieldUI(roles).GetLabel(); got != "Roles" {
		t.Errorf("roles label = %q", got)
	}
	m := rulesOf(roles).GetMap()
	if m.GetMaxPairs() != 32 {
		t.Errorf("roles map.max_pairs = %d, want 32", m.GetMaxPairs())
	}
	keys := m.GetKeys().GetString()
	if keys.GetMinLen() != 1 || keys.GetMaxLen() != 64 || keys.GetPattern() != "^[a-z0-9][a-z0-9_-]*$" {
		t.Errorf("roles key rules = %v", keys)
	}

	role := (&harnessv1.Role{}).ProtoReflect().Descriptor()

	desc := field(role, "description", 1)
	if desc.Kind() != protoreflect.StringKind {
		t.Errorf("description kind = %v", desc.Kind())
	}
	if s := rulesOf(desc).GetString(); s.GetMinLen() != 1 || s.GetMaxLen() != 512 {
		t.Errorf("description rules = %v, want min_len 1 max_len 512", s)
	}
	if got := fieldUI(desc).GetLabel(); got != "Description" {
		t.Errorf("description label = %q", got)
	}
	const descHelp = "What the main agent reads when deciding whom to hand work to."
	if got := fieldUI(desc).GetHelp(); got != descHelp {
		t.Errorf("description help = %q, want %q", got, descHelp)
	}

	instr := field(role, "instructions", 2)
	if instr.Kind() != protoreflect.StringKind {
		t.Errorf("instructions kind = %v", instr.Kind())
	}
	if got := rulesOf(instr).GetString().GetMaxLen(); got != 16384 {
		t.Errorf("instructions max_len = %d, want 16384", got)
	}
	if got := fieldUI(instr).GetWidget(); got != harnessv1.Widget_WIDGET_TEXTAREA {
		t.Errorf("instructions widget = %v, want WIDGET_TEXTAREA", got)
	}

	tools := field(role, "tools", 3)
	if !tools.IsList() || tools.Kind() != protoreflect.StringKind {
		t.Errorf("tools is not repeated string")
	}
	rep := rulesOf(tools).GetRepeated()
	if rep.GetMinItems() != 0 {
		t.Errorf("tools min_items = %d, want 0", rep.GetMinItems())
	}
	if got := rep.GetItems().GetString().GetMinLen(); got != 1 {
		t.Errorf("tools item min_len = %d, want 1", got)
	}
	if got := fieldUI(tools).GetWidget(); got != harnessv1.Widget_WIDGET_LIST {
		t.Errorf("tools widget = %v, want WIDGET_LIST", got)
	}

	model := field(role, "model", 4)
	if model.Kind() != protoreflect.StringKind {
		t.Errorf("model kind = %v", model.Kind())
	}
	if got := rulesOf(model).GetString().GetMaxLen(); got != 128 {
		t.Errorf("model max_len = %d, want 128", got)
	}
	if got := fieldUI(model).GetWidget(); got != harnessv1.Widget_WIDGET_MODEL_ALIAS {
		t.Errorf("model widget = %v, want WIDGET_MODEL_ALIAS", got)
	}

	wfRoles := field((&harnessv1.Workflow{}).ProtoReflect().Descriptor(), "roles", 4)
	if !wfRoles.IsList() || wfRoles.Kind() != protoreflect.StringKind {
		t.Errorf("Workflow.roles is not repeated string")
	}
	if got := fieldUI(wfRoles).GetLabel(); got != "Roles" {
		t.Errorf("Workflow.roles label = %q", got)
	}
	if got := fieldUI(wfRoles).GetWidget(); got != harnessv1.Widget_WIDGET_LIST {
		t.Errorf("Workflow.roles widget = %v, want WIDGET_LIST", got)
	}
}

// TestSkillRefFieldContract pins the wire contract of carried skills: the
// SkillRef description and body fields, the message rule that a carried entry
// needs a description, and the list rules on HarnessSpec.skills.
func TestSkillRefFieldContract(t *testing.T) {
	md := (&harnessv1.SkillRef{}).ProtoReflect().Descriptor()
	field := func(name string, number protoreflect.FieldNumber) (protoreflect.FieldDescriptor, *validate.FieldRules) {
		t.Helper()
		fd := md.Fields().ByName(protoreflect.Name(name))
		if fd == nil {
			t.Fatalf("SkillRef has no %s field", name)
		}
		if fd.Number() != number {
			t.Errorf("%s number = %d, want %d", name, fd.Number(), number)
		}
		if fd.Kind() != protoreflect.StringKind {
			t.Errorf("%s kind = %v, want string", name, fd.Kind())
		}
		rules, ok := proto.GetExtension(fd.Options(), validate.E_Field).(*validate.FieldRules)
		if !ok || rules == nil {
			t.Fatalf("%s has no buf.validate field rules", name)
		}
		return fd, rules
	}
	field("name", 1)

	desc, descRules := field("description", 2)
	if ui := fieldUI(desc); ui == nil || ui.GetLabel() != "Description" || ui.GetWidget() != harnessv1.Widget_WIDGET_TEXT {
		t.Errorf("description ui = %+v, want label Description, WIDGET_TEXT", ui)
	}
	if got := descRules.GetString().GetMaxLen(); got != 1024 {
		t.Errorf("description string.max_len = %d, want 1024", got)
	}
	if got := descRules.GetString().GetPattern(); got != `^[^\r\n]*$` {
		t.Errorf("description string.pattern = %q", got)
	}

	body, bodyRules := field("body", 3)
	if ui := fieldUI(body); ui == nil || ui.GetLabel() != "Instructions" || ui.GetWidget() != harnessv1.Widget_WIDGET_TEXTAREA {
		t.Errorf("body ui = %+v, want label Instructions, WIDGET_TEXTAREA", ui)
	}
	if got := bodyRules.GetString().GetMaxBytes(); got != 32768 {
		t.Errorf("body string.max_bytes = %d, want 32768", got)
	}

	msgRules, ok := proto.GetExtension(md.Options(), validate.E_Message).(*validate.MessageRules)
	if !ok || msgRules == nil {
		t.Fatal("SkillRef has no buf.validate message rules")
	}
	if !hasCEL(msgRules.GetCel(), "fuse.skill.carried_description") {
		t.Errorf("SkillRef message rules lack fuse.skill.carried_description: %+v", msgRules.GetCel())
	}
	if !hasCEL(msgRules.GetCel(), "fuse.skill.description_needs_body") {
		t.Errorf("SkillRef message rules lack fuse.skill.description_needs_body: %+v", msgRules.GetCel())
	}

	skills := (&harnessv1.HarnessSpec{}).ProtoReflect().Descriptor().Fields().ByName("skills")
	listRules, ok := proto.GetExtension(skills.Options(), validate.E_Field).(*validate.FieldRules)
	if !ok || listRules == nil {
		t.Fatal("HarnessSpec.skills has no buf.validate field rules")
	}
	if got := listRules.GetRepeated().GetMaxItems(); got != 64 {
		t.Errorf("skills repeated.max_items = %d, want 64", got)
	}
	for _, id := range []string{"fuse.skills.unique_names", "fuse.skills.carried_max"} {
		if !hasCEL(listRules.GetCel(), id) {
			t.Errorf("skills lacks CEL rule %s", id)
		}
	}
}

func hasCEL(rules []*validate.Rule, id string) bool {
	for _, r := range rules {
		if r.GetId() == id {
			return true
		}
	}
	return false
}
