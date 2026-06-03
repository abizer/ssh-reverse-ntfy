# Eternal Terminal as a third transport

## Summary

Add [Eternal Terminal](https://eternalterminal.dev/) (`et`) as a third
interactive transport alongside ssh and mosh. ET is a TCP-based roaming shell
(survives disconnects/IP changes like mosh, but supports native scrollback and
works through more firewalls). It bootstraps over SSH and brokers the session
through a persistent `etserver` daemon on the remote.

## Why this is cleanly scoped

nssh's clipboard/URL bridge runs entirely over ntfy and is **transport-agnostic**.
The interactive transport is just the subprocess `runSession` execs after the
remote-prepare step (`prepareRemote`, which writes the session file + seeds the
remote log over a plain `ssh ... bash -l -s` invocation — independent of which
transport carries the interactive shell). ET slots in as a peer of mosh. No
changes to:

- the ntfy subscriber (`subscribeNtfy`/`handleMessage`)
- the wire format / envelope kinds
- the remote shims (`xclip`, `wl-copy`, `xdg-open`, …)
- `prepareRemote` / session-file convention
- session-collision logic

The work is concentrated in `selectTransport` plus flag, log-schema, and doc
plumbing.

## Behavior

### Flags
- New `--et` flag in session mode, **mutually exclusive** with `--ssh` and
  `--mosh`. At most one of the three may be passed.

### Auto-selection (when none of `--ssh`/`--mosh`/`--et` is forced)
Order of preference: **ET → mosh → ssh**.

- ET is chosen when `et` is on the **local** PATH **and** `etserver` is on the
  **remote** PATH (binary check only — `command -v etserver`, mirroring the
  existing `command -v mosh-server` probe for mosh).
- Else mosh, when `mosh` is local and `mosh-server` is remote.
- Else ssh.

Binary-check-only is a deliberate best-effort: `etserver` is a long-running
daemon, so the binary being present doesn't *prove* the daemon is listening. If
it's down, `et` errors out on launch and the user reruns with `--mosh`/`--ssh`.
nssh does not attempt to recover or probe the port — same posture as today's
mosh handling.

### ET launch form
`et <host>` — like the existing mosh path, **only the host target (`args[0]`) is
passed**, not the user's extra ssh flags. `et` resolves the host (and any
custom port/options) via `~/.ssh/config`. Default etserver port only; no
nssh-level port knob. No `LC_ALL`/`LANG` UTF-8 override (that workaround stays
mosh-specific).

## Code changes (`cmd/nssh/`)

### `main.go`
- Add `--et` to the session usage line in `usage()`.

### `session.go`
- Parse `--et` → `forceET` in `nsshMain`'s flag loop.
- Enforce "at most one of `--ssh`/`--mosh`/`--et`" (extend the current
  ssh+mosh mutual-exclusion check).
- Rework `selectTransport` to return the transport **name** as a string
  (`"ssh" | "mosh" | "et"`) instead of `(*exec.Cmd, useMosh bool)`. The ET
  branch builds `exec.Command("et", sshTarget)` with no env override.
- Add `remoteHasET(sshTarget)`; fold it and `remoteHasMosh` into a single
  `remoteHasCommand(sshTarget, cmd string) bool` helper that runs
  `ssh -o BatchMode=yes <target> "command -v <cmd> >/dev/null 2>&1"`. (Small,
  in-scope dedup of two identical probes.)
- Update the `session-end` log call site to record `Transport: <name>` (see
  log schema change below) instead of `Mosh: &useMosh`.

### Testability
- Extract the pure decision into:
  ```go
  func pickTransport(force string, localMosh, remoteMosh, localET, remoteET bool) string
  ```
  where `force` is `""|"ssh"|"mosh"|"et"`. `selectTransport` feeds it the real
  `exec.LookPath` + `remoteHasCommand` results and then constructs the
  `*exec.Cmd` for the chosen name.
- Add `transport_test.go` with a truth table over `pickTransport` (forced
  cases, auto-select precedence, partial availability). This logic is currently
  untested because `selectTransport` does I/O.

## Log schema change

`session-end` records which transport was used. The current `Mosh *bool` cannot
express three transports, so:

- In `log.go`, **replace** `Mosh *bool` with `Transport string`
  (`json:"transport,omitempty"`). It is no longer pointer-typed — the empty
  string naturally means "unset" and is dropped by `omitempty`, and there's no
  meaningful zero-value-that-must-be-recorded as there was for `mosh=false`.
- In `status.go::formatEvent`, replace the `if e.Mosh != nil` block with
  `if e.Transport != "" { fmt.Fprintf(&sb, " transport=%s", e.Transport) }`.
- Old log lines carrying a `mosh` field are silently ignored by the new reader
  (the field no longer exists on the struct). Acceptable: logs are ephemeral
  per-session diagnostics.

`Exit *int` stays pointer-typed (exit=0 is a real value that must be logged).

## Data flow

Unchanged. ET, like mosh, is only the interactive subprocess. Clipboard (text +
images) and URL/OAuth forwarding continue to flow over the ntfy topic exactly as
before.

## Error handling

- Daemon down despite `etserver` binary present → `et` exits non-zero;
  `runSession` returns the exit error; nssh reports the exit code. User reruns
  with `--mosh`/`--ssh`. No special-casing.
- `--ssh`/`--mosh`/`--et` conflict → error to stderr, exit 1 (existing pattern).

## Testing

- **Unit:** `transport_test.go` truth table over `pickTransport`:
  - each `force` value returns that transport regardless of availability
  - unforced + all available → `et`
  - unforced + et missing, mosh present → `mosh`
  - unforced + neither → `ssh`
  - unforced + local et but no remote etserver → falls through to mosh/ssh
- **Manual:**
  - `nssh --et <host>` launches et and the clipboard bridge still works.
  - On a host with both et and mosh, unforced `nssh <host>` selects et
    (stderr: "nssh: using et for interactive session").
  - `nssh status --tail` shows `transport=et` on the `session-end` line.

## Docs to update (same change)

- **CLAUDE.md** — session-mode description ("Wraps `ssh` or `mosh`" → add et);
  the persona/transport mention.
- **docs/internals.md** — transport-selection narrative; "whether the outer
  session is ssh or mosh" → add et.
- **docs/protocol.md** — `LogEvent` struct (`Mosh *bool` → `Transport string`),
  the `session-end` event-vocabulary row, and the pointer-typed-fields note
  (now only `Exit` is pointer-typed).
- **README.md** — pitch lines that say "ssh/mosh", the `--et` line in the usage
  block, and a requirements note (et on both ends, optional like mosh).

## Out of scope

- ET port config knob (`et_port` in config.toml) — default only for now.
- `nssh sweep` support for `et`/`etterminal` — etserver is a shared daemon that
  must not be killed; ET cleans up its own sessions.
- Forwarding the user's extra ssh flags to `et` — mirrors the existing mosh
  limitation.
