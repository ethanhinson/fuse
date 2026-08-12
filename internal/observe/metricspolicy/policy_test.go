package metricspolicy

import (
	"reflect"
	"testing"
)

func TestPolicyDeterministicIndependentBudgetsAndPins(t *testing.T) {
	cfg := Config{HashVersion: HashV1, Salt: "fleet", Dimensions: map[Dimension]DimensionConfig{
		TenantID: {Budget: 3, Catalog: []string{"t3", "t1", "t2"}, Pinned: []string{"t3"}},
		Model:    {Budget: 2, Catalog: []string{"m1", "m2"}},
		Tool:     {Budget: 2, Catalog: []string{"z", "a"}},
	}}
	a, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Dimensions[TenantID] = DimensionConfig{Budget: 3, Catalog: []string{"t2", "t3", "t1"}, Pinned: []string{"t3"}}
	b, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a.Admitted(TenantID), b.Admitted(TenantID)) {
		t.Fatalf("order changed admission: %v / %v", a.Admitted(TenantID), b.Admitted(TenantID))
	}
	if a.Map(TenantID, "t3") != "t3" {
		t.Fatal("pin not admitted")
	}
	if a.Map(TenantID, "unknown") != Overflow {
		t.Fatal("unknown not collapsed")
	}
	if a.AdmittedCount(Model) != 1 || a.AdmittedCount(Tool) != 1 || a.Budget(TenantID) != 3 {
		t.Fatal("budgets are not independent")
	}
}

func TestPolicyValidation(t *testing.T) {
	cases := []Config{
		{HashVersion: HashV1, Dimensions: map[Dimension]DimensionConfig{TenantID: {Budget: 0}}},
		{HashVersion: HashV1, Dimensions: map[Dimension]DimensionConfig{TenantID: {Budget: 1, Catalog: []string{"a"}, Pinned: []string{"a"}}}},
		{HashVersion: HashV1, Dimensions: map[Dimension]DimensionConfig{TenantID: {Budget: 1, Catalog: []string{"a", "b"}, Pinned: []string{"a", "b"}}}},
		{HashVersion: HashV1, Dimensions: map[Dimension]DimensionConfig{TenantID: {Budget: 1, Catalog: []string{Overflow}}}},
		{HashVersion: "future", Dimensions: map[Dimension]DimensionConfig{}},
	}
	for i, cfg := range cases {
		if _, err := New(cfg); err == nil {
			t.Fatalf("case %d accepted", i)
		}
	}
}

func TestPolicyBudgetIncludesOverflow(t *testing.T) {
	p, err := New(Config{HashVersion: HashV1, Dimensions: map[Dimension]DimensionConfig{
		TenantID: {Budget: 2, Catalog: []string{"admitted", "collapsed"}},
		Model:    {Budget: 1},
		Tool:     {Budget: 1},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := p.AdmittedCount(TenantID); got != 1 {
		t.Fatalf("tenant admitted count = %d, want 1 reserved slot excluded", got)
	}
	overflows := 0
	for _, value := range []string{"admitted", "collapsed"} {
		if p.Map(TenantID, value) == Overflow {
			overflows++
		}
	}
	if overflows != 1 {
		t.Fatalf("catalog overflow count = %d, want 1", overflows)
	}
	if got := p.AdmittedCount(TenantID) + 1; got != p.Budget(TenantID) {
		t.Fatalf("admitted values plus overflow = %d, want configured budget %d", got, p.Budget(TenantID))
	}
}

func TestCatalogChangeReplacesAtMostOneAdmission(t *testing.T) {
	base := Config{HashVersion: HashV1, Salt: "same", Dimensions: map[Dimension]DimensionConfig{TenantID: {Budget: 4, Catalog: []string{"a", "b", "c", "d", "e"}}}}
	a, _ := New(base)
	base.Dimensions[TenantID] = DimensionConfig{Budget: 4, Catalog: []string{"a", "b", "c", "d", "e", "f"}}
	b, _ := New(base)
	changes := symmetricDifference(a.Admitted(TenantID), b.Admitted(TenantID))
	if changes > 2 {
		t.Fatalf("more than one replacement: %v -> %v", a.Admitted(TenantID), b.Admitted(TenantID))
	}
}

func symmetricDifference(a, b []string) int {
	m := map[string]int{}
	for _, v := range a {
		m[v]++
	}
	for _, v := range b {
		m[v]--
	}
	n := 0
	for _, v := range m {
		if v != 0 {
			n++
		}
	}
	return n
}
