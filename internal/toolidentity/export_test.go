package toolidentity

// rawTokenForTest exposes the unredacted token to same-package tests only. It is
// compiled solely into the test binary (export_test.go), so production code can
// never reach the token except through Header.
func (c Credential) rawTokenForTest() string { return c.Token }
