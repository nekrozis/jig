package main

import (
	"os"
	"path/filepath"
	"strings"
)

// entryStatus is the on-disk reality for one name; ls and check share this
// single probe.
type entryStatus struct {
	Name      string
	HasShim   bool
	HasRef    bool
	LinkRaw   string // raw readlink output
	LinkErr   error  // non-nil when the entry is not a symlink
	Relative  bool   // relative link: depends on the shape of .ref itself
	Final     string // fully expanded path
	FinalErr  error  // os.ErrNotExist when dangling
	Cycle     bool   // final path lands inside the shim directory
	Subsystem uint16 // target subsystem, 0 when unknown
	ShimSHA   string
	ShimOK    bool // matches the embedded template digest
}

func probeName(l layout, name string) entryStatus {
	s := entryStatus{Name: name}
	shimPath := l.shim(name)
	entry := l.entry(name)

	if _, err := os.Lstat(shimPath); err == nil {
		s.HasShim = true
		if sum, err := fileSHA256(shimPath); err == nil {
			s.ShimSHA = sum
		}
	}
	if _, err := os.Lstat(entry); err != nil {
		return s
	}
	s.HasRef = true

	raw, err := os.Readlink(entry)
	if err != nil {
		s.LinkErr = err
		return s
	}
	s.LinkRaw = raw
	s.Relative = !filepath.IsAbs(raw)

	final, err := filepath.EvalSymlinks(entry)
	if err != nil {
		s.FinalErr = err
		return s
	}
	s.Final = final
	s.Cycle = within(l.ref(), final) || within(l.bin(), final)

	// A batch file is not a PE image but is still a legal target (std wraps it
	// in cmd.exe); treat it as console.
	switch strings.ToLower(filepath.Ext(final)) {
	case ".bat", ".cmd":
		s.Subsystem = subsystemCUI
		s.ShimOK = s.HasShim && s.ShimSHA == templateSum(subsystemCUI)
		return s
	}

	if h, err := readPE(final); err == nil {
		s.Subsystem = h.Subsystem
		s.ShimOK = s.HasShim && s.ShimSHA == templateSum(h.Subsystem)
	}
	return s
}
