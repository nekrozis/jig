package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	envRoot        = "JIG_ROOT"
	rootMarkerName = ".jig-root"
	rootMarkerBody = "jig-root 1\n"
)

// layout is the only place that knows the on-disk contract. Every path derives
// from root, so no caller has to re-invent a join and drift out of sync.
//
// One constraint is inherited from wshim and cannot be redesigned here: the
// .ref directory must be a sibling child of the shims themselves, because a
// running shim resolves <its own directory>\.ref\<its own file name>. Moving
// .ref anywhere else breaks every shim at runtime, silently.
type layout struct {
	root string
}

func (l layout) bin() string      { return filepath.Join(l.root, "bin") }
func (l layout) ref() string      { return filepath.Join(l.root, "bin", ".ref") }
func (l layout) tpl() string      { return filepath.Join(l.root, "templates") }
func (l layout) db() string       { return filepath.Join(l.root, "shim.db") }
func (l layout) lockPath() string { return filepath.Join(l.root, ".lock") }
func (l layout) marker() string   { return filepath.Join(l.root, rootMarkerName) }

func (l layout) shim(name string) string  { return filepath.Join(l.bin(), name+".exe") }
func (l layout) entry(name string) string { return filepath.Join(l.ref(), name+".exe") }

// findRoot decides which directory jig manages.
//
// jig deliberately does not adopt its own directory as the root. Package
// managers install tools wherever they like - scoop\apps\jig\current, a
// per-user tools folder, a winget link farm - and treating that location as
// the shim root would scatter bin\, .ref\ and shim.db into a tree jig does not
// own. The root is therefore always something stated up front:
//
//  1. --root <path>
//  2. $JIG_ROOT
//  3. the nearest ancestor of jig.exe carrying a .jig-root marker
//
// Rule 3 keeps "jig lives in <root>\bin" convenient without turning it into an
// assumption: installed anywhere else, jig simply finds no marker and asks.
func findRoot(flag string) (string, error) {
	if flag != "" {
		return filepath.Abs(flag)
	}
	if v := os.Getenv(envRoot); v != "" {
		return filepath.Abs(v)
	}
	if dir, ok := markerRoot(); ok {
		return dir, nil
	}
	return "", fmt.Errorf(
		"cannot determine the jig root\n"+
			"  --root was not given, $%s is not set, and no %s marker exists above %s\n"+
			"  run: jig init --root <dir>   (writes the marker; later runs find it unaided)",
		envRoot, rootMarkerName, selfPath())
}

// selfPath is only ever used for diagnostics, so a failure here must not
// change control flow.
func selfPath() string {
	exe, err := os.Executable()
	if err != nil {
		return "the jig executable"
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	return exe
}

// markerRoot walks from the executable's directory towards the drive root and
// stops at the first directory that declares itself a jig root.
func markerRoot() (string, bool) {
	exe, err := os.Executable()
	if err != nil {
		return "", false
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	for dir := filepath.Dir(exe); ; {
		if hasMarker(dir) {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

func hasMarker(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, rootMarkerName))
	return err == nil && info.Mode().IsRegular()
}

// ensure creates the minimal skeleton. bin\ and .ref\ are both writable;
// templates\ is created on demand by whoever needs it.
func (l layout) ensure() error {
	for _, d := range []string{l.root, l.bin(), l.ref()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fail(exitPartial, "cannot create directory %s: %v", d, err)
		}
	}
	return l.ensureMarker()
}

// ensureMarker records that this directory really is a jig root, so that later
// invocations can locate it without being told. Idempotent: an existing marker
// is left untouched, including one a user edited.
func (l layout) ensureMarker() error {
	p := l.marker()
	if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
		return nil
	}
	if err := writeAtomic(p, []byte(rootMarkerBody), 0o644); err != nil {
		return fail(exitPartial, "cannot write root marker %s: %v", p, err)
	}
	return nil
}

// acquire holds .lock exclusively for the duration of a mutation.
// Go's os.OpenFile always shares read/write, which is not mutual exclusion on
// Windows, so this goes straight to CreateFileW with a share mode of zero.
func (l layout) acquire() (func(), error) {
	deadline := time.Now().Add(5 * time.Second)
	path := l.lockPath()
	for {
		h, err := createExclusive(path)
		if err == nil {
			return func() { syscall.CloseHandle(h) }, nil
		}
		if !errors.Is(err, errSharingViolation) {
			return nil, fail(exitLocked, "cannot create lock file %s: %v", path, err)
		}
		if time.Now().After(deadline) {
			return nil, fail(exitLocked, "lock contention: %s stayed busy for over 5s", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func createExclusive(path string) (syscall.Handle, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return syscall.CreateFile(p,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0, // share mode 0: any other open fails
		nil,
		syscall.OPEN_ALWAYS,
		syscall.FILE_ATTRIBUTE_HIDDEN,
		0)
}
