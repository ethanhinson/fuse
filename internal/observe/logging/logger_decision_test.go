package logging

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ethanhinson/fuse/internal/observe"
)

// TestProjectWritesDecisionEnums pins that the permission-decision enums
// survive the Entry→wire re-marshaling in write() (they were silently dropped
// by the explicit field list once already).
func TestProjectWritesDecisionEnums(t *testing.T) {
	var buf bytes.Buffer
	l := New(&buf, nil, Identity{Service: "fuse", Instance: "test"})
	err := l.Project(context.Background(), observe.Record{
		Timestamp: time.Now(), EventName: "permission.decision",
		Operation: observe.OperationPermission, Outcome: observe.OutcomeSuccess,
		Verdict: "deny", DecisionLayer: "classifier", ClassifierOutcome: "truncated",
	})
	if err != nil {
		t.Fatal(err)
	}
	line := buf.String()
	for _, want := range []string{`"verdict":"deny"`, `"decision_layer":"classifier"`, `"classifier_outcome":"truncated"`} {
		if !strings.Contains(line, want) {
			t.Fatalf("log line missing %s: %s", want, line)
		}
	}
}
