package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/urfave/cli/v3"

	"zach.tools/go/stream-dvr/internal/config"
	"zach.tools/go/stream-dvr/internal/escape"
	"zach.tools/go/stream-dvr/internal/logger"
	"zach.tools/go/stream-dvr/internal/notify"
	"zach.tools/go/stream-dvr/internal/paths"
)

// ///////////////////////////////////////////////
// notify-agent
// ///////////////////////////////////////////////

// agentLogMegabytes bounds the agent's own rotated log. It is smaller than the
// recorder's, because the agent writes a line when it starts, a line when
// something goes wrong, and nothing in between.
const agentLogMegabytes = 2

func notifyAgentCommand() *cli.Command {
	return &cli.Command{
		Name:  "notify-agent",
		Usage: "raise desktop notifications for a recorder running elsewhere",
		Description: "Runs in the operator's session and shows what the recorder reports. " +
			"Windows starts the recorder in session 0, which has no desktop to post to, " +
			"so this is what raises a notification there. It ends when the session does.",
		Action: func(ctx context.Context, cmd *cli.Command) error {
			if err := noOperands(cmd); err != nil {
				return err
			}
			return runNotifyAgent(ctx, os.Stdout, configFile(cmd))
		},
	}
}

// runNotifyAgent follows the recorder's socket and raises what it reports.
//
// It outlives any one recorder. The socket may not exist yet when a session
// starts, and the recorder restarts on upgrades and reboots, so the
// subscription reconnects rather than ending.
func runNotifyAgent(ctx context.Context, out io.Writer, configPath string) error {
	out = styled(out)
	// A console arrives with a process started from a Run key whether it
	// wants one or not, and this one has nothing to show in it.
	notify.HideConsole()

	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	if !cfg.Notify.Desktop {
		return fmt.Errorf("desktop notifications are turned off in %s", configPath)
	}

	log, closeLog, err := buildAgentLogger()
	if err != nil {
		return err
	}
	defer closeLog.Close()

	desktop, err := notify.NewSessionDesktop(log)
	if err != nil {
		return fmt.Errorf("this session cannot raise notifications: %w", err)
	}
	defer desktop.Close()

	// A command that blocks and never says so reads as one that finished,
	// which is how this was first taken for a one-shot.
	socket := paths.NotifySocket(cfg.Library.Root)
	banner(out, "watching for events from the recorder",
		[]string{escape.Text(socket)},
		"this runs until you press Ctrl-C")

	log.Info("notify agent started", slog.String("socket", escape.Field(socket)))

	// Follow returns only when the context is done, which for an agent
	// started at logon means the session ending.
	err = notify.Follow(ctx, socket, log, func(event notify.Event) {
		if raiseErr := desktop.Notify(ctx, event); raiseErr != nil {
			log.Warn("could not raise a notification",
				slog.String("kind", escape.Field(event.Kind)),
				slog.String("error", escape.Field(raiseErr.Error())))
		}
	})
	if err != nil && ctx.Err() == nil {
		return fmt.Errorf("following the recorder: %w", err)
	}
	return nil
}

// buildAgentLogger opens the agent's log.
//
// It goes to the machine-local log directory rather than into a library. The
// agent serves whichever library the config names, so it has no business
// writing into one. It can also start before the recorder makes that library
// reachable.
func buildAgentLogger() (*slog.Logger, io.Closer, error) {
	path := paths.LogNotifyAgent.Path(paths.LogDir())

	rotated, closer, err := logger.NewLogger(path, logger.LevelInfo, agentLogMegabytes)
	if err != nil {
		return nil, nil, fmt.Errorf("opening the notify agent log: %w", err)
	}
	return rotated.Logger, closer, nil
}
