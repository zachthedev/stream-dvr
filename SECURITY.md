# Security Policy

## Supported versions

stream-dvr is pre-release, at `v0.0.0`. Only the `main` branch gets fixes.

## Reporting a vulnerability

Report privately through GitHub, under Security, then Report a
vulnerability.

If that button is not there, private vulnerability reporting has not been
switched on yet. In that case open an issue saying only that you have a
security report and asking for a private channel, with no detail in it.
Do not put the details in a public issue.

Include what an attacker gains, the steps to reproduce, and the commit
you tested.

## What matters most here

stream-dvr runs unattended, drives subprocesses, and holds the only copy
of a recording. Reports in these areas get read first:

- A path that deletes, truncates, or overwrites a recording.
- A config value, channel name, or broadcast title that escapes into a
  shell, a filesystem path outside the library, or a terminal.
- Anything that reads or writes outside the library root and the data
  directory.
- Credentials or channel names reaching a log, a webhook payload, or a
  crash report.

The daemon is user-scoped by design and needs no elevation to record.
Registering autostart on Windows does, so a path that gains privilege
from an unelevated shell is in scope.
