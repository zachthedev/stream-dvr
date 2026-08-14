//go:build !dev

package library

import "testing"

// TestBuildOwner_IsProd pins the default build to production libraries. A
// released binary that identified as dev would open a sandbox, and one that
// identified as neither would open nothing.
func TestBuildOwner_IsProd(t *testing.T) {
	if BuildOwner != OwnerProd {
		t.Errorf("BuildOwner = %q, want %q in an untagged build", BuildOwner, OwnerProd)
	}
}
