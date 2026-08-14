package secret

import (
	"errors"
	"strings"
	"testing"
)

// ///////////////////////////////////////////////
// Fixtures
// ///////////////////////////////////////////////

// sentinelToken stands in for a credential. It is obviously fake and fixed
// length, so a leak is recognisable in any output a test captures.
const sentinelToken = "EXAMPLETOKENEXAMPLETOKEN123456"

// ///////////////////////////////////////////////
// Memory
// ///////////////////////////////////////////////

func TestMemory_ReportsAnAccountNothingIsStoredUnder(t *testing.T) {
	// The ordinary state before the operator authenticates. Reporting it as
	// a failure would make a first run look broken.
	_, err := NewMemory().Get(AccountTwitch)

	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get() err = %v, want ErrNotFound", err)
	}
}

func TestMemory_ReturnsWhatItWasGiven(t *testing.T) {
	store := NewMemory()
	if err := store.Set(AccountTwitch, sentinelToken); err != nil {
		t.Fatalf("Set() err = %v, want nil", err)
	}

	got, err := store.Get(AccountTwitch)
	if err != nil {
		t.Fatalf("Get() err = %v, want nil", err)
	}
	if got != sentinelToken {
		t.Errorf("Get() = %q, want the stored value", got)
	}
}

func TestMemory_KeepsAccountsApart(t *testing.T) {
	// One service holds every credential this project has, so an account
	// that leaked into another would hand the wrong token to the wrong tool.
	store := NewMemory()
	if err := store.Set(AccountTwitch, sentinelToken); err != nil {
		t.Fatalf("Set() err = %v, want nil", err)
	}

	if _, err := store.Get("somewhere-else"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get() on another account err = %v, want ErrNotFound", err)
	}
}

func TestMemory_DeleteIsSafeToRepeat(t *testing.T) {
	// Logout runs twice for anyone unsure whether the first took. The second
	// is not a failure.
	store := NewMemory()
	if err := store.Set(AccountTwitch, sentinelToken); err != nil {
		t.Fatalf("Set() err = %v, want nil", err)
	}

	for range 2 {
		if err := store.Delete(AccountTwitch); err != nil {
			t.Errorf("Delete() err = %v, want nil", err)
		}
	}
	if _, err := store.Get(AccountTwitch); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get() after Delete err = %v, want ErrNotFound", err)
	}
}

func TestMemory_OverwritesRatherThanAccumulating(t *testing.T) {
	// Re-authenticating replaces the credential. A store that kept the old
	// one would hand a dead token to the next reader.
	store := NewMemory()
	if err := store.Set(AccountTwitch, "first"); err != nil {
		t.Fatalf("Set() err = %v, want nil", err)
	}
	if err := store.Set(AccountTwitch, sentinelToken); err != nil {
		t.Fatalf("Set() err = %v, want nil", err)
	}

	if got, _ := store.Get(AccountTwitch); got != sentinelToken {
		t.Errorf("Get() = %q, want the newer value", got)
	}
}

func TestMemory_UsableAsItsZeroValue(t *testing.T) {
	// A fake reached through the interface is often written as &Memory{}.
	// One that panicked there would fail every test using it, for reasons
	// that have nothing to do with the code under test.
	var store Memory

	if err := store.Set(AccountTwitch, sentinelToken); err != nil {
		t.Fatalf("Set() on a zero Memory err = %v, want nil", err)
	}
	if got, _ := store.Get(AccountTwitch); got != sentinelToken {
		t.Errorf("Get() = %q, want the stored value", got)
	}
}

// ///////////////////////////////////////////////
// Size
// ///////////////////////////////////////////////

func TestSet_RefusesASecretNoStoreWouldAccept(t *testing.T) {
	// The bound is what stops a caller writing a whole response body in by
	// mistake. Nothing legitimate comes near it.
	err := NewMemory().Set(AccountTwitch, strings.Repeat("x", maxSecretBytes+1))

	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("Set() err = %v, want ErrTooLarge", err)
	}
}

func TestSet_RefusalNamesNeitherTheSecretNorItsLength(t *testing.T) {
	// An error is where a credential reliably escapes: it gets wrapped,
	// logged, and pasted into a bug report.
	oversized := strings.Repeat(sentinelToken, 200)

	err := NewMemory().Set(AccountTwitch, oversized)
	if err == nil {
		t.Fatal("Set() err = nil for an oversized secret, want a refusal")
	}
	if strings.Contains(err.Error(), sentinelToken) {
		t.Errorf("the refusal carries the secret: %q", err.Error())
	}
}

// ///////////////////////////////////////////////
// The interface
// ///////////////////////////////////////////////

func TestStore_IsSatisfiedByBothImplementations(t *testing.T) {
	// Memory is what every other package's tests substitute for File, so
	// the two staying interchangeable is what keeps those tests honest.
	var _ Store = NewMemory()
	var _ Store = NewFile(t.TempDir())
}
