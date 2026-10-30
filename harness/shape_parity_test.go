package harness_test

import (
	"encoding/json"
	"os"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/ethanhinson/fuse/harness"
	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
)

// shapeCase is one entry of testdata/shape_cases.json. The SAME file drives the
// TS SDK's validateHarnessShape test (sdk/ts/test/harness.test.ts), so the two
// languages are held to identical field paths and rule ids for the same input.
type shapeCase struct {
	Name string          `json:"name"`
	Spec json.RawMessage `json:"spec"`
	Want []struct {
		FieldPath string `json:"fieldPath"`
		RuleID    string `json:"ruleId"`
	} `json:"want"`
}

func TestValidateShapeSharedCases(t *testing.T) {
	data, err := os.ReadFile("testdata/shape_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []shapeCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			spec := &harnessv1.HarnessSpec{}
			if err := protojson.Unmarshal(tc.Spec, spec); err != nil {
				t.Fatalf("fixture spec: %v", err)
			}
			got := harness.ValidateShape(spec)
			if len(got) != len(tc.Want) {
				t.Fatalf("got %+v, want %+v", got, tc.Want)
			}
			for i, w := range tc.Want {
				if got[i].FieldPath != w.FieldPath || got[i].RuleID != w.RuleID {
					t.Errorf("violation %d = {%s %s}, want {%s %s}", i, got[i].FieldPath, got[i].RuleID, w.FieldPath, w.RuleID)
				}
			}
		})
	}
}
