// jig - install-time manager for wshim shims.
//
// It does one thing: maintain <root>\bin and <root>\bin\.ref so that a running
// shim is left with exactly two statements, read_link and spawn. Every part of
// the contract is therefore validated here, at install time; a violation at
// runtime is a process abort, not a readable error.
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

const (
	version = "0.3.0"
)

const (
	exitOK        = 0
	exitPartial   = 1
	exitUsage     = 2
	exitInvalid   = 3
	exitPrivilege = 4
	exitLocked    = 5
)

// failure carries an exit code; anything else defaults to exitPartial.
// reported means the caller already printed this error item by item, and
// report must not print it a second time.
type failure struct {
	code     int
	err      error
	reported bool
}

func (f *failure) Error() string {
	if f.err == nil {
		return ""
	}
	return f.err.Error()
}

func (f *failure) Unwrap() error { return f.err }

func fail(code int, format string, a ...any) error {
	return &failure{code: code, err: fmt.Errorf(format, a...)}
}

// already marks an error as printed, keeping only the exit code.
func already(code int) error { return &failure{code: code, reported: true} }

func codeOf(err error) int {
	var f *failure
	if errors.As(err, &f) {
		return f.code
	}
	return exitPartial
}

func usageErr(format string, a ...any) error {
	return &failure{code: exitUsage, err: fmt.Errorf(format, a...)}
}

func main() { os.Exit(run()) }

func run() int {
	args, rootFlag := extractRoot(os.Args[1:])
	if len(args) == 0 {
		printUsage()
		return exitUsage
	}

	switch args[0] {
	case "help", "-h", "--help":
		printUsage()
		return exitOK
	}

	if err := verifyEmbedded(); err != nil {
		fmt.Fprintln(os.Stderr, "error: "+err.Error())
		return exitPartial
	}

	// version touches no disk and must run before findRoot: a standalone jig
	// has to be able to identify itself without being pointed at a root.
	if args[0] == "version" || args[0] == "-V" || args[0] == "--version" {
		return report(cmdVersion(args[1:]))
	}

	root, err := findRoot(rootFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: "+err.Error())
		return exitUsage
	}
	l := layout{root: root}

	cmd, rest := args[0], args[1:]
	switch cmd {
	case "add":
		err = cmdAdd(l, rest)
	case "rm", "remove":
		err = cmdRm(l, rest)
	case "ls", "list":
		err = cmdLs(l, rest)
	case "check":
		err = cmdCheck(l, rest)
	case "which":
		err = cmdWhich(l, rest)
	case "gc":
		err = cmdGc(l, rest)
	case "path":
		err = cmdPath(l, rest)
	case "init":
		err = cmdInit(l, rest)
	default:
		err = usageErr("unknown subcommand %q (available: add rm ls check which gc path init version help)", cmd)
	}
	return report(err)
}

func report(err error) int {
	if err == nil {
		return exitOK
	}
	var f *failure
	if errors.As(err, &f) {
		if f.err != nil && !f.reported {
			fmt.Fprintln(os.Stderr, "error: "+f.err.Error())
		}
		return f.code
	}
	fmt.Fprintln(os.Stderr, "error: "+err.Error())
	return exitPartial
}

// extractRoot lets --root appear anywhere; the first remaining word is the
// subcommand.
func extractRoot(args []string) ([]string, string) {
	var out []string
	var root string
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--root":
			if i+1 < len(args) {
				root = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--root="):
			root = strings.TrimPrefix(a, "--root=")
		default:
			out = append(out, a)
		}
	}
	return out, root
}

func printUsage() {
	fmt.Print(`jig - install-time manager for wshim shims

Usage:
  jig [--root <dir>] <subcommand> [args...]

Subcommands:
  add     <name> <target> [--as <alias>]... [--force] [--allow-script]
  rm      <name>... [--force] [--keep-target]
  ls      [--json]
  check   [<name>...] [--fix] [--json]
  which   <name> [--json]
  gc      [--dry-run|--apply]
  path
  init    [--force]
  version [--json]

Root resolution:
  --root <dir>  >  $JIG_ROOT  >  nearest .jig-root marker above jig.exe

  jig never treats its own location as the root: a package manager may install
  it anywhere. The first run in a new tree is "jig init --root <dir>", which
  writes the marker every later invocation looks for.

Exit codes: 0 ok / 1 partial failure / 2 usage / 3 invalid state / 4 privilege / 5 lock contention
`)
}
