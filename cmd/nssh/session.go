package main

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/mod/semver"

	"github.com/abizer/nssh/v2/internal/ntfy"
	"github.com/abizer/nssh/v2/internal/wire"
)

// nsshMain runs the default `nssh [--ssh|--mosh] <host>` flow:
// resolves the host, opens the per-session log, prepares the remote
// (writes session file + seeds remote log + probes installed version),
// starts the ntfy subscriber goroutine, then execs ssh or mosh
// interactively and waits for it to exit.
func nsshMain() {
	args := os.Args[1:]
	forceSSH := false
	forceMosh := false
	forceET := false
	collisionFlag := "" // "join" | "replace" | "new" | ""
parse:
	for len(args) > 0 {
		switch args[0] {
		case "--ssh":
			forceSSH = true
		case "--mosh":
			forceMosh = true
		case "--et":
			forceET = true
		case "--join", "--replace", "--new":
			collisionFlag = strings.TrimPrefix(args[0], "--")
		case "-h", "--help":
			usage()
		default:
			break parse
		}
		args = args[1:]
	}
	forced := 0
	for _, f := range []bool{forceSSH, forceMosh, forceET} {
		if f {
			forced++
		}
	}
	if forced > 1 {
		fmt.Fprintln(os.Stderr, "nssh: --ssh, --mosh, and --et are mutually exclusive")
		os.Exit(1)
	}
	forceTransport := ""
	switch {
	case forceSSH:
		forceTransport = "ssh"
	case forceMosh:
		forceTransport = "mosh"
	case forceET:
		forceTransport = "et"
	}
	if len(args) < 1 {
		usage()
	}

	sshArgs := args
	sshTarget := args[0]

	shortHost := resolveShortHost(sshArgs)
	if shortHost == "" {
		fmt.Fprintln(os.Stderr, "nssh: could not resolve hostname, falling back to plain ssh")
		cmd := exec.Command("ssh", sshArgs...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		cmd.Run()
		return
	}

	cfg := loadSessionConfig()
	joinedPID := 0
	if cfg.Topic == "" {
		existing := findActiveSessionForHost(shortHost)
		choice := resolveSessionCollision(existing, collisionFlag)
		switch {
		case existing != nil && choice == "join":
			cfg.Topic = existing.Topic
			cfg.Server = existing.Server
			joinedPID = existing.PID
			fmt.Fprintf(os.Stderr, "nssh: joining active session for %s (PID %d)\n", shortHost, existing.PID)
		case existing != nil && choice == "replace":
			fmt.Fprintf(os.Stderr, "nssh: replacing existing session for %s (PID %d)\n", shortHost, existing.PID)
			replaceSession(existing)
			cfg.Topic = generateTopic()
		default:
			if existing != nil && choice == "new" {
				fmt.Fprintf(os.Stderr, "nssh: starting on a fresh topic; existing PID %d will be left on the old one\n", existing.PID)
			}
			cfg.Topic = generateTopic()
		}
	}
	fmt.Fprintf(os.Stderr, "nssh: subscribing to %s\n", cfg.topicURL())

	openLog(cfg.Topic, "session")
	logEvent(LogEvent{
		Event:  "session-start",
		Target: sshTarget,
		Host:   shortHost,
		Server: cfg.Server,
		Joined: joinedPID, // omitempty drops 0
	})

	sessionFile, err := registerSession(cfg, sshTarget, shortHost)
	if err != nil {
		fmt.Fprintf(os.Stderr, "nssh: register session: %v\n", err)
	}
	defer unregisterSession(sessionFile)

	// First nssh for this host: probe version, write the remote session file,
	// and seed the remote JSONL log. When joining, the original process
	// already did this and the remote state is still correct — skip the extra
	// SSH and the version prompt the user has already seen.
	if joinedPID == 0 {
		remoteVer := prepareRemote(sshTarget, cfg)
		if localVer := version(); isReleaseVersion(localVer) {
			switch {
			case remoteVer == "":
				fmt.Fprintln(os.Stderr, "nssh: not installed on remote — clipboard bridge will not work")
				if promptYes("  install it now?") {
					infectRemote(sshTarget, false)
				}
			case semver.Compare(remoteVer, localVer) != 0:
				fmt.Fprintf(os.Stderr, "nssh: remote version %s, local %s\n", remoteVer, localVer)
				if promptYes("  update remote to " + localVer + "?") {
					infectRemote(sshTarget, false)
				}
			}
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go subscribeNtfy(ctx, cfg, sshTarget)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	session, transport := selectTransport(forceTransport, sshArgs, sshTarget)
	sessErr := runSession(session, sigs)
	resetTerminal()
	exitCode := 0
	if exitErr, ok := sessErr.(*exec.ExitError); ok {
		exitCode = exitErr.ExitCode()
	}
	logEvent(LogEvent{Event: "session-end", Exit: &exitCode, Transport: transport})
	unregisterSession(sessionFile) // defers don't fire under os.Exit
	if exitCode != 0 {
		os.Exit(exitCode)
	}
}

// resolveSessionCollision picks how this nssh should relate to any existing
// nssh attached to the same host. Inputs:
//   - existing: nil if no live session for this host (just generate a topic)
//   - flag: one of "" (decide automatically) / "join" / "replace" / "new"
//
// When unforced, we ping the existing topic to test whether the peer is
// responsive; an unresponsive peer makes "replace" the prompt default since
// joining a wedged subscriber is almost never what the user wants.
func resolveSessionCollision(existing *sessionInfo, flag string) string {
	if existing == nil {
		return "new"
	}
	switch flag {
	case "join", "replace", "new":
		return flag
	}

	alive := pingTopic(existing.Server, existing.Topic, 1500*time.Millisecond)
	age := time.Since(existing.Started).Round(time.Second)
	fmt.Fprintf(os.Stderr, "nssh: existing session on %s (PID %d, started %s ago, alive=%v)\n",
		existing.Host, existing.PID, age, alive)

	stat, err := os.Stdin.Stat()
	interactive := err == nil && stat.Mode()&os.ModeCharDevice != 0
	if !interactive {
		// In a script — joining is the least surprising default. Warn if the
		// peer didn't answer so the operator sees something is wrong.
		if !alive {
			fmt.Fprintln(os.Stderr, "nssh: peer did not respond to ping; joining anyway (pass --replace or --new to override)")
		}
		return "join"
	}

	defaultChoice := "join"
	prompt := "  [J]oin / [R]eplace (kill PID) / [N]ew topic / [C]ancel? [J] "
	if !alive {
		defaultChoice = "replace"
		prompt = "  [R]eplace (kill PID) / [N]ew topic / [J]oin anyway / [C]ancel? [R] "
	}
	fmt.Fprint(os.Stderr, prompt)
	var resp string
	fmt.Scanln(&resp)
	switch strings.ToLower(strings.TrimSpace(resp)) {
	case "j", "join":
		return "join"
	case "r", "replace":
		return "replace"
	case "n", "new":
		return "new"
	case "c", "cancel", "q", "quit":
		fmt.Fprintln(os.Stderr, "nssh: cancelled")
		os.Exit(0)
	case "":
		return defaultChoice
	}
	fmt.Fprintf(os.Stderr, "nssh: unrecognized choice %q, using default (%s)\n", resp, defaultChoice)
	return defaultChoice
}

// replaceSession terminates the existing nssh process and removes its pidfile.
// Sends SIGTERM first (lets defers run on the old process — unregister, logs);
// escalates to SIGKILL if it's still alive after 1s. Best-effort: missing
// pidfile or already-dead process is not an error.
func replaceSession(s *sessionInfo) {
	if s == nil || s.PID <= 0 {
		return
	}
	pidfile := filepath.Join(sessionsDir(), fmt.Sprintf("%d.json", s.PID))
	defer os.Remove(pidfile)

	if err := syscall.Kill(s.PID, syscall.SIGTERM); err != nil {
		return // process already gone
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(s.PID, 0) != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	// Still alive — escalate. SIGKILL won't run the other process's defers,
	// so we leave pidfile cleanup to our deferred os.Remove above.
	_ = syscall.Kill(s.PID, syscall.SIGKILL)
}

// runSession execs the interactive ssh/mosh subprocess, wires its stdio to
// our terminal, and forwards INT/TERM/HUP signals until it exits.
func runSession(cmd *exec.Cmd, sigs <-chan os.Signal) error {
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	for {
		select {
		case err := <-done:
			return err
		case sig := <-sigs:
			cmd.Process.Signal(sig)
		}
	}
}

// resetTerminal disables xterm mouse-tracking modes and re-shows the cursor.
// vim, htop, etc. enable these and don't always restore them on exit; this
// runs after the interactive session ends so the local prompt is sane again.
func resetTerminal() {
	os.Stdout.WriteString(
		"\x1b[?1000l" + "\x1b[?1002l" + "\x1b[?1003l" + "\x1b[?1006l" + "\x1b[?25h",
	)
}

// remoteHasCommand reports whether `cmd` is on the remote's PATH. Used to
// auto-select a transport (mosh-server / etserver) when none is forced.
func remoteHasCommand(sshTarget, cmd string) bool {
	c := exec.Command("ssh", "-o", "BatchMode=yes", sshTarget,
		fmt.Sprintf("command -v %s >/dev/null 2>&1", cmd))
	return c.Run() == nil
}

// pickTransport decides which interactive transport to use. force is one of
// "" (auto), "ssh", "mosh", "et"; a forced choice is honored regardless of
// detected availability — the user knows their setup. When auto-selecting,
// preference is et > mosh > ssh, and a transport only wins if both its local
// and remote halves are present.
func pickTransport(force string, localMosh, remoteMosh, localET, remoteET bool) string {
	switch force {
	case "ssh", "mosh", "et":
		return force
	}
	switch {
	case localET && remoteET:
		return "et"
	case localMosh && remoteMosh:
		return "mosh"
	default:
		return "ssh"
	}
}

// selectTransport resolves the transport (honoring force, else probing local
// binaries and the remote PATH), builds the interactive command, and returns
// it alongside the chosen transport name for downstream logging. Remote probes
// are skipped entirely when a choice is forced or the local binary is absent.
// When mosh is selected we force a UTF-8 locale because mosh's terminal
// emulation breaks under POSIX/C locales on minimal images.
func selectTransport(force string, sshArgs []string, sshTarget string) (*exec.Cmd, string) {
	var localMosh, remoteMosh, localET, remoteET bool
	if force == "" {
		_, errET := exec.LookPath("et")
		localET = errET == nil
		_, errMosh := exec.LookPath("mosh")
		localMosh = errMosh == nil
		if localET {
			remoteET = remoteHasCommand(sshTarget, "etserver")
		}
		if localMosh {
			remoteMosh = remoteHasCommand(sshTarget, "mosh-server")
		}
	}

	switch pickTransport(force, localMosh, remoteMosh, localET, remoteET) {
	case "et":
		fmt.Fprintln(os.Stderr, "nssh: using et for interactive session")
		return exec.Command("et", sshTarget), "et"
	case "mosh":
		fmt.Fprintln(os.Stderr, "nssh: using mosh for interactive session")
		cmd := exec.Command("mosh", sshTarget)
		cmd.Env = append(os.Environ(), "LC_ALL=C.UTF-8", "LANG=C.UTF-8")
		return cmd, "mosh"
	default:
		return exec.Command("ssh", sshArgs...), "ssh"
	}
}

// deadlineConn wraps net.Conn to push the read deadline forward on every Read.
// The ntfy server sends keepalive events every ~55s, so if no bytes arrive
// for well past that window the connection is silently dead (laptop sleep, NAT
// rebind, proxy drop) — the next Read returns i/o timeout and the subscriber
// reconnects. Without this, Read can block forever on a zombie TCP socket.
type deadlineConn struct {
	net.Conn
	period time.Duration
}

func (c *deadlineConn) Read(p []byte) (int, error) {
	_ = c.Conn.SetReadDeadline(time.Now().Add(c.period))
	return c.Conn.Read(p)
}

// subscribeNtfy maintains a long-lived GET on the ntfy /json endpoint,
// reconnecting on disconnect/timeout. Each line of the response is a ntfy
// event; messages with non-empty bodies are dispatched to handleMessage in
// their own goroutines. Stops when ctx is canceled.
func subscribeNtfy(ctx context.Context, cfg nsshConfig, sshTarget string) {
	topicURL := cfg.topicURL()
	endpoint := topicURL + "/json"

	dialer := &net.Dialer{KeepAlive: 15 * time.Second}
	client := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				conn, err := dialer.DialContext(ctx, network, addr)
				if err != nil {
					return nil, err
				}
				return &deadlineConn{Conn: conn, period: 90 * time.Second}, nil
			},
			ResponseHeaderTimeout: 30 * time.Second,
		},
	}

	var (
		lastID     string    // most recent ntfy message id we processed
		downAt     time.Time // when the previous connection dropped, zero on cold start
		downLogged bool      // already logged subscribe-down for the current outage
	)
	// markDown is idempotent for the current outage: connect-failure and
	// stream-end paths both invoke it, but only the first call records the
	// timestamp + log line. Cleared on every successful subscribe-up.
	markDown := func(reason string) {
		if downLogged || ctx.Err() != nil {
			return
		}
		logEvent(LogEvent{Event: "subscribe-down", Err: reason})
		downAt = time.Now()
		downLogged = true
	}

	for {
		if ctx.Err() != nil {
			return
		}
		url := endpoint
		if lastID != "" {
			// ntfy's ?since=<id> is exclusive: we get back messages strictly
			// newer than lastID. Without this, anything posted while we were
			// asleep or disconnected is dropped on the floor.
			url += "?since=" + lastID
		}
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			return
		}
		resp, err := client.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			fmt.Fprintf(os.Stderr, "nssh: ntfy: %v — retrying\n", err)
			markDown(err.Error())
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
			continue
		}

		upEvent := LogEvent{Event: "subscribe-up"}
		if !downAt.IsZero() {
			upEvent.Reconnect = true
			upEvent.Gap = time.Since(downAt).Round(time.Second).String()
		}
		if lastID != "" {
			upEvent.Since = lastID
		}
		logEvent(upEvent)
		downLogged = false

		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			var msg ntfy.Msg
			if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
				continue
			}
			if msg.Event == "message" && msg.Message != "" {
				if msg.ID != "" {
					lastID = msg.ID
				}
				go handleMessage(msg, topicURL, sshTarget)
			}
		}
		reason := "eof"
		if err := scanner.Err(); err != nil {
			reason = err.Error()
			if ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "nssh: ntfy stream ended (%v) — reconnecting\n", err)
			}
		}
		markDown(reason)
		resp.Body.Close()

		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
	}
}

// handleMessage parses an incoming ntfy message envelope and dispatches it
// to the appropriate handler based on Kind. Unknown kinds are logged and
// dropped. clip-read-response is intentionally ignored — it's the remote
// shim's response to an outgoing read request, not for us.
func handleMessage(msg ntfy.Msg, topicURL, sshTarget string) {
	env, ok := wire.Parse(msg.Message)
	if !ok {
		fmt.Fprintf(os.Stderr, "nssh: ignoring unrecognized message (%d bytes)\n", len(msg.Message))
		logEvent(LogEvent{Event: "msg-unknown", Size: len(msg.Message)})
		return
	}

	// Decode the inline body once: handlers that need it use this slice and
	// logMessage uses its length for size accounting.
	var body []byte
	if env.Body != "" {
		decoded, err := base64.StdEncoding.DecodeString(env.Body)
		if err != nil {
			fmt.Fprintf(os.Stderr, "nssh: %s: base64 decode: %v\n", env.Kind, err)
			return
		}
		body = decoded
	}

	size := len(body)
	if msg.Attachment != nil {
		size = int(msg.Attachment.Size)
	}
	logMessage("in", env, size)

	switch env.Kind {
	case "open":
		handleOpen(env.URL, sshTarget)
	case "clip-write":
		handleClipWrite(env, msg.Attachment, body)
	case "clip-read-request":
		handleClipReadRequest(env, topicURL)
	case "clip-read-response":
		// Responses are for the remote shim, not us. Ignore.
	case "ping":
		handlePing(env, topicURL)
	case "pong":
		// Pongs are for whoever issued the matching ping. Ignore here.
	default:
		fmt.Fprintf(os.Stderr, "nssh: unknown envelope kind %q\n", env.Kind)
	}
}

// handlePing publishes a pong with the same correlation id. Used by a peer
// nssh process to verify this subscriber is alive (not just kill -0 alive).
func handlePing(env wire.Envelope, topicURL string) {
	resp := wire.Envelope{Kind: "pong", ID: env.ID}
	if err := wire.Publish(topicURL, resp, nil); err != nil {
		fmt.Fprintf(os.Stderr, "nssh: pong: %v\n", err)
		return
	}
	logMessage("out", resp, 0)
}

// pingTopic publishes a ping envelope to the topic and waits up to `timeout`
// for a pong with the matching correlation id. Returns true if any peer on
// the topic acked the ping. Used at session-start to decide whether an
// existing pidfile points at a live, responsive nssh or a wedged one.
//
// We open the subscriber *before* publishing so we don't race the pong:
// ntfy's "messages I have not seen yet" view starts at connect time.
func pingTopic(server, topic string, timeout time.Duration) bool {
	topicURL := strings.TrimRight(server, "/") + "/" + topic
	corrID := generateTopic() // reuse: just need an unguessable random string

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "GET", topicURL+"/json", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	// Publish ping *after* the subscriber is connected — otherwise the pong
	// can race ahead of us and we'd never see it.
	go func() {
		_ = wire.Publish(topicURL, wire.Envelope{Kind: "ping", ID: corrID}, nil)
	}()

	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		if ctx.Err() != nil {
			return false
		}
		var msg ntfy.Msg
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			continue
		}
		if msg.Event != "message" {
			continue
		}
		env, ok := wire.Parse(msg.Message)
		if !ok {
			continue
		}
		if env.Kind == "pong" && env.ID == corrID {
			return true
		}
	}
	return false
}
