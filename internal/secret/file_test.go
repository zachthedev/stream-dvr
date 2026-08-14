package secret

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// ///////////////////////////////////////////////
// File
// ///////////////////////////////////////////////

func TestFile_ReportsAnAccountBeforeAnythingIsStored(t *testing.T) {
	// The state of every machine before the operator authenticates, and
	// there is no file at all yet. Reporting it as a failure would make a
	// first run look broken.
	_, err := NewFile(t.TempDir()).Get(AccountTwitch)

	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get() err = %v, want ErrNotFound", err)
	}
}

func TestFile_SurvivesBeingReopened(t *testing.T) {
	// The whole point of a file rather than memory: the daemon and the
	// interactive command are different processes.
	dir := t.TempDir()
	if err := NewFile(dir).Set(AccountTwitch, sentinelToken); err != nil {
		t.Fatalf("Set() err = %v, want nil", err)
	}

	got, err := NewFile(dir).Get(AccountTwitch)
	if err != nil {
		t.Fatalf("Get() err = %v, want nil", err)
	}
	if got != sentinelToken {
		t.Errorf("Get() = %q, want the stored value", got)
	}
}

func TestFile_KeepsTheFileToItsOwner(t *testing.T) {
	// The token sits here for as long as the recorder runs, so the mode is
	// the only thing between it and another account on the machine.
	if runtime.GOOS == "windows" {
		t.Skip("Go's perm bits do not carry on Windows; the ACL is inherited from the data directory")
	}
	dir := t.TempDir()
	store := NewFile(dir)
	if err := store.Set(AccountTwitch, sentinelToken); err != nil {
		t.Fatalf("Set() err = %v, want nil", err)
	}

	info, err := os.Stat(store.Path())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if got := info.Mode().Perm(); got != fileMode {
		t.Errorf("mode = %o, want %o", got, fileMode)
	}
}

func TestFile_KeepsAccountsApart(t *testing.T) {
	store := NewFile(t.TempDir())
	if err := store.Set(AccountTwitch, sentinelToken); err != nil {
		t.Fatalf("Set() err = %v, want nil", err)
	}
	if err := store.Set("elsewhere", "OTHERVALUE"); err != nil {
		t.Fatalf("Set() err = %v, want nil", err)
	}

	if got, _ := store.Get(AccountTwitch); got != sentinelToken {
		t.Errorf("Get(twitch) = %q, want its own value", got)
	}
	if got, _ := store.Get("elsewhere"); got != "OTHERVALUE" {
		t.Errorf("Get(elsewhere) = %q, want its own value", got)
	}
}

func TestFile_DeletingOneLeavesTheOthers(t *testing.T) {
	// Logging out of one provider must not sign the operator out of every
	// other one.
	store := NewFile(t.TempDir())
	for account, value := range map[string]string{AccountTwitch: sentinelToken, "elsewhere": "OTHER"} {
		if err := store.Set(account, value); err != nil {
			t.Fatalf("Set() err = %v, want nil", err)
		}
	}

	if err := store.Delete(AccountTwitch); err != nil {
		t.Fatalf("Delete() err = %v, want nil", err)
	}
	if _, err := store.Get(AccountTwitch); !errors.Is(err, ErrNotFound) {
		t.Errorf("the deleted account survived: %v", err)
	}
	if got, _ := store.Get("elsewhere"); got != "OTHER" {
		t.Errorf("Get(elsewhere) = %q, want it untouched", got)
	}
}

func TestFile_DeleteIsSafeToRepeat(t *testing.T) {
	store := NewFile(t.TempDir())

	for range 2 {
		if err := store.Delete(AccountTwitch); err != nil {
			t.Errorf("Delete() err = %v, want nil", err)
		}
	}
}

func TestFile_RefusesToReadOverAFileItCannotParse(t *testing.T) {
	// Treating a damaged file as empty would overwrite it on the next Set,
	// discarding a credential the operator would have to obtain again. A
	// refusal is recoverable. A silent overwrite is not.
	dir := t.TempDir()
	store := NewFile(dir)
	if err := os.WriteFile(store.Path(), []byte("{not json"), 0o600); err != nil {
		t.Fatalf("writing the damaged file: %v", err)
	}

	if _, err := store.Get(AccountTwitch); err == nil || errors.Is(err, ErrNotFound) {
		t.Errorf("Get() err = %v, want a failure that is not ErrNotFound", err)
	}
	if err := store.Set(AccountTwitch, sentinelToken); err == nil {
		t.Error("Set() overwrote a file it could not read")
	}
}

func TestFile_ReplacesAtomicallyRatherThanTruncating(t *testing.T) {
	// A refresh token is spent when it is used, so a half-written file
	// during rotation costs the session. Nothing partial survives the
	// replacement, and the content is always complete JSON.
	dir := t.TempDir()
	store := NewFile(dir)
	if err := store.Set(AccountTwitch, sentinelToken); err != nil {
		t.Fatalf("Set() err = %v, want nil", err)
	}

	body, err := os.ReadFile(store.Path())
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	var decoded map[string]string
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Errorf("the stored file is not complete JSON: %v", err)
	}

	// The temporary file used for the replacement must not survive it.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading the directory: %v", err)
	}
	for _, entry := range entries {
		if entry.Name() != FileName {
			t.Errorf("left %q behind beside the credential file", entry.Name())
		}
	}
}

func TestFile_SerialisesConcurrentWriters(t *testing.T) {
	// Two callers rotating at once would each write the whole map back, and
	// the loser's change would vanish. Every account must survive.
	store := NewFile(t.TempDir())
	accounts := []string{"one", "two", "three", "four", "five", "six", "seven", "eight"}

	var wg sync.WaitGroup
	for _, account := range accounts {
		wg.Go(func() {
			if err := store.Set(account, "value-"+account); err != nil {
				t.Errorf("Set(%s) err = %v, want nil", account, err)
			}
		})
	}
	wg.Wait()

	for _, account := range accounts {
		got, err := store.Get(account)
		if err != nil {
			t.Errorf("Get(%s) err = %v: a concurrent write was lost", account, err)
			continue
		}
		if got != "value-"+account {
			t.Errorf("Get(%s) = %q, want its own value", account, got)
		}
	}
}

func TestFile_RefusesASecretNoStoreWouldAccept(t *testing.T) {
	err := NewFile(t.TempDir()).Set(AccountTwitch, strings.Repeat("x", maxSecretBytes+1))

	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("Set() err = %v, want ErrTooLarge", err)
	}
}

func TestFile_CreatesTheDirectoryItNeeds(t *testing.T) {
	// A first run authenticates before anything else has made the data
	// directory.
	store := NewFile(filepath.Join(t.TempDir(), "not", "there", "yet"))

	if err := store.Set(AccountTwitch, sentinelToken); err != nil {
		t.Errorf("Set() err = %v, want nil for an absent directory", err)
	}
}

func TestFile_SatisfiesTheStoreInterface(t *testing.T) {
	var _ Store = NewFile(t.TempDir())
}

func TestFile_LockExcludesASecondHandle(t *testing.T) {
	// A refresh token is one time use. Two handles reading the same map and
	// each writing the whole of it back leaves the loser's rotation on
	// disk, naming a token already spent at the provider: the session is
	// dead and nothing says so until the next renewal hours later. The
	// daemon, an interactive auth command and doctor each build their own
	// handle, so a mutex held in memory guards none of them against the
	// others.
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)

	held := NewFile(path)
	release, err := held.lock()
	if err != nil {
		t.Fatalf("lock() err = %v, want nil", err)
	}

	// A different handle, which is what a second process amounts to here.
	other := NewFile(path)
	if _, err := other.lock(); err == nil {
		t.Error("a second handle took the lock while the first held it")
	}

	release()
	next, err := other.lock()
	if err != nil {
		t.Fatalf("lock() err = %v after release, want it available", err)
	}
	next()
}

func TestFile_LockIsNotHeldByAProcessThatDied(t *testing.T) {
	// A lock left behind by a killed process must not hold the credential
	// store for the life of the machine.
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)

	stale := path + lockSuffix
	if err := os.WriteFile(stale, nil, fileMode); err != nil {
		t.Fatalf("staging a stale lock: %v", err)
	}
	old := time.Now().Add(-2 * lockStale)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatalf("ageing the stale lock: %v", err)
	}

	release, err := NewFile(path).lock()
	if err != nil {
		t.Fatalf("lock() err = %v, want a stale lock to be broken", err)
	}
	release()
}
