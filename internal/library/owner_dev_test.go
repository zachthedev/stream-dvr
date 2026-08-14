//go:build dev

package library

import "testing"

// TestBuildOwner_IsDev pins the dev-tagged build to sandbox libraries. This
// is the barrier that keeps development off a real archive, so it fails
// loudly rather than silently widening access.
func TestBuildOwner_IsDev(t *testing.T) {
	if BuildOwner != OwnerDev {
		t.Errorf("BuildOwner = %q, want %q in a build tagged dev", BuildOwner, OwnerDev)
	}
}
