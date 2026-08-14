package fsretry

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// ///////////////////////////////////////////////
// Helpers
// ///////////////////////////////////////////////

// assertOwnerOnly reports whether nothing but the running account may reach
// the path.
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

// ///////////////////////////////////////////////
// RestrictToOwner
// ///////////////////////////////////////////////

func TestRestrictToOwner_ReplacesAnInheritedAccessList(t *testing.T) {
	// A file created with a Unix mode carries whatever its directory grants,
	// which is the opposite of what the mode claims.
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}

	before, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		t.Fatalf("reading the access list of %s: %v", path, err)
	}
	if strings.Contains(before.String(), "D:P") {
		t.Fatalf("access list = %q, want an inherited list for RestrictToOwner to replace", before)
	}

	if err := RestrictToOwner(path, 0o600); err != nil {
		t.Fatalf("RestrictToOwner() err = %v, want nil", err)
	}
	assertOwnerOnly(t, path)
}

func TestRestrictToOwner_ReportsAMissingFile(t *testing.T) {
	if err := RestrictToOwner(filepath.Join(t.TempDir(), "absent.json"), 0o600); err == nil {
		t.Error("RestrictToOwner() err = nil, want an error for a file that is not there")
	}
}

// ///////////////////////////////////////////////
// WriteFilePrivate
// ///////////////////////////////////////////////

func TestWriteFilePrivate_PublishesAFileNothingElseCanRead(t *testing.T) {
	// The access has to survive the rename, or restricting the staged file
	// would buy nothing over repairing the published one.
	path := filepath.Join(t.TempDir(), "credentials.json")

	if err := WriteFilePrivate(context.Background(), path, []byte("{}"), 0o600); err != nil {
		t.Fatalf("WriteFilePrivate() err = %v, want nil", err)
	}
	assertOwnerOnly(t, path)
}

func TestWriteFilePrivate_LeavesNoWiderFileWhenItReplacesOne(t *testing.T) {
	// The rename replaces whatever stood at the path, and a file already
	// there must not lend its access to the one that replaces it.
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatalf("writing the file already there: %v", err)
	}

	if err := WriteFilePrivate(context.Background(), path, []byte("new"), 0o600); err != nil {
		t.Fatalf("WriteFilePrivate() err = %v, want nil", err)
	}
	assertOwnerOnly(t, path)

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if string(body) != "new" {
		t.Errorf("contents = %q, want %q", body, "new")
	}
}
