# Internals

This is the "why" doc — design decisions, end-to-end flows, and the
reasoning behind the more unusual choices. For the on-the-wire schema,
see [protocol.md](./protocol.md).

## Why ntfy

The design constraint is **mosh compatibility**. mosh is UDP, deliberately
opaque, and offers no in-band channels:

- No port forwarding (`-L`, `-R`, `-D`)
- No Unix-socket multiplexing (`ControlMaster`)
- OSC 52 is the only escape hatch, and mosh caps it at 256 bytes and
  doesn't support binary

That eliminates the obvious solutions. Anything we want to send between
local and remote has to ride a side channel that's reachable from both.

The criteria for a side channel:

1. **Roaming-safe.** Survives laptop sleep, NAT rebind, network change.
   mosh users care about this — it's why they use mosh.
2. **No inbound connectivity required on the laptop.** The laptop is
   often behind NAT, on a hotel Wi-Fi, etc. We can't ask the remote to
   open a TCP connection back.
3. **Authentication-light.** A long-lived ssh key per host is one
   authentication boundary; we'd rather not stack a second one per
   session.
4. **Stateless on the server side.** No per-user accounts, no admin
   overhead.

ntfy fits all four. A topic is just a path; subscribing is HTTP GET on
`/<topic>/json`; publishing is HTTP POST to `/<topic>`. The default
public ntfy.sh works out of the box; users who want privacy run their
own. Subscriptions are streamed responses — the client holds them open,
the server pushes events as newline-delimited JSON, and a 90-second
read deadline is enough to detect zombie connections (laptop closed,
NAT dropped) without polling. See `subscribeNtfy` in `session.go`.

The security boundary becomes "anyone with the topic can publish and
subscribe." Topics are generated per-session via 12 random bytes of
base32 (`generateTopic` in `config.go`) — unguessable in practice. A
user who wants a stable topic can pin it in `config.toml`; any password
auth on top is delegated to ntfy's own ACL config (we don't reimplement
it).

### Transport selection (ssh / mosh / et)

The interactive shell can ride `ssh`, `mosh`, or `et` (Eternal Terminal),
chosen in `selectTransport` (`session.go`). With no `--ssh`/`--mosh`/`--et`
flag, auto-selection prefers `et` (local `et` + remote `etserver`), then
`mosh` (local `mosh` + remote `mosh-server`), else `ssh`. Detection is a
binary-presence check only (`command -v` over `ssh -o BatchMode=yes`); the
pure decision lives in `pickTransport` and is unit-tested. A daemon that's
installed but not listening isn't caught here — the transport's own launch
fails and the user reruns with another flag.

Note that `et` *does* support port forwarding, unlike mosh. nssh still
routes the clipboard/URL bridge over ntfy regardless of transport, so the
bridge's behavior is identical no matter how the shell is carried — the
remote shims never have to know which transport is in play. Like the mosh
path, the `et` launch passes only the host target (`et` resolves it via
`~/.ssh/config`); extra ssh flags are not forwarded.

## Why dispatch on argv[0]

Tools like Claude Code, `gh auth login`, and `gcloud auth login` call
`xclip`, `xdg-open`, etc. by name via `exec.Command` (or PATH lookup).
We can't change those callers. The cleanest interception is to **be
those tools** — same name, same flags, same exit semantics, but with a
custom implementation.

Dispatch on `argv[0]` means the same binary can answer to multiple
names by symlinking. `nssh infect <host>` does this on the remote: scp
the binary to `~/.local/bin/nssh`, then create symlinks
`~/.local/bin/{xclip,wl-copy,wl-paste,xdg-open,sensible-browser}` →
`nssh`. As long as `~/.local/bin` is first on `$PATH`, our shim wins.

The dispatch happens at the top of `main()`:

```go
persona := filepath.Base(os.Args[0])
switch persona {
case "xdg-open", "sensible-browser", "xclip", "wl-copy", "wl-paste":
    shimMain(persona, os.Args[1:])
    return
}
```

If the persona doesn't match a shim we own, we fall through to
nssh-as-itself: subcommands (`infect`, `status`) or the default
`nssh <host>` session wrapper.

The shims (`shim.go`) parse just enough of each tool's flag vocabulary
to do their job, and shell out to `/usr/bin/<name>` for cases we don't
handle (e.g. `xdg-open <local-file-path>`, `xclip -selection primary`).

## Why refuse `infect self` on a desktop

Persona symlinks shadow the real `xclip`/`xdg-open`. On a headless dev
box that's the whole point: we replace tools that would fail (no
display) with tools that work (forward to the laptop). On a desktop,
the real `xclip` is what your password manager and browser use to
write/read the clipboard — shadowing it would silently break them.

`detectLocalDesktop` and `detectRemoteDesktop` (in `infect.go`) sniff
the obvious markers: `$DISPLAY`, `$WAYLAND_DISPLAY`, an X11 socket in
`/tmp/.X11-unix`, or a Wayland socket under `/run/user/*/wayland-*`.
If any of those exist, we refuse to install (or refuse to run `infect
self`) without `--force`.

A Linux laptop user who runs `nssh devbox` is a perfectly normal case
— they'd never want `infect self` on the laptop, only on remotes. The
desktop check protects them automatically.

## End-to-end: paste image (laptop → remote Claude Code)

```
[mac]                         [ntfy]                       [linux remote]

User: Cmd-Shift-Ctrl-4 (screenshot to clipboard)
                                                          User: Ctrl-V in Claude Code

                                                          Claude Code: spawns
                                                            `xclip -t image/png -o`
                                                            (which is our nssh shim)

                                                          shim: publish
                                                            envelope kind=clip-read-request
                                                            id=<random>
                                                            mime=image/png
                                                          POST /topic ─────────►
                                                          subscribe GET /topic/json
                                                            ?since=<now> (5s timeout)

                              ◄─ POST /topic ──────────  (line received)

local nssh subscriber:
  scanner.Scan() returns
  the published line.
  handleMessage dispatches
  to handleClipReadRequest.

  pngpaste reads the Mac
  pasteboard → PNG bytes.
  wire.Publish picks
  attachment path (image
  mime + bytes > 3KB).

PUT /topic ─────────────────► (received by ntfy, attachment URL)

                              ─────► /topic/json line   (shim's GET completes)

                                                          shim: line has
                                                            kind=clip-read-response
                                                            id=<matching>
                                                            attachment.url=...
                                                          shim fetches the URL,
                                                          writes PNG bytes
                                                          to its stdout.

                                                          Claude Code reads
                                                          stdout → rendered.
```

The whole round trip is typically ~200ms over a public ntfy.sh. The
correlation `id` ensures the shim's `since=`-bounded subscription
only consumes the response intended for *this* request — multiple
concurrent reads on the same topic don't cross paths.

## End-to-end: OAuth callback

```
[mac]                                              [linux remote]

                                                  $ gh auth login
                                                  Press Enter to open in browser

                                                  gh: spawns
                                                    `xdg-open https://github.com/.../oauth?...
                                                     redirect_uri=http%3A%2F%2Flocalhost%3A8585%2Fcb`

                                                  shim publishes
                                                    envelope kind=open
                                                    url=<full OAuth URL>

local nssh subscriber:
  handleMessage → handleOpen.
  handleOpen URL-parses, sees
  redirect_uri contains
  localhost:8585. Spawns
  `proxyOAuthCallback` goroutine
  that listens on :8585, then
  runs `open <url>` to launch
  the browser locally.

User logs in via browser.
Browser GETs http://localhost:8585/cb?code=...

  proxyOAuthCallback: ln.Accept()
  returns a conn. Spawns
  `ssh -W localhost:8585 <target>`
  with conn piped to ssh's
  stdin/stdout.

                                                  gh: HTTP server bound to
                                                    localhost:8585 receives the
                                                    forwarded request via
                                                    ssh's accepted forward.
                                                    Returns 200 OK.

  ssh -W exits when the conn closes.
  proxyOAuthCallback prints
  "OAuth callback on :8585 done".

                                                  gh: token exchanged.
                                                  ✓ Logged in.
```

Notes on this flow:

- We use a fresh `ssh -W` per callback. No `ControlMaster`, no socket
  files. This makes it work whether the outer session is ssh, mosh, or
  et — `ssh -W` is its own connection. The user authenticates once when
  the session starts (or relies on a key); subsequent `ssh -W` calls
  for OAuth callbacks reuse the same auth.
- `ln.Accept()` has a 5-minute deadline (`oauthAcceptTimeout` in
  `oauth.go`). If the user closes the browser tab without completing,
  we don't leak the listener.
- The shim sends the URL via ntfy *and* falls back to `/usr/bin/xdg-open`
  on publish failure — so if ntfy is down or the topic is misconfigured,
  the user still gets the same exit code they'd get on a normal
  headless system (255 — "no display").

## Why one binary, three roles

A unified binary keeps deployment trivial: `infect` scps a single file,
makes 5 symlinks, done. There's no separate "shim" package with its
own version drift, no risk of the shim and the daemon disagreeing on
the wire format. `prepareRemote` (in `remote.go`) probes the remote's
`nssh --version` at session start and prompts for an upgrade if it's
behind the local — version mismatch is a one-prompt fix.

The trade-off is that `cmd/nssh/main.go` has to dispatch three roles
based on argv[0] and subcommand. That's why it's split into:

- `main.go` — argv[0] dispatch + version + usage
- `session.go` — local-side wrapper (the default `nssh <host>` flow)
- `shim.go` — remote-side persona implementation
- Both sides share `wire`, `ntfy`, `config`, and `log`.

## Logging

Every nssh process opens `~/.local/state/nssh/nssh.<topic>.jsonl` for
appending. Both sides write to it (locally on the laptop, on the
remote it's `$XDG_STATE_HOME/nssh/...`). The schema is the typed
`LogEvent` struct in `log.go`; see [protocol.md](./protocol.md) for
the full event vocabulary.

`nssh status --tail` follows the active sessions' logs and pretty-prints
events as they arrive — useful for "what happened?" debugging. POSIX
`O_APPEND` writes < `PIPE_BUF` (~4KB) are atomic, so concurrent shim
invocations on the same log don't interleave lines.

## What we deliberately don't do

- **Eval received content.** The local side never executes anything
  it receives. URLs are passed to `open(1)` after a strict `http(s)://`
  prefix check; clipboard payloads go to `pbcopy`/`osascript` as data,
  never as commands.
- **Bridge PRIMARY selection.** Only CLIPBOARD. PRIMARY is what gets
  set when you select text in an X11 terminal — bridging it would
  generate continuous traffic from terminal use.
- **Authenticate the bridge.** Topic secrecy is the only auth. If you
  need auth, run a private ntfy server with its own ACLs and pin the
  server in `config.toml`.
