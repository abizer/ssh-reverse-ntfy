package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
)

// buildVersion is set by ldflags at release build time (e.g. in the homebrew
// formula). For go install / go build with a tagged module it's empty and we
// fall back to debug.ReadBuildInfo.
var buildVersion string

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  nssh [--ssh|--mosh|--et] [--join|--replace|--new] <host> [ssh args...]")
	fmt.Fprintln(os.Stderr, "                                              open a session")
	fmt.Fprintln(os.Stderr, "  nssh infect [--force] <host>               install on a remote host")
	fmt.Fprintln(os.Stderr, "  nssh infect [--force] self                 symlink personas on this machine")
	fmt.Fprintln(os.Stderr, "  nssh status [--tail]                       show active sessions")
	fmt.Fprintln(os.Stderr, "  nssh sweep [--all|--older <dur>] <host>    kill orphan mosh-servers on a host")
	fmt.Fprintln(os.Stderr, "  nssh --version                             print version info")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "session collision flags (when another nssh is already attached to <host>):")
	fmt.Fprintln(os.Stderr, "  --join      share the existing ntfy topic (no prompt)")
	fmt.Fprintln(os.Stderr, "  --replace   kill the existing nssh, take over with a fresh topic")
	fmt.Fprintln(os.Stderr, "  --new       generate a fresh topic without killing the existing")
	os.Exit(1)
}

func printVersion() {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		fmt.Println("nssh (build info unavailable)")
		return
	}
	v := buildVersion
	if v == "" {
		v = info.Main.Version
	}
	if v == "" {
		v = "(devel)"
	}
	fmt.Printf("nssh %s\n", v)
	fmt.Printf("  go      %s\n", info.GoVersion)
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			fmt.Printf("  commit  %s\n", s.Value)
		case "vcs.time":
			fmt.Printf("  built   %s\n", s.Value)
		case "vcs.modified":
			if s.Value == "true" {
				fmt.Println("  dirty   true")
			}
		case "GOOS":
			fmt.Printf("  os      %s\n", s.Value)
		case "GOARCH":
			fmt.Printf("  arch    %s\n", s.Value)
		}
	}
}

func main() {
	persona := filepath.Base(os.Args[0])
	switch persona {
	case "xdg-open", "sensible-browser", "xclip", "wl-copy", "wl-paste":
		shimMain(persona, os.Args[1:])
		return
	}
	// Invoked as nssh (or equivalent). Route on first arg.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "infect":
			infectCmd(os.Args[2:])
			return
		case "status":
			statusCmd(os.Args[2:])
			return
		case "sweep":
			sweepCmd(os.Args[2:])
			return
		case "-v", "--version":
			printVersion()
			return
		case "-h", "--help":
			usage()
		}
	}
	nsshMain()
}
