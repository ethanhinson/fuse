package harness

import (
	"fmt"

	harnessv1 "github.com/ethanhinson/fuse/harness/v1"
)

// ValidationError reports a spec a fuse server refused to start a loop from
// (change 0085): every violation, each with the field path a form places it
// against. The Go SDK returns it from StartLoop for both the in-process and the
// networked backends; match it with errors.As.
//
// On the Connect wire it travels as the single detail of an InvalidArgument
// error, a fuse.harness.v1.ValidateHarnessResponse (Proto), the same message
// HarnessService.ValidateHarness returns, so a client renders both the same way.
type ValidationError struct {
	Violations []Violation
}

// Error summarizes the violations: the count and the first one.
func (e *ValidationError) Error() string {
	n := len(e.Violations)
	if n == 0 {
		return "harness spec: invalid"
	}
	noun := "violations"
	if n == 1 {
		noun = "violation"
	}
	v := e.Violations[0]
	where := v.Message
	if v.FieldPath != "" {
		where = v.FieldPath + ": " + v.Message
	}
	more := ""
	if n > 1 {
		more = "; ..."
	}
	return fmt.Sprintf("harness spec: %d %s: %s (%s)%s", n, noun, where, v.RuleID, more)
}

// Proto returns the violations as the wire detail message.
func (e *ValidationError) Proto() *harnessv1.ValidateHarnessResponse {
	resp := &harnessv1.ValidateHarnessResponse{Violations: make([]*harnessv1.Violation, 0, len(e.Violations))}
	for _, v := range e.Violations {
		resp.Violations = append(resp.Violations, v.Proto())
	}
	return resp
}

// ValidationErrorFromProto rebuilds a ValidationError from its wire detail.
func ValidationErrorFromProto(m *harnessv1.ValidateHarnessResponse) *ValidationError {
	e := &ValidationError{Violations: make([]Violation, 0, len(m.GetViolations()))}
	for _, v := range m.GetViolations() {
		e.Violations = append(e.Violations, Violation{FieldPath: v.GetFieldPath(), Message: v.GetMessage(), RuleID: v.GetRuleId()})
	}
	return e
}
