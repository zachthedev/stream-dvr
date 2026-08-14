package fsretry

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// ///////////////////////////////////////////////
// Ownership
// ///////////////////////////////////////////////

// RestrictToOwner replaces a file or directory's access control list with
// one naming only the account this process runs as.
//
// A Unix mode is not a permission on Windows. Go maps it to the read-only
// attribute and nothing else, so the 0600 a file is created with says
// nothing about who may read it. os.Stat reports 0666 back. What governs
// the file is the list it inherits from its directory, which is owner-only
// under a user profile and whatever the volume hands out anywhere else. A
// data directory holds a live OAuth token, the recording credential, and a
// config carrying a webhook URL that is itself a credential.
//
// perm is ignored here and is taken so the two platforms share one
// signature.
//
// The list is marked protected. Without that flag the explicit entry joins
// the inherited ones rather than replacing them.
func RestrictToOwner(path string, perm os.FileMode) error {
	_ = perm

	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return fmt.Errorf("reading the account that owns %s: %w", path, err)
	}

	list, err := windows.ACLFromEntries([]windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Inheritance:       windows.NO_INHERITANCE,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeType:  windows.TRUSTEE_IS_USER,
			TrusteeValue: windows.TrusteeValueFromSID(user.User.Sid),
		},
	}}, nil)
	if err != nil {
		return fmt.Errorf("building an access list for %s: %w", path, err)
	}

	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, list, nil); err != nil {
		return fmt.Errorf("restricting %s to its owner: %w", path, err)
	}
	return nil
}
