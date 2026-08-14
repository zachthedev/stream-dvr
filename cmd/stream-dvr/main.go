// Command stream-dvr records live streams and manages the resulting library.
// This file wires the CLI. Every behavior lives in an internal package.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	// Windows ships no IANA timezone database, so time.LoadLocation fails
	// there for every named zone. Embedding it costs a few hundred
	// kilobytes. It buys naming.timezone meaning the same thing on every
	// platform, which a portable library needs.
	_ "time/tzdata"

	"github.com/urfave/cli/v3"

	"zach.tools/go/stream-dvr/internal/config"
	"zach.tools/go/stream-dvr/internal/deps"
	"zach.tools/go/stream-dvr/internal/escape"
	"zach.tools/go/stream-dvr/internal/library"
	"zach.tools/go/stream-dvr/internal/paths"
	"zach.tools/go/stream-dvr/internal/post"
	"zach.tools/go/stream-dvr/internal/providers/twitch"
	"zach.tools/go/stream-dvr/internal/secret"
	"zach.tools/go/stream-dvr/internal/space"
	"zach.tools/go/stream-dvr/internal/store"
	"zach.tools/go/stream-dvr/internal/version"
)

// resolveFunc reports the state of every external tool. deps.ResolveAll
// satisfies it. A test substitutes a fixed set of results.
type resolveFunc func(context.Context) []deps.Resolution

// encodersFunc lists the encoders this machine can actually run.
// (*post.Pipeline).Encoders satisfies it. A test substitutes a fixed set, so
// no probe spawns ffmpeg.
type encodersFunc func(context.Context) ([]post.Encoder, error)

// ///////////////////////////////////////////////
// Entrypoint
// ///////////////////////////////////////////////

func main() {
	// SIGTERM is how Unix asks the daemon to stop. `systemctl stop` and
	// `disable --now` both send it, and this program installs the unit that
	// issues them. Unhandled, it is fatal by default, so an in-flight capture
	// dies mid-write and the daemon's clean shutdown never runs. Windows never
	// delivers it, so listening there costs nothing and keeps one signal set
	// for every platform.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := newApp().Run(ctx, os.Args); err != nil {
		refuse(styled(os.Stderr), err)
		os.Exit(1)
	}
}

// refuse reports an error as a result row of one.
//
// Cause and hint arrive as a single string, because an error crosses a
// package boundary as an error rather than as a pair. A semicolon is how this
// codebase writes the two halves: what went wrong, then what to do about it.
//
// Every error the tree can produce ends here, and most of them carry text
// somebody else wrote: a stored path, a subprocess's output, a remote
// message. This is a terminal.
//
// G705: escape.Text is the sanitizer, and the sink is stderr rather than a web response
func refuse(out io.Writer, err error) {
	// Cut first, escape after. Escaping the whole message and cutting the
	// result splits inside whatever escape.Text produced: a message
	// carrying a newline comes back as one quoted literal, so the cut
	// lands mid-literal, the hint is a fragment, and the opening quote is
	// never closed. A config with two problems and a mistyped subcommand
	// both carry one.
	cause, hint, _ := strings.Cut(err.Error(), "; ")
	failure(out, escapeLines(cause), escapeLines(hint))
}

// escapeLines renders each line of a message on its own.
//
// A message spanning lines is this build's own layout, and failure()
// already indents one. Handing the whole thing to escape.Text would make
// the newline the reason it quotes, so every line arrives as the two
// characters backslash-n and every Windows path separator doubles, leaving
// a path nobody can copy. Untrusted values are escaped where they are
// interpolated, so the only line breaks left here are the ones this build
// wrote.
func escapeLines(text string) string {
	if text == "" {
		return ""
	}

	lines := strings.Split(text, "\n")
	for i, line := range lines {
		lines[i] = escape.Text(line)
	}
	return strings.Join(lines, "\n")
}

// newApp builds the command tree.
//
// Running the binary with no subcommand opens the calendar, because that is
// what using a DVR means. Everything under the root is a named operation on
// the library or the recorder.
func newApp() *cli.Command {
	return &cli.Command{
		Name:  paths.BinaryName,
		Usage: "record live streams and manage the recording library",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "config",
				Usage: "config file, defaulting to the one in the data directory",
				// Inherited by every subcommand, so one flag answers
				// "which config" at any depth. A subcommand declaring
				// its own --config would shadow this one. A value given
				// ahead of the subcommand name would then be parsed and
				// never read.
			},
			&cli.BoolFlag{
				Name:  "monday",
				Usage: "start calendar weeks on Monday",
				// Only the calendar has weeks. Local keeps a subcommand
				// from accepting the flag and silently doing nothing.
				Local: true,
			},
		},
		Commands: []*cli.Command{
			versionCommand(),
			doctorCommand(),
			configCommand(),
			libraryCommand(),
			serveCommand(),
			backfillCommand(),
			notifyAgentCommand(),
			authCommand(),
			installCommand(),
			uninstallCommand(),
			statusCommand(),
		},
		Action: runRoot,
	}
}

// unknownCommand reports an argument that named no command.
//
// Every command carrying subcommands needs this as its own action. The root
// action runs for any argument matching no subcommand, so without it a typo
// opens the calendar as though nothing had been typed. A parent with no action
// at all falls through to urfave's help-topic lookup. That prints "No help
// topic for 'bogus'" at exit 3, and names neither what was typed nor what
// would have been valid.
func unknownCommand(cmd *cli.Command) error {
	if name := cmd.Args().First(); name != "" {
		return fmt.Errorf("unknown command %s\n  run %s --help for the list",
			escape.Text(name), cmd.FullName())
	}
	return fmt.Errorf("%s needs a subcommand\n  run %s --help for the list",
		cmd.FullName(), cmd.FullName())
}

// noOperands rejects a positional argument the command has no use for.
//
// urfave hands extra arguments to the action, which ignores them. `config init
// extra` writes the config and exits 0, so a mistyped flag that landed as an
// operand reads as though it applied.
func noOperands(cmd *cli.Command) error {
	if extra := cmd.Args().First(); extra != "" {
		return fmt.Errorf("%s takes no arguments, got %s", cmd.FullName(), escape.Text(extra))
	}
	return nil
}

// oneOperand returns the single positional argument a command takes.
func oneOperand(cmd *cli.Command, what string) (string, error) {
	args := cmd.Args()
	if args.Len() > 1 {
		return "", fmt.Errorf("%s takes one %s, got %d arguments", cmd.FullName(), what, args.Len())
	}
	return args.First(), nil
}

// configFile resolves the config file a command acts on. Every command
// reads the one persistent flag, so the answer to "which config" does not
// depend on where in the tree it is asked.
func configFile(cmd *cli.Command) string {
	if explicit := cmd.String("config"); explicit != "" {
		return explicit
	}
	return paths.ConfigPath(paths.DataDir())
}

// ///////////////////////////////////////////////
// version
// ///////////////////////////////////////////////

func versionCommand() *cli.Command {
	return &cli.Command{
		Name:  "version",
		Usage: "print build version",
		Action: func(_ context.Context, cmd *cli.Command) error {
			if err := noOperands(cmd); err != nil {
				return err
			}
			fmt.Fprintln(os.Stdout, versionLine())
			return nil
		},
	}
}

// versionLine describes this binary in one line.
//
// The lineage is named only when it is the sandbox one. A build tagged dev
// refuses to open a library a released binary made. An operator holding one
// without knowing it reads that refusal as a damaged library. The ordinary
// build says nothing, because every operator already assumes a binary opens
// real libraries, and a note claiming it sits oddly beside a version reading
// 0.0.0-dev.
func versionLine() string {
	line := fmt.Sprintf("%s %s", paths.BinaryName, version.Info())
	if library.BuildOwner == library.OwnerDev {
		return line + " (dev sandbox: refuses a library a released build made)"
	}
	return line
}

// ///////////////////////////////////////////////
// doctor
// ///////////////////////////////////////////////

func doctorCommand() *cli.Command {
	return &cli.Command{
		Name:  "doctor",
		Usage: "check external tools, data directory, and library",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:  "library",
				Usage: "also check the library rooted at this path",
			},
			&cli.BoolFlag{
				Name:    "verbose",
				Aliases: []string{"v"},
				Usage:   "show the resolved path of each tool",
			},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if err := noOperands(cmd); err != nil {
				return err
			}
			return runDoctor(ctx, os.Stdout, deps.ResolveAll, post.New().Encoders,
				configFile(cmd), cmd.String("library"), cmd.Bool("verbose"))
		},
	}
}

// runDoctor reports the state of every external dependency and location
// stream-dvr needs, returning an error when a required piece is missing.
func runDoctor(ctx context.Context, out io.Writer, resolve resolveFunc, encoders encodersFunc,
	configPath, libraryRoot string, verbose bool,
) error {
	out = styled(out)
	checks, failures := 0, 0

	// Resolved once. Each entry costs a process spawn to read a version, so
	// a second call would run every tool on the machine again.
	resolutions := resolve(ctx)
	tools := make([]row, 0, len(resolutions)+1)
	for _, res := range resolutions {
		if res.Err != nil {
			failures++
		}
		tools = append(tools, dependencyRow(res, verbose))
	}
	// 'config validate' reports a config that will not load, and names which
	// field and why. Here the defaults answer the machine question, so
	// doctor stays useful on a machine nobody has configured yet.
	//
	// The error is carried rather than dropped. Two rows below read config,
	// and a load failure zeroes every field it would have set: without this
	// the metadata row would report an application id nobody registered
	// when the real fault is an unrelated field elsewhere in the file.
	cfg, cfgErr := config.Load(configPath)
	if cfgErr != nil {
		cfg = config.DefaultConfig()
	}
	tools = append(tools, recompressRow(ctx, encoders, cfg.Space.Recompress))
	checks += len(tools)
	section(out, "dependencies", tools)

	dataDir := paths.DataDir()
	credentials := []row{
		credentialRow(dataDir, verbose),
		metadataRow(cfg.Twitch.ClientID, cfgErr, dataDir, time.Now()),
	}
	checks += len(credentials)
	section(out, "credentials", credentials)

	// Its own block rather than a line among the paths below. Opening a
	// library either works or does not, and a check that can fail does not
	// belong in a list of directories that only ever report where they are.
	where := [][2]string{
		{"data dir", dataDir},
		{"config", paths.ConfigPath(dataDir)},
	}
	if libraryRoot != "" {
		opened, ok := libraryRow(libraryRoot)
		if !ok {
			failures++
		}
		checks++
		section(out, "library", []row{opened})
		where = append(where, [2]string{"library", escape.Text(libraryRoot)})
	}

	pairs(out, "locations", where)

	counted := fmt.Sprintf("%d %s, all passed", checks, plural(checks, "check", "checks"))
	if failures == 0 {
		summary(out, counted, "run 'stream-dvr serve' to start recording")
		return nil
	}

	// No next action on a failure. Each row above names what is missing and
	// what it was needed for, and a generic instruction under them would say
	// less than the rows already did.
	summary(out, fmt.Sprintf("%d %s, %d failed", checks, plural(checks, "check", "checks"), failures), "")
	// A sentence rather than the count, because summary just printed the
	// count and refuse prints whatever this returns. The same figure twice
	// under each other reads as two different results.
	return errors.New("some checks did not pass")
}

// credentialRow reports whether a Twitch credential is in place.
//
// It reads the derived file rather than the credential store, for the reason
// the daemon does: doctor can run from a scheduled task, and an S4U logon
// cannot reach the store. It never asks Twitch either, because doctor answers
// about this machine and must work offline.
//
// The login is shown only under --verbose. It is the operator's real identity,
// and doctor output is the kind of thing pasted into a bug report.
func credentialRow(dataDir string, verbose bool) row {
	path := twitch.AuthConfigPath(dataDir)
	if _, err := os.Stat(path); err != nil {
		// Not a failure. Recording public streams is a supported way to run
		// this, and a first install has no credential by definition.
		return row{
			State:   outcomeNote,
			Label:   "twitch",
			Trailer: "no token stored, subscriber streams record as public",
		}
	}

	detail := "a token is stored"
	if verbose {
		detail = trailer(detail, "run 'stream-dvr auth twitch status' to check it still works")
	}
	return row{State: outcomePass, Label: "twitch", Trailer: detail}
}

// metadataRow reports on the Twitch metadata session, which is a different
// credential from the one above and is not interchangeable with it.
//
// It never fails a check, whatever it finds. The metadata API is an
// optimisation: it makes listing a channel's past broadcasts one request
// rather than one per broadcast. A machine without it records and recovers
// exactly the same, only slower. A doctor that failed over it would teach the
// operator to ignore doctor.
//
// Offline, like the row above. Nothing here asks Twitch, and nothing here
// refreshes. A status command that spent the one-time refresh token would
// rotate the session just by reporting on it.
func metadataRow(clientID string, cfgErr error, dataDir string, now time.Time) row {
	if cfgErr != nil {
		// The config never loaded, so the id is absent for a reason that
		// has nothing to do with Twitch. Sending the operator to register
		// an application would be an instruction to fix the wrong thing.
		return row{
			State: outcomeNote,
			Label: "twitch api",
			Trailer: "config did not load, so no application id was read; " +
				"run 'stream-dvr config validate' to see what is wrong with it",
		}
	}
	if clientID == "" {
		// An instruction rather than a note, because the operator is the
		// one who fixes this: the id is config, and registering an
		// application is the step nothing else can do for them.
		return row{
			State: outcomeNote,
			Label: "twitch api",
			Trailer: "no application id, register one at dev.twitch.tv/console/apps " +
				"and set twitch.client_id; recovery lists one broadcast per request until then",
		}
	}

	expiresAt, stored := twitch.NewSession(
		clientID, secret.NewFile(dataDir), secret.AccountTwitchAPI, nil).Stored()
	if !stored {
		return row{
			State:   outcomeNote,
			Label:   "twitch api",
			Trailer: "not authorized, run 'stream-dvr auth twitch metadata' to speed up recovery",
		}
	}

	// An expired access token is the ordinary state between passes: it lives
	// about four hours and is renewed from the refresh half on next use. Only
	// a session left idle long enough for Twitch to forget the refresh token
	// needs the operator, and nothing offline can tell the two apart.
	if !now.Before(expiresAt) {
		return row{
			State:   outcomePass,
			Label:   "twitch api",
			Trailer: "authorized, the access token renews on next use",
		}
	}
	return row{State: outcomePass, Label: "twitch api", Trailer: "authorized"}
}

// dependencyRow renders one tool's result.
//
// The version is a line of a subprocess's stdout and the path is whatever
// resolution found, so neither is text this build produced.
func dependencyRow(res deps.Resolution, verbose bool) row {
	if res.Err != nil {
		return row{
			State:   outcomeFail,
			Label:   res.Tool.Name,
			Trailer: "not found, needed to " + res.Tool.Purpose,
		}
	}

	source := ""
	if res.Source != deps.SourcePath {
		source = string(res.Source)
	}
	// A fallback is the one resolution nothing on the machine points at.
	// PATH and an override are both places an operator put the tool. A
	// fallback is a package directory this search picked out of several, so
	// the file it settled on is shown without anyone asking for it.
	path := ""
	if verbose || res.Source == deps.SourceFallback {
		path = escape.Text(res.Path)
	}
	return row{
		State:   outcomePass,
		Label:   res.Tool.Name,
		Trailer: trailer(escape.Text(res.Version), source),
		Path:    path,
	}
}

// recompressRow reports the encoder a recompress would actually select on
// this machine.
//
// This is where the cross-platform answer reaches the operator rather than
// staying buried. Which encoder exists is a fact about the machine, not the
// configuration: a Mac has no nvenc and most Linux boxes have no qsv. One
// four-hour broadcast costs under an hour on a hardware encoder against six
// to sixteen in software. That is the difference between a rung of the ladder
// that works and one that never catches up.
//
// It is reported whether or not recompress is enabled, because the
// setting's own documentation tells the operator to run doctor before
// turning it on.
//
// Never a failure, and so never the passing mark either. Recompress is off by
// default and a machine with no hardware encoder is a supported machine, so
// every answer here is a note.
func recompressRow(ctx context.Context, encoders encodersFunc, recompress config.Recompress) row {
	state := "off"
	if recompress.Enabled {
		state = "on"
	}
	detail := fmt.Sprintf("%s, %s", state, recompress.Codec)

	available, err := encoders(ctx)
	if err != nil {
		return row{
			State:   outcomeNote,
			Label:   "recompress",
			Trailer: trailer(detail, "encoders could not be probed: "+escape.Text(err.Error())),
		}
	}

	encoder, software, err := post.SelectEncoder(available, post.TranscodeOptions{
		Codec:          recompress.Codec,
		Quality:        recompress.Quality,
		PreferHardware: recompress.PreferHardware,
	})
	if err != nil {
		return row{
			State:   outcomeNote,
			Label:   "recompress",
			Trailer: trailer(detail, "no encoder on this machine can produce it"),
		}
	}

	// The name comes from ffmpeg's own encoder listing, so it is a line of
	// a subprocess's output rather than text this build produced.
	chosen := "would use " + escape.Text(encoder.Name)
	if software {
		return row{
			State: outcomeNote,
			Label: "recompress",
			Trailer: trailer(detail, chosen+" in software",
				"hours per broadcast, no hardware encoder for this codec"),
		}
	}
	return row{State: outcomeNote, Label: "recompress", Trailer: trailer(detail, chosen)}
}

// libraryRow reports whether a library opened, and what it holds.
func libraryRow(root string) (row, bool) {
	lib, err := library.Open(root)
	if err != nil {
		return row{State: outcomeFail, Label: "refused", Trailer: escape.Text(err.Error())}, false
	}

	detail := escape.Text(string(lib.Owner()))
	if free, freeErr := space.Free(lib.Root()); freeErr == nil {
		detail = trailer(detail, config.Size(free).String()+" free")
	}
	return row{State: outcomePass, Label: "opened", Trailer: detail}, true
}

// ///////////////////////////////////////////////
// config
// ///////////////////////////////////////////////

func configCommand() *cli.Command {
	return &cli.Command{
		Name:  "config",
		Usage: "create and inspect the configuration file",
		Action: func(_ context.Context, cmd *cli.Command) error {
			return unknownCommand(cmd)
		},
		Commands: []*cli.Command{
			{
				Name:  "init",
				Usage: "write a commented default config",
				Action: func(_ context.Context, cmd *cli.Command) error {
					if err := noOperands(cmd); err != nil {
						return err
					}
					return initConfig(os.Stdout, configFile(cmd))
				},
			},
			{
				Name:  "validate",
				Usage: "check the config and report every problem",
				Action: func(_ context.Context, cmd *cli.Command) error {
					if err := noOperands(cmd); err != nil {
						return err
					}
					return validateConfig(os.Stdout, configFile(cmd))
				},
			},
			{
				Name:  "path",
				Usage: "print where the config is read from",
				Action: func(_ context.Context, cmd *cli.Command) error {
					if err := noOperands(cmd); err != nil {
						return err
					}
					fmt.Fprintln(os.Stdout, configFile(cmd))
					return nil
				},
			},
		},
	}
}

// initConfig writes a default config and reports where it landed.
func initConfig(out io.Writer, path string) error {
	out = styled(out)
	if err := config.Init(path); err != nil {
		return err
	}

	pairs(out, "wrote", [][2]string{{"config", escape.Text(path)}})
	summary(out, "1 file written",
		"run 'stream-dvr library init <path>' to point it at a library")
	return nil
}

// validateConfig loads a config and prints each problem it finds.
//
// The count is stated once, in the closing line. The error this returns names
// the command rather than the count, because a caller that printed it too
// would put the same number on screen twice.
func validateConfig(out io.Writer, path string) error {
	out = styled(out)
	cfg, err := config.Load(path)
	if err == nil {
		enabled := len(cfg.EnabledChannels())
		section(out, "config", []row{{
			State:   outcomePass,
			Label:   "loaded",
			Trailer: fmt.Sprintf("%d of %d channels enabled", enabled, len(cfg.Channels)),
		}})
		pairs(out, "locations", [][2]string{
			{"config", escape.Text(path)},
			{"library", escape.Text(cfg.Library.Root)},
		})

		next := "run 'stream-dvr serve' to start recording"
		if enabled == 0 {
			next = "add a channel, because nothing is enabled to record"
		}
		summary(out, "no problems", next)
		return nil
	}

	var invalid *config.ValidationError
	if !errors.As(err, &invalid) {
		return err
	}

	problems := make([]row, 0, len(invalid.Problems))
	for _, problem := range invalid.Problems {
		problems = append(problems, row{
			State:   outcomeFail,
			Label:   escape.Text(problem.Field),
			Trailer: escape.Text(problem.Detail),
		})
	}
	section(out, "problems", problems)
	summary(out, fmt.Sprintf("%d %s", len(problems), plural(len(problems), "problem", "problems")), "")
	return errors.New("the config is not valid")
}

// ///////////////////////////////////////////////
// library
// ///////////////////////////////////////////////

func libraryCommand() *cli.Command {
	return &cli.Command{
		Name:  "library",
		Usage: "create, claim, or import into a recording library",
		Action: func(_ context.Context, cmd *cli.Command) error {
			return unknownCommand(cmd)
		},
		Commands: []*cli.Command{
			{
				Name:      "init",
				Usage:     "initialize an empty library",
				ArgsUsage: "<path>",
				Flags:     []cli.Flag{repointFlag()},
				Action: func(_ context.Context, cmd *cli.Command) error {
					root, err := oneOperand(cmd, "library path")
					if err != nil {
						return err
					}
					return initLibrary(os.Stdout, configFile(cmd), root, false, cmd.Bool("force"))
				},
			},
			{
				Name:      "adopt",
				Usage:     "claim a directory of existing recordings",
				ArgsUsage: "<path>",
				Flags:     []cli.Flag{repointFlag()},
				Action: func(_ context.Context, cmd *cli.Command) error {
					root, err := oneOperand(cmd, "library path")
					if err != nil {
						return err
					}
					return initLibrary(os.Stdout, configFile(cmd), root, true, cmd.Bool("force"))
				},
			},
			{
				Name:  "import",
				Usage: "record library files no recording names yet",
				Flags: []cli.Flag{
					&cli.BoolFlag{
						Name:  "dry-run",
						Usage: "report what would be imported and write nothing",
					},
					&cli.StringFlag{
						Name:  "channel",
						Usage: "import only this channel",
					},
				},
				Action: func(ctx context.Context, cmd *cli.Command) error {
					if err := noOperands(cmd); err != nil {
						return err
					}
					return runImport(ctx, os.Stdout, configFile(cmd),
						cmd.String("channel"), cmd.Bool("dry-run"))
				},
			},
		},
	}
}

// repointFlag lets a library command claim a config that already names a
// different library.
func repointFlag() cli.Flag {
	return &cli.BoolFlag{
		Name:  "force",
		Usage: "repoint the config even when it already names another library",
	}
}

// initLibrary creates or adopts a library, points the config at it, and
// reports the result.
//
// The config write is what makes the next command work. A library created
// while the config stays empty leaves 'config validate' answering the
// operator with the command they just ran.
//
// Everything that can refuse is asked before the library is built: an
// unusable root, and a config already naming a different one. A refusal that
// came after would leave a directory the operator has to find and delete.
//
// A config already naming a different library needs --force, because
// repointing it silently orphans that library and every recording in it.
func initLibrary(out io.Writer, configPath, root string, adopt, force bool) error {
	out = styled(out)
	if root == "" {
		return fmt.Errorf("a library path is required")
	}
	// The config refuses a relative root, since one resolves against whatever
	// directory the daemon starts in. Resolving it here means the library and
	// the config name one directory rather than two.
	root, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolving library path: %w", err)
	}
	// Asked before anything is created, so a root the config would refuse
	// cannot leave a library on disk that no config can name. A zero-width
	// space pasted from a web page is the ordinary way this happens.
	if problem := config.LibraryRootProblem(root); problem != "" {
		return fmt.Errorf("library path %s", problem)
	}

	current, err := config.LibraryRoot(configPath)
	if err != nil {
		return err
	}
	if current != "" && !force && !paths.SameRoot(current, root) {
		return fmt.Errorf("%s points at library %s; pass --force to repoint it at %s",
			escape.Text(configPath), escape.Text(current), escape.Text(root))
	}

	lib, created, err := openOrCreate(root, adopt)
	if err != nil {
		return err
	}

	// Building the schema here is what leaves the calendar something to read
	// before any recording exists. Every other reader opens as a client and
	// refuses to migrate, so a library initialised and never served would
	// otherwise have no database at all.
	//
	// Only when there is none. store.Open migrates, and a library that
	// already carries a database may be one a daemon is serving right now,
	// whose schema this would move underneath it. A library with no database
	// is one nothing can be serving, so creating it there is free.
	if _, err := os.Stat(lib.DatabasePath()); errors.Is(err, os.ErrNotExist) {
		db, openErr := store.Open(lib.DatabasePath())
		if openErr != nil {
			return openErr
		}
		if closeErr := db.Close(); closeErr != nil {
			return closeErr
		}
	} else if err != nil {
		return fmt.Errorf("checking the library database: %w", err)
	}

	if err := config.SetLibraryRoot(configPath, lib.Root(), current); err != nil {
		if created {
			return fmt.Errorf("the library at %s exists but the config was not written, "+
				"so run this again once the config is reachable: %w", escape.Text(lib.Root()), err)
		}
		return err
	}

	reportLibrary(out, lib, configPath, created)
	return nil
}

// openOrCreate returns the library at root, creating or adopting it when
// there is none, and reports whether this call was what made it.
//
// A library that already exists is opened rather than refused. The operator
// asked for this path to be their library, and where it already is, the
// config is the only thing left to write. Refusing there is what leaves a
// machine holding a library on disk, a config that never learned about it,
// and no command that can say so.
//
// Open enforces ownership, so a library belonging to the other build lineage
// is still refused rather than adopted.
func openOrCreate(root string, adopt bool) (*library.Library, bool, error) {
	createdBy := fmt.Sprintf("%s %s", paths.BinaryName, version.Info())

	var (
		lib *library.Library
		err error
	)
	if adopt {
		lib, err = library.Adopt(root, createdBy)
	} else {
		lib, err = library.Create(root, createdBy)
	}
	switch {
	case err == nil:
		return lib, true, nil
	case !errors.Is(err, library.ErrAlreadyLibrary):
		return nil, false, err
	}

	lib, err = library.Open(root)
	if err != nil {
		return nil, false, err
	}
	return lib, false, nil
}

// reportLibrary prints what the command settled.
//
// The root is an operand, so it is whatever the shell handed over. On Linux
// and macOS a filename may hold any byte but NUL and the separator, which
// includes the escape sequences a terminal acts on.
func reportLibrary(out io.Writer, lib *library.Library, configPath string, created bool) {
	state, detail := outcomePass, "owned by the "+string(lib.Owner())+" build"
	if !created {
		state, detail = outcomeNote, "already owned by the "+string(lib.Owner())+" build"
	}

	section(out, "library", []row{
		{State: state, Label: "root", Trailer: detail},
		{State: outcomePass, Label: "config", Trailer: "points at it"},
	})
	pairs(out, "paths", [][2]string{
		{"root", escape.Text(lib.Root())},
		{"config", escape.Text(configPath)},
	})
	summary(out, "1 library", "add channels, then run 'stream-dvr config validate'")
}
