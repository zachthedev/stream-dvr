package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/urfave/cli/v3"

	"zach.tools/go/stream-dvr/internal/escape"
	"zach.tools/go/stream-dvr/internal/fsretry"
	"zach.tools/go/stream-dvr/internal/paths"
	"zach.tools/go/stream-dvr/internal/service"
)

// executableMode keeps an installed binary runnable by its owner. The
// directory above it is what keeps anyone else out.
const executableMode = 0o700

// ///////////////////////////////////////////////
// install
// ///////////////////////////////////////////////

// serviceName is the registration's name on every platform.
const serviceName = paths.BinaryName

// serviceDescription appears in the platform's own tooling.
const serviceDescription = "Records live streams and organizes them into a library."

// removeFile deletes a program file. It is a variable so a test can force
// the path that runs when a platform refuses to delete a running image,
// which is otherwise unreachable on a system that allows it.
var removeFile = os.Remove

func installCommand() *cli.Command {
	return &cli.Command{
		Name:  "install",
		Usage: "register the recorder to start automatically",
		Action: func(_ context.Context, cmd *cli.Command) error {
			if err := noOperands(cmd); err != nil {
				return err
			}
			// The raw flag rather than configFile: an unset value must stay
			// unset. The registration then carries no --config, and the
			// daemon resolves the default itself at every start.
			return runInstall(os.Stdout, cmd.String("config"))
		},
	}
}

func uninstallCommand() *cli.Command {
	return &cli.Command{
		Name:  "uninstall",
		Usage: "remove the automatic startup registration",
		Action: func(_ context.Context, cmd *cli.Command) error {
			if err := noOperands(cmd); err != nil {
				return err
			}
			return runUninstall(os.Stdout)
		},
	}
}

func statusCommand() *cli.Command {
	return &cli.Command{
		Name:  "status",
		Usage: "report whether the recorder is registered to start automatically",
		Action: func(_ context.Context, cmd *cli.Command) error {
			if err := noOperands(cmd); err != nil {
				return err
			}
			return runStatus(os.Stdout)
		},
	}
}

// runInstall registers the recorder with the platform's autostart facility.
func runInstall(out io.Writer, configPath string) error {
	out = styled(out)
	manager, err := service.New()
	if err != nil {
		return err
	}

	running, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating this binary: %w", err)
	}

	executable, copied, err := installBinary(running, paths.ProgramPath())
	if err != nil {
		return err
	}

	args := []string{"serve"}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}

	def := service.Definition{
		Name:        serviceName,
		Description: serviceDescription,
		Executable:  executable,
		Args:        args,
	}

	if err := manager.Install(def); err != nil {
		return startupHint(err)
	}

	registered := []row{{
		State:   outcomePass,
		Label:   "recorder",
		Trailer: trailer(manager.Mechanism(), sessionLimit()),
	}}
	where := [][2]string{
		{"command", escape.Text(executable + " " + joinComma(args))},
	}
	if copied {
		where = append([][2]string{{"program", escape.Text(executable)}}, where...)
	}

	if agent, ok := installAgent(executable, configPath); ok {
		registered = append(registered, agent)
	}

	section(out, "installed", registered)
	pairs(out, "paths", where)
	summary(out,
		fmt.Sprintf("%d %s", len(registered), plural(len(registered), "registration", "registrations")),
		"run '"+paths.BinaryName+" uninstall' to remove "+plural(len(registered), "it", "them"))
	return nil
}

// installBinary puts the running binary where the platform keeps programs. It
// reports the installed path, and whether this call was what put it there.
//
// An autostart entry runs whatever stands at the path it records, forever, so
// that path is a security boundary rather than a convenience. A binary run
// from a downloads folder, or from a directory made at the root of a Windows
// volume, is writable by anyone the directory admits. Replacing it makes them
// the operator at the next boot. Copying under the profile first means the
// recorded path is one only the owner can write.
//
// A binary already in place is left alone, so installing twice copies once
// and re-registering an installed copy over itself is not an error.
func installBinary(running, target string) (string, bool, error) {
	same, err := sameFile(running, target)
	if err != nil {
		return "", false, err
	}
	if same {
		return target, false, nil
	}

	body, err := os.ReadFile(running)
	if err != nil {
		return "", false, fmt.Errorf("reading this binary: %w", err)
	}
	if err := fsretry.MkdirPrivate(filepath.Dir(target), paths.DataDirMode); err != nil {
		return "", false, fmt.Errorf("creating %s: %w", filepath.Dir(target), err)
	}
	// Staged and renamed, so a failure partway leaves the installed copy whole
	// rather than a truncated binary an autostart entry then runs. Windows
	// refuses to replace a running image, which surfaces here as a named error
	// rather than a service that starts nothing.
	if err := fsretry.WriteFilePrivate(context.Background(), target, body, executableMode); err != nil {
		return "", false, fmt.Errorf("installing to %s: %w", target, err)
	}
	return target, true, nil
}

// sameFile reports whether two paths name one file, treating an absent
// target as different rather than as an error.
func sameFile(a, b string) (bool, error) {
	infoA, err := os.Stat(a)
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", a, err)
	}
	infoB, err := os.Stat(b)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading %s: %w", b, err)
	}
	return os.SameFile(infoA, infoB), nil
}

// sessionLimit names what this platform's registration cannot do, or "" for
// one with no such limit.
//
// It is printed at the moment the operator registers the recorder, which is
// the only moment they are thinking about when it runs. Finding out from a
// missing broadcast is the alternative.
func sessionLimit() string {
	if runtime.GOOS != "darwin" {
		return ""
	}
	return "a launchd agent runs while you are signed in; a Mac at the login window records nothing"
}

// installAgent registers the notification helper, on the platform that
// needs one.
//
// A failure here is reported and not returned. The recorder is registered by
// this point and works without notifications. Refusing the whole install over
// a helper would leave an operator with neither.
func installAgent(executable, configPath string) (row, bool) {
	autostart, err := service.NewAgentAutostart()
	if errors.Is(err, service.ErrNoAgent) {
		return row{}, false
	}
	if err != nil {
		return row{
			State:   outcomeWarn,
			Label:   "notifier",
			Trailer: "unavailable: " + escape.Text(err.Error()),
		}, true
	}

	args := []string{"notify-agent"}
	if configPath != "" {
		args = append(args, "--config", configPath)
	}

	if err := autostart.Install(executable, args); err != nil {
		return row{
			State:   outcomeWarn,
			Label:   "notifier",
			Trailer: "could not be registered: " + escape.Text(err.Error()),
		}, true
	}

	// Present tense, because the helper is a command that keeps running.
	// "or run: stream-dvr notify-agent" reads as a one-shot, and an operator
	// who ran it that way waited for a prompt that was never coming.
	return row{
		State: outcomePass,
		Label: "notifier",
		Trailer: trailer(autostart.Mechanism(),
			"starts at your next sign-in, or run '"+paths.BinaryName+
				" notify-agent' now, which keeps running until you stop it"),
	}, true
}

// uninstallAgent removes the notification helper's registration.
//
// It says nothing about a helper that was not registered. Uninstall treats
// an absent entry as already done, so reporting unconditionally would tell
// an operator a helper stopped starting at sign-in on a machine where one
// never did.
func uninstallAgent() (row, bool) {
	autostart, err := service.NewAgentAutostart()
	if err != nil {
		return row{}, false
	}
	installed, err := autostart.Installed()
	if err != nil {
		return row{
			State:   outcomeWarn,
			Label:   "notifier",
			Trailer: "could not be checked: " + escape.Text(err.Error()),
		}, true
	}
	if !installed {
		return row{}, false
	}
	if err := autostart.Uninstall(); err != nil {
		return row{
			State:   outcomeWarn,
			Label:   "notifier",
			Trailer: "could not be removed: " + escape.Text(err.Error()),
		}, true
	}
	return row{
		State:   outcomePass,
		Label:   "notifier",
		Trailer: "no longer starts at sign-in",
	}, true
}

// removeProgram deletes an installed program, and says where it went when
// deleting is refused.
//
// The two answers are the two platform behaviours, reached without asking
// which platform this is. Unix unlinks a running image happily: the name
// goes, the inode survives until the last mapping closes, and the process
// carries on. Windows holds an image section that refuses every delete, so
// the fallback renames instead, which it does allow. Measured: a plain
// delete and a POSIX-semantics delete are both refused there, and a rename
// of a running image succeeds.
//
// Renaming rather than deferring to the next boot, because the delete
// Windows can schedule needs an administrative token and leaves the file
// sitting in the install directory until a reboot nobody asked for.
//
// A returned path is where the program now sits. An empty one means it is
// gone.
func removeProgram(path string) (string, error) {
	if err := removeFile(path); err == nil {
		return "", nil
	}

	// A directory of its own, so a second uninstall cannot collide with
	// what a first one left, and so the leftover is obviously disposable.
	dir, err := os.MkdirTemp("", "stream-dvr-uninstall-")
	if err != nil {
		return "", fmt.Errorf("removing %s: %w", path, err)
	}
	aside := filepath.Join(dir, filepath.Base(path))
	if err := os.Rename(path, aside); err != nil {
		// The empty directory is worth more gone than kept: it names
		// nothing and the failure already names the file.
		_ = os.Remove(dir)
		return "", fmt.Errorf("removing %s: %w", path, err)
	}
	return aside, nil
}

// uninstallBinary removes the copy install made, reporting what happened.
//
// A failure here is reported rather than returned. The registrations are
// already gone by this point, so the recorder no longer starts, and a
// leftover file is not worth failing an uninstall over.
func uninstallBinary() (row, bool) {
	target := paths.ProgramPath()
	if _, err := os.Stat(target); err != nil {
		return row{}, false
	}

	aside, err := removeProgram(target)
	switch {
	case err != nil:
		return row{
			State:   outcomeWarn,
			Label:   "program",
			Trailer: "could not be removed: " + escape.Text(err.Error()),
			Path:    escape.Text(target),
		}, true
	case aside != "":
		return row{
			State:   outcomeWarn,
			Label:   "program",
			Trailer: "a running program cannot delete itself, so it moved to",
			Path:    escape.Text(aside),
		}, true
	}
	return row{State: outcomePass, Label: "program", Trailer: "removed", Path: escape.Text(target)}, true
}

// runUninstall removes the registration.
func runUninstall(out io.Writer) error {
	out = styled(out)
	manager, err := service.New()
	if err != nil {
		return err
	}

	status, err := manager.Status(serviceName)
	if err != nil {
		return err
	}

	removed := make([]row, 0, 3)
	switch status.State {
	case service.StateAbsent:
		removed = append(removed, row{
			State:   outcomeNote,
			Label:   "recorder",
			Trailer: "nothing was registered under " + escape.Text(serviceName),
		})
	default:
		if err := manager.Uninstall(serviceName); err != nil {
			return startupHint(err)
		}
		removed = append(removed, row{
			State:   outcomePass,
			Label:   "recorder",
			Trailer: "removed the " + manager.Mechanism(),
		})
	}

	// The helper has its own registration, which outlives a recorder's that
	// something else removed. It comes out whether or not there was one here
	// to remove. Left behind, it starts a helper at every sign-in for an
	// executable the operator may have already deleted.
	if agent, ok := uninstallAgent(); ok {
		removed = append(removed, agent)
	}

	// Last, because both registrations name this file. Removing it first
	// would leave a task pointing at nothing for as long as the removal
	// after it takes.
	if program, ok := uninstallBinary(); ok {
		removed = append(removed, program)
	}

	section(out, "uninstalled", removed)
	summary(out, fmt.Sprintf("%d %s", len(removed), plural(len(removed), "item", "items")),
		"recordings and configuration are untouched")
	return nil
}

// runStatus reports the registration's condition.
func runStatus(out io.Writer) error {
	out = styled(out)
	manager, err := service.New()
	if err != nil {
		return err
	}

	status, err := manager.Status(serviceName)
	if err != nil {
		return err
	}

	reportStatus(out, status, manager.Mechanism())
	return nil
}

// reportStatus writes what a registration's condition means.
func reportStatus(out io.Writer, status service.Status, mechanism string) {
	state := outcomeNote
	switch status.State {
	case service.StateRunning:
		state = outcomePass
	case service.StateDisabled:
		state = outcomeWarn
	}

	// Disabled looks like a healthy recorder between broadcasts, so the one
	// thing that separates them has to be said rather than shown.
	note := ""
	if status.State == service.StateDisabled {
		note = "it will not start until it is enabled again"
	}

	// The note leads, because a trailer is cut from the right and this is
	// the one line that separates a disabled recorder from a healthy one
	// waiting between broadcasts.
	section(out, "recorder", []row{{
		State:   state,
		Label:   string(status.State),
		Trailer: trailer(note, escape.Text(status.Detail), mechanism),
	}})
	summary(out, "1 registration", "")
}

// startupHint adds the one instruction that resolves a privilege refusal.
//
// The scheduler's own message says only that access was denied, which does
// not tell an operator what to do about it.
func startupHint(err error) error {
	if errors.Is(err, service.ErrUnsupported) {
		return err
	}
	if errors.Is(err, service.ErrElevationRequired) {
		return fmt.Errorf("%w; recording itself needs no elevation, only this one command does", err)
	}
	return err
}
