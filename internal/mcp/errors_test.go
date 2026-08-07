package mcp

import "testing"

func TestMCPErrorCodeValues(t *testing.T) {
	cases := []struct {
		name string
		got  int
		want int
	}{
		{"ToolNotFound", ErrToolNotFound, -32900},
		{"ResourceNotFound", ErrResourceNotFound, -32901},
		{"PromptNotFound", ErrPromptNotFound, -32902},
		{"ListResultEmpty", ErrListResultEmpty, -32903},
		{"ConnectionClosed", ErrConnectionClosed, -32904},
		{"InvalidRequest", ErrInvalidRequest, -32600},
		{"MethodNotFound", ErrMethodNotFound, -32601},
		{"InvalidParams", ErrInvalidParams, -32602},
		{"Internal", ErrInternal, -32603},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %d, want %d", c.name, c.got, c.want)
		}
	}
}

func TestIsMCPErrorCode(t *testing.T) {
	inRange := []int{-32900, -32901, -32999, ErrToolNotFound}
	for _, code := range inRange {
		if !isMCPErrorCode(code) {
			t.Errorf("isMCPErrorCode(%d) = false, want true", code)
		}
	}
	outOfRange := []int{-32899, -33000, -32601, -32600, -32700, 0}
	for _, code := range outOfRange {
		if isMCPErrorCode(code) {
			t.Errorf("isMCPErrorCode(%d) = true, want false", code)
		}
	}
}
