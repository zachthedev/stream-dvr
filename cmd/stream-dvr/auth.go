package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/urfave/cli/v3"
	"golang.org/x/term"

	"zach.tools/go/stream-dvr/internal/config"
	"zach.tools/go/stream-dvr/internal/escape"
	"zach.tools/go/stream-dvr/internal/paths"
	"zach.tools/go/stream-dvr/internal/providers/twitch"
	"zach.tools/go/stream-dvr/internal/secret"
)

// ///////////////////////////////////////////////
// Types
// ///////////////////////////////////////////////

// authDeps is what the auth commands act through.
//
// A struct rather than globals, so a test drives the real command against a
// fake store and a local server. That is where the leak assertions have to
// run: a token escapes through the wiring between the careful parts.
type authDeps struct {
	store  secret.Store
	client *http.Client
	// dataDir is where the derived streamlink config is written.
	dataDir string
	// prompt reads the token without echoing it. It is a field so a test
	// supplies one, since there is no terminal under go test.
	prompt func(io.Writer) (string, error)
	// clientID is the Twitch application this install acts as, read from
	// config. Empty means the operator has registered none, and the device
	// flow refuses rather than acting as somebody else's application.
	clientID string
	// configErr is why the config did not load, when it did not. An empty
	// clientID means one of two different things, and only this separates
	// them: nobody registered an application, or the file that would have
	// named one could not be read.
	configErr error
	// startDevice opens a device flow. A field for the same reason: the real
	// one refuses without a configured application id, and the branch worth
	// testing is the one that decides whether the code still has to be
	// typed. Nil means twitch.StartDevice.
	startDevice func(context.Context, *http.Client, string) (twitch.DeviceCode, error)
}

// ///////////////////////////////////////////////
// Instructions
// ///////////////////////////////////////////////

// theOnlyTokenThatWorks explains why this cannot be automated.
//
// It is stated plainly rather than apologised for. streamlink removed Twitch
// authentication in 2.0.0, because Twitch's playback endpoint refuses a token
// issued to any third-party client id. Measured here: a live channel offered
// zero streams. An operator who is not told this reasonably assumes the tool
// is being lazy.
//
//nolint:gosec // G101: instruction text, not a credential. It names the cookie an operator copies and holds no secret of its own.
const theOnlyTokenThatWorks = `twitch only accepts a token issued by its own website, so this step
cannot be automated.

  1. sign in at https://twitch.tv in a browser
  2. open developer tools with F12, choose Console, and run the line below
  3. copy the value it prints`

// browserSnippet reads the cookie Twitch's own website sets.
//
// Printed on a line of its own with no indent, no glyph and no trailing
// punctuation, so selecting it with a mouse selects the snippet and nothing
// around it. That is the whole reason it is not part of the numbered list
// above.
//
// G101: instruction text, not a credential. It names the cookie an operator copies and holds no secret of its own.
const browserSnippet = `document.cookie.split("; ").find(i=>i.startsWith("auth-token="))?.split("=")[1]`

// revocationWarning is what an operator is owed for pasting a credential.
//
// streamlink's own documentation says these tokens grant full access to the
// account. Someone handing one over deserves to know that, and how to take
// it back.
const revocationWarning = `this token grants full access to your twitch account, revoke it under
Twitch Settings, Security, Sign Out Everywhere`

// metadataPurpose says what the second authorization is for, and what it is
// not.
//
// The distinction is the safety property: a token granted this way is refused
// by Twitch's playback endpoint, measured as a live channel offering zero
// streams. An operator who thought this replaced the recording credential would
// have a recorder capturing nothing and no sign of why.
const metadataPurpose = `this authorizes reading broadcast listings and nothing else. it does
not record: twitch refuses a token issued this way at its playback
endpoint, so it neither replaces nor affects the recording token.

what it buys is speed. listing a channel's past broadcasts becomes one
request instead of one per broadcast, which is what recovery spends
most of its time on.`

// ///////////////////////////////////////////////
// auth
// ///////////////////////////////////////////////

func authCommand() *cli.Command {
	return &cli.Command{
		Name:  "auth",
		Usage: "store the credential streamlink records subscriber streams with",
		Description: "Without a token streamlink records only what is public, and says " +
			"nothing about it. This stores one, checks it is alive, and tells you when it dies.",
		Action: func(_ context.Context, cmd *cli.Command) error {
			return unknownCommand(cmd)
		},
		Commands: []*cli.Command{
			{
				Name:  "twitch",
				Usage: "store a Twitch token",
				Commands: []*cli.Command{
					{
						Name:   "status",
						Usage:  "report whether the stored token still works",
						Action: authAction(runAuthStatus),
					},
					{
						Name:   "logout",
						Usage:  "remove the stored token",
						Action: authAction(runAuthLogout),
					},
					{
						Name:  "metadata",
						Usage: "authorize the metadata API, which recovers past broadcasts faster",
						Description: "A separate authorization from the recording token, and not a " +
							"substitute for one: Twitch refuses a token issued this way at its playback " +
							"endpoint. It makes listing a channel's past broadcasts one request instead " +
							"of one per broadcast.",
						Commands: []*cli.Command{
							{
								Name:   "status",
								Usage:  "report whether the metadata session still works",
								Action: authAction(runMetadataStatus),
							},
							{
								Name:   "logout",
								Usage:  "remove the stored metadata session",
								Action: authAction(runMetadataLogout),
							},
						},
						Action: authAction(runMetadataLogin),
					},
				},
				Action: authAction(runAuthLogin),
			},
		},
	}
}

// authAction adapts one of the runners below to a cli action.
func authAction(run func(context.Context, io.Writer, authDeps) error) cli.ActionFunc {
	return func(ctx context.Context, cmd *cli.Command) error {
		if err := noOperands(cmd); err != nil {
			return err
		}
		dataDir := paths.DataDir()
		// Not fatal here. Storing a recording token needs no config at all,
		// so a config problem must not stand between an operator and that.
		// The metadata commands do read it, and they are handed the error
		// so they can tell the two empty ids apart.
		cfg, cfgErr := config.Load(configFile(cmd))
		return run(ctx, os.Stdout, authDeps{
			store:     secret.NewFile(dataDir),
			dataDir:   dataDir,
			prompt:    promptForToken,
			clientID:  cfg.Twitch.ClientID,
			configErr: cfgErr,
		})
	}
}

// runAuthLogin walks the operator through storing a token.
func runAuthLogin(ctx context.Context, out io.Writer, deps authDeps) error {
	out = styled(out)
	instruct(out, theOnlyTokenThatWorks, browserSnippet)

	token, err := deps.prompt(out)
	if err != nil {
		return err
	}
	if strings.TrimSpace(token) == "" {
		return errors.New("no token was entered")
	}

	identity, err := twitch.Validate(ctx, deps.client, token)
	if err != nil {
		if errors.Is(err, twitch.ErrInvalidToken) {
			return errors.New("twitch refused that token; check it was copied whole")
		}
		return err
	}
	// Validating is not the same as playing. Twitch's validation endpoint
	// accepts any user token and says which application it belongs to, while
	// the playback endpoint accepts only its own website's. A token from
	// anywhere else validates here, stores cleanly, and then records nothing
	// at all while the hourly check keeps reporting the credential healthy.
	if identity.ClientID != twitch.WebClientID {
		return fmt.Errorf("that token belongs to another application, so twitch will not play a stream with it; " +
			"copy the auth-token cookie from a browser signed in to twitch.tv")
	}

	if err := deps.store.Set(secret.AccountTwitch, token); err != nil {
		return err
	}

	path, err := twitch.WriteAuthConfig(deps.dataDir, token)
	if err != nil {
		return err
	}

	// G705: the login is Twitch's own text and the path is one
	// this process chose. escape.Text is what makes both safe to print, so it
	// is the mitigation rather than its absence.
	section(out, "twitch", []row{
		{State: outcomePass, Label: "token", Trailer: "valid, signed in as " + escape.Text(identity.Login)},
		{State: outcomePass, Label: "stored", Trailer: "in the credential store"},
		{State: outcomePass, Label: "wrote", Path: escape.Text(path)},
	})
	summary(out, "1 token stored", revocationWarning)
	return nil
}

// runAuthStatus reports on the stored token without changing anything.
func runAuthStatus(ctx context.Context, out io.Writer, deps authDeps) error {
	out = styled(out)
	token, err := deps.store.Get(secret.AccountTwitch)
	if errors.Is(err, secret.ErrNotFound) {
		section(out, "twitch", []row{
			{State: outcomeNote, Label: "token", Trailer: "none stored"},
		})
		summary(out, "no token", "run 'stream-dvr auth twitch' to store one")
		return nil
	}
	if err != nil {
		return err
	}

	identity, err := twitch.Validate(ctx, deps.client, token)
	switch {
	case errors.Is(err, twitch.ErrInvalidToken):
		section(out, "twitch", []row{
			{State: outcomeFail, Label: "token", Trailer: "no longer works"},
		})
		summary(out, "1 token, 1 dead", "run 'stream-dvr auth twitch' to replace it")
		return nil
	case err != nil:
		return err
	}

	// G705: Twitch's own text, escaped before printing.
	section(out, "twitch", []row{
		{State: outcomePass, Label: "token", Trailer: "valid, signed in as " + escape.Text(identity.Login)},
	})
	summary(out, "1 token", "")
	return nil
}

// runAuthLogout removes the token and the file derived from it.
//
// Both, always. Leaving the derived file would keep handing a credential
// the operator asked to forget to every capture.
func runAuthLogout(_ context.Context, out io.Writer, deps authDeps) error {
	out = styled(out)
	if err := deps.store.Delete(secret.AccountTwitch); err != nil {
		return err
	}
	if err := twitch.RemoveAuthConfig(deps.dataDir); err != nil {
		return err
	}

	section(out, "twitch", []row{
		{State: outcomePass, Label: "token", Trailer: "removed from the credential store"},
		{State: outcomePass, Label: "config", Trailer: "the derived streamlink file is gone"},
	})
	summary(out, "2 removed", "subscriber streams now record as public")
	return nil
}

// ///////////////////////////////////////////////
// The metadata session
// ///////////////////////////////////////////////

// metadataSession is the stored Helix authorization.
//
// Under its own account, so the recording credential and this one cannot be
// handed to the wrong reader. They are not interchangeable in either
// direction: this one does not play a stream, and the browser token is not a
// refreshable pair.
func metadataSession(deps authDeps) *twitch.Session {
	return twitch.NewSession(deps.clientID, deps.store, secret.AccountTwitchAPI, deps.client)
}

// runMetadataLogin walks the operator through authorizing the metadata API.
//
// The code is shown and Twitch is polled at the interval Twitch asked for.
// Nothing is typed here, which is the point of the device flow: the operator
// authorizes in a browser they are already signed into.
func runMetadataLogin(ctx context.Context, out io.Writer, deps authDeps) error {
	out = styled(out)
	start := deps.startDevice
	if start == nil {
		start = twitch.StartDevice
	}

	code, err := start(ctx, deps.client, deps.clientID)
	if errors.Is(err, twitch.ErrNoClientID) && deps.configErr != nil {
		return fmt.Errorf("the config did not load, so no application id was read: %w", deps.configErr)
	}
	if errors.Is(err, twitch.ErrNoClientID) {
		return errors.New("no twitch application id is configured. " +
			"Register one at dev.twitch.tv/console/apps, set its client type to public, " +
			"and put its client id in twitch.client_id. " +
			"Recovery still works through the listing tool without it")
	}
	if err != nil {
		return err
	}

	// The code is stated only where the page will not carry it. Twitch puts
	// it in the URI, so an instruction to type it is an instruction to type
	// over something already filled in.
	// G705: the page and the code are Twitch's own text, and
	// escape.Text is what makes them safe to print. That is the mitigation.
	steps := metadataPurpose + "\n\n  1. open the page below in a browser signed in to twitch"
	if !code.CodeIsInTheURI {
		steps += "\n  2. enter the code " + escape.Text(code.UserCode)
	}
	instruct(out, steps, escape.Text(code.VerificationURI))
	fmt.Fprintf(out, "  %s\n", styleDim.Render("waiting for the authorization, Ctrl-C stops"))

	tokens, err := twitch.PollDevice(ctx, deps.client, code, deps.clientID)
	if err != nil {
		return err
	}

	// Stored before anything else is done with it. The pair is the only copy
	// and the refresh half is what keeps the session alive unattended.
	if err := metadataSession(deps).Authorize(tokens); err != nil {
		return err
	}

	identity, err := twitch.Validate(ctx, deps.client, tokens.Access)
	if err != nil {
		return err
	}
	// G705: Twitch's own text, escaped before printing.
	section(out, "twitch api", []row{
		{State: outcomePass, Label: "session", Trailer: "authorized as " + escape.Text(identity.Login)},
	})
	summary(out, "1 session", "recovery now lists a channel in one request")
	return nil
}

// runMetadataStatus reports on the stored session.
//
// Asking for a token is what checks it, because that is where a session near
// expiry refreshes. A session left idle long enough for Twitch to forget it
// answers here rather than during an unattended recovery pass.
func runMetadataStatus(ctx context.Context, out io.Writer, deps authDeps) error {
	out = styled(out)
	const reauthorize = "run 'stream-dvr auth twitch metadata' to authorize"

	token, err := metadataSession(deps).Token(ctx)
	switch {
	case errors.Is(err, twitch.ErrNoSession):
		section(out, "twitch api", []row{
			{State: outcomeNote, Label: "session", Trailer: "none stored"},
		})
		summary(out, "no session", reauthorize)
		return nil
	case errors.Is(err, twitch.ErrReauthorize):
		section(out, "twitch api", []row{
			{State: outcomeFail, Label: "session", Trailer: "expired"},
		})
		summary(out, "1 session, 1 expired", reauthorize)
		return nil
	case err != nil:
		return err
	}

	identity, err := twitch.Validate(ctx, deps.client, token)
	switch {
	case errors.Is(err, twitch.ErrInvalidToken):
		section(out, "twitch api", []row{
			{State: outcomeFail, Label: "session", Trailer: "no longer works"},
		})
		summary(out, "1 session, 1 dead", reauthorize)
		return nil
	case err != nil:
		return err
	}

	// G705: Twitch's own text, escaped before printing.
	section(out, "twitch api", []row{
		{State: outcomePass, Label: "session", Trailer: "authorized as " + escape.Text(identity.Login)},
	})
	summary(out, "1 session", "")
	return nil
}

// runMetadataLogout removes the stored session.
func runMetadataLogout(_ context.Context, out io.Writer, deps authDeps) error {
	out = styled(out)
	if err := deps.store.Delete(secret.AccountTwitchAPI); err != nil {
		return err
	}

	section(out, "twitch api", []row{
		{State: outcomePass, Label: "session", Trailer: "removed from the credential store"},
	})
	summary(out, "1 removed", "recovery falls back to one request per broadcast")
	return nil
}

// ///////////////////////////////////////////////
// Instructions
// ///////////////////////////////////////////////

// instruct prints steps the operator carries out elsewhere, then the one line
// they copy.
//
// The instruction stays dim throughout, so the only thing at full weight is
// the line meant to be selected. It carries no indent, no glyph and no
// trailing punctuation: a double-click takes the word under it and a
// triple-click takes the line, and either has to yield the snippet alone.
func instruct(out io.Writer, steps, copyable string) {
	fmt.Fprintln(out)
	for line := range strings.SplitSeq(steps, "\n") {
		fmt.Fprintf(out, "%s\n", styleDim.Render(line))
	}
	fmt.Fprintf(out, "\n%s\n\n", copyable)
}

// ///////////////////////////////////////////////
// Reading the token
// ///////////////////////////////////////////////

// promptForToken reads a token with the terminal's echo off.
//
// A non-terminal stdin is refused rather than read. A token arriving on a pipe
// came from a shell history or a script. That is the situation this command
// exists to end, and reading one would quietly bless it.
func promptForToken(out io.Writer) (string, error) {
	fd := int(os.Stdin.Fd())
	if !term.IsTerminal(fd) {
		return "", errors.New("a token must be typed at a terminal, " +
			"so that it does not reach a shell history or a script")
	}

	fmt.Fprintf(out, "  %s\n", styleDim.Render("paste the token, nothing is shown as you type"))
	fmt.Fprint(out, "  > ")
	entered, err := term.ReadPassword(fd)
	fmt.Fprintln(out)
	if err != nil {
		// The error from the terminal names no input.
		return "", fmt.Errorf("reading the token: %w", err)
	}
	return string(entered), nil
}
