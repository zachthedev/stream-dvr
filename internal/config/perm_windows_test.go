package config

import (
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// assertOwnerOnly reports whether nothing but the running account may reach
// the file.
//
// The mode is not the thing to read here: Go maps a Unix mode to the
// read-only attribute, so os.Stat answers 0666 for a file created 0600. What
// governs the file is its access control list, so that is what is asserted.
func assertOwnerOnly(t *testing.T, path string) {
	t.Helper()

	descriptor, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("reading the access list of %s: %v", path, err)
	}
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatalf("reading the current account: %v", err)
	}

	// SDDL is the readable form of the list. "D:P" marks it protected, which
	// is what stops the directory's inherited entries applying as well.
	sddl := descriptor.String()
	if !strings.Contains(sddl, "D:P") {
		t.Errorf("access list = %q, want the DACL marked protected so nothing is inherited", sddl)
	}
	// Windows renders a well-known account in SDDL by its alias rather than
	// its SID: a runner signed in as the built-in Administrator produces
	// "LA" where an ordinary account produces the full S-1-5-21-...-500.
	// Both name the same account, so the account being looked for is put
	// through the same canonicalization rather than compared as SID text.
	canonical, err := windows.SecurityDescriptorFromString("D:(A;;FA;;;" + user.User.Sid.String() + ")")
	if err != nil {
		t.Fatalf("rendering the expected entry for %s: %v", user.User.Sid, err)
	}
	entry := strings.TrimPrefix(canonical.String(), "D:")
	if !strings.Contains(sddl, entry) {
		t.Errorf("access list = %q, want an entry %s for %s", sddl, entry, user.User.Sid)
	}
	if entries := strings.Count(sddl, "(A;"); entries != 1 {
		t.Errorf("access list = %q, want exactly one allow entry, got %d", sddl, entries)
	}
}
