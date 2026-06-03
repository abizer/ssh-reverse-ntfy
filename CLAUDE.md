# CLAUDE.md

Single Go binary (`nssh`) that runs everywhere. On the local side it wraps
SSH/mosh sessions and subscribes to ntfy for incoming clipboard and URL
messages. On remote hosts the same binary is symlinked as `xclip`, `wl-copy`,
`wl-paste`, and `xdg-open` — dispatching on `argv[0]` to act as a clipboard
bridge and URL forwarder over ntfy pub/sub.

## Repo layout

```
cmd/nssh/              The single binary (session wrapper + shim + --infect, dispatched on argv[0])
internal/wire/         Shared envelope type and parser
internal/ntfy/         Shared ntfy HTTP helpers (publish, attach, fetch)
internal/clipboard/    macOS pasteboard helpers (pbcopy, pbpaste, pngpaste, osascript)
docs/                  internals.md (architecture, flows) + protocol.md (wire/log schema)
.github/workflows/     CI (cachix.yaml for nix, release.yml for tagged releases)
justfile               Build recipes
flake.nix              Nix package
```

## Building

```bash
just build          # builds nssh for local platform
just install        # copies nssh to ~/.local/bin/ and ad-hoc signs it
just test           # runs all tests
```

## Setup commands

```bash
nssh infect <host>     # install on a remote host
nssh infect self       # set up persona symlinks on this machine
nssh infect --force …  # bypass the desktop-environment safety check
```

`infect <host>` downloads the matching binary from the latest GitHub
release, scps it to the remote, and asks the freshly-installed binary
to `infect self`. `infect self` creates symlinks in `~/.local/bin`
pointing at the running nssh binary (darwin: no-op; desktop linux:
refuses without --force to avoid shadowing real xclip/xdg-open).

`nssh sweep <host>` lists `mosh-server` processes owned by $USER on
the remote and offers to kill them. Safe when running tmux-inside-mosh:
killing mosh-server doesn't kill the tmux server, so detached sessions
survive. Use `--all` for unattended cleanup or `--older 168h` to keep
only the last week.

## Architecture

### Dispatch on argv[0]

| argv[0] | Mode | Description |
|---------|------|-------------|
| `nssh` (or anything else) | session | SSH/mosh/et wrapper + ntfy subscriber |
| `xclip` | shim | Clipboard bridge via ntfy |
| `wl-copy` / `wl-paste` | shim | Wayland clipboard bridge via ntfy |
| `xdg-open` / `sensible-browser` | shim | URL forwarding via ntfy |

### Session mode (local)

- Wraps `ssh`, `mosh`, or `et` (Eternal Terminal) with automatic transport
  selection: when no `--ssh`/`--mosh`/`--et` flag is given, prefers `et`
  (local `et` + remote `etserver`), then `mosh` (local `mosh` + remote
  `mosh-server`), else falls back to plain `ssh`. Detection is a binary
  presence check only; if a forced/auto transport's daemon is down, that
  transport's own launch fails and the user reruns with another flag.
- Generates a random topic (or reads a pinned one from config), writes it to
  the remote's `~/.config/nssh/session`, subscribes to ntfy in a background goroutine
- Dispatches incoming messages by `kind`:
  - `open`: opens URL locally, proxies OAuth callbacks via fresh `ssh -W`
  - `clip-write`: writes to macOS clipboard (text via pbcopy, images via osascript)
  - `clip-read-request`: reads macOS clipboard, publishes response back to ntfy

### Shim mode (remote)

- Dispatches on `os.Args[0]` (persona), parses the relevant CLI flags
- Publishes clipboard data / URL open requests to ntfy as JSON envelopes
- For clipboard reads: publishes a request with correlation ID, subscribes to
  the topic stream with a 5s timeout waiting for the matching response
- Handles `xclip -t TARGETS -o` by returning a static list of supported types
  (image/png, text/plain, etc.) — Claude Code probes this before attempting
  image paste via Ctrl+V

### Wire format

JSON envelopes on the ntfy topic. Every message has a `kind` field:

| Kind | Direction | Description |
|------|-----------|-------------|
| `open` | remote → local | Open a URL in the local browser |
| `clip-write` | remote → local | Write data to the Mac clipboard |
| `clip-read-request` | remote → local | Request the Mac clipboard contents |
| `clip-read-response` | local → remote | Response with clipboard data |
| `ping` | local ↔ local | Liveness probe between two nssh processes sharing a topic |
| `pong` | local ↔ local | Ack for `ping`, echoing the same correlation id |

Small text (≤3KB) is base64-encoded inline in the `body` field. Larger payloads
and images are sent as ntfy attachments (PUT with `Filename` + `X-Message` headers).

### Topic convention

Each connection gets a random topic (`nssh_<random>`) by default — unguessable,
no config required. nssh writes the server + topic to `~/.local/state/nssh/session`
on the remote before launching the shell (and seeds a `session-open` event into
the JSONL log). The shim reads this file.

### Session collisions

A pidfile per live local nssh is kept at `~/.local/state/nssh/sessions/<pid>.json`.
On startup, nssh looks up the host (canonical short name from `ssh -G`) in that
registry. When an existing session is found nssh sends a `ping` on the topic and
waits ~1.5s for a `pong`, then prompts (in an interactive shell) for one of:

| Choice | Effect |
|--------|--------|
| join | adopt the existing topic; both subscribers see every message |
| replace | SIGTERM the existing PID, then SIGKILL after 1s if it's still up; fresh topic |
| new | fresh topic; existing PID is left running but the remote bridge will follow the new topic |

Default in the prompt is `join` if the peer answered the ping, `replace` if it
didn't. Non-interactive shells silently join (with a warning on the stderr if the
peer was unresponsive). Override with `--join` / `--replace` / `--new` on the
command line.

Optional `~/.config/nssh/config.toml` on either side to pin values:
```toml
server = "https://ntfy.example.com"  # default: https://ntfy.sh
topic = "my-fixed-topic"             # default: random per-connection
```

Priority: `NSSH_NTFY_BASE` env > config.toml > session file > defaults.

## Key constraints

- Minimize external Go dependencies — prefer stdlib + `golang.org/x/*`
  modules over third-party. Pull in a dep only when it replaces a
  hand-rolled re-implementation that's a real correctness or
  ergonomics liability (current set: `golang.org/x/mod/semver`).
- Single binary cross-compiles for macOS and Linux with zero runtime deps
- Never eval or execute received content on local side
- Only bridge CLIPBOARD selection, not PRIMARY

## Maintaining docs

`docs/internals.md` and `docs/protocol.md` are the precise current-state
references. Update them in the same change that makes them stale —
don't ship the code change and document later. Things that require a
docs touch:

- New or removed envelope `kind` → update the kinds table in
  `protocol.md` and any flow diagrams in `internals.md` that mention it.
- New, renamed, or removed `LogEvent` field or event name → update
  the schema and event vocabulary in `protocol.md`.
- New config key, env var, or precedence change → update the config
  section in `protocol.md`.
- New ntfy endpoint or change to inline-vs-attachment rules → update
  the endpoints / transport sections in `protocol.md`.
- New shim persona, transport (e.g. ssh/mosh siblings), or
  cross-cutting design choice → update the relevant `internals.md`
  section and the persona table here in CLAUDE.md.

Pure refactors (file splits, helper extractions) usually don't need
doc updates unless they change a name the docs reference. The README
is for the pitch and the install path; everything precise lives under
`docs/`.
