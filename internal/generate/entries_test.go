package generate

import "testing"

func TestDefault_IsInitialized(t *testing.T) {
	if Default == nil {
		t.Fatal("Default registry = nil")
	}
}
