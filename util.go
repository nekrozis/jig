package main

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const (
	errPrivilegeNotHeld   = syscall.Errno(1314) // ERROR_PRIVILEGE_NOT_HELD
	errFilenameExcedRange = syscall.Errno(206)  // ERROR_FILENAME_EXCED_RANGE
	errSharingViolation   = syscall.Errno(32)   // ERROR_SHARING_VIOLATION
	errPathNotFound       = syscall.Errno(3)    // ERROR_PATH_NOT_FOUND
	errTooManyLinks       = syscall.Errno(1142) // ERROR_TOO_MANY_LINKS
)

func randHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "0000"
	}
	return hex.EncodeToString(b)
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeAtomic guarantees that the destination path holds either the old
// content or the complete new content, never a prefix. Temporary names look
// like <base>.tmp.<rand>; check and gc recognise leftovers by ".tmp.".
func writeAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)

	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp, perm); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// resolveFinal expands every reparse point and returns the final path without
// the \\?\ prefix. A missing target surfaces as an error wrapping
// os.ErrNotExist - which is exactly the "dangling" predicate.
func resolveFinal(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	final, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return filepath.Clean(final), nil
}

// createSymlink creates a file-level symlink. The target must be absolute:
// that is a runtime contract, not a preference.
func createSymlink(target, link string) error {
	err := os.Symlink(target, link)
	if err == nil {
		return nil
	}

	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case errPrivilegeNotHeld:
			return fail(exitPrivilege,
				"cannot create a symlink (%v)\n"+
					"  enable Developer Mode (Settings > System > For developers) or run once as administrator.\n"+
					"  there is no fallback: a shim only understands read_link at runtime.", err)
		case errFilenameExcedRange, errPathNotFound:
			// Long paths need an explicit \\?\ prefix, otherwise
			// CreateSymbolicLinkW reports 206.
			if e2 := os.Symlink(longPath(target), longPath(link)); e2 == nil {
				return nil
			}
		}
	}
	return fail(exitInvalid, "cannot create symlink %s -> %s: %v", link, target, err)
}

func longPath(p string) string {
	if strings.HasPrefix(p, `\\?\`) || strings.HasPrefix(p, `\\.\`) {
		return p
	}
	if !filepath.IsAbs(p) {
		if abs, err := filepath.Abs(p); err == nil {
			p = abs
		}
	}
	return `\\?\` + p
}

func stripLong(p string) string {
	if strings.HasPrefix(p, `\\?\UNC\`) {
		return `\\` + p[len(`\\?\UNC\`):]
	}
	if strings.HasPrefix(p, `\\?\`) {
		return p[len(`\\?\`):]
	}
	return p
}

// samePath compares with Windows semantics: case-insensitive, Clean first.
func samePath(a, b string) bool {
	return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
}

// within reports whether child sits under parent (equality included).
func within(parent, child string) bool {
	p := strings.ToLower(filepath.Clean(parent))
	c := strings.ToLower(filepath.Clean(child))
	if p == c {
		return true
	}
	if !strings.HasSuffix(p, string(os.PathSeparator)) {
		p += string(os.PathSeparator)
	}
	return strings.HasPrefix(c, p)
}

// listNames returns the base name (extension stripped) of every *.exe entry,
// symlinks included.
func listNames(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if !strings.EqualFold(filepath.Ext(n), ".exe") {
			continue
		}
		out = append(out, n[:len(n)-len(".exe")])
	}
	return out
}

// listTemps returns leftover temporary files (*.tmp.*) in dir.
func listTemps(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if strings.Contains(strings.ToLower(e.Name()), ".tmp.") {
			out = append(out, e.Name())
		}
	}
	return out
}

// selfBaseName returns the lower-cased file name jig runs as, so bookkeeping
// can keep jig itself out of the shim inventory. jig is commonly installed at
// <root>\bin\jig.exe, which would otherwise be classified as an unregistered
// executable.
func selfBaseName() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	return strings.ToLower(filepath.Base(exe))
}

// actualName returns the real spelling in dir of a name differing only in
// case. On Windows os.Lstat matches regardless of case, so without comparing
// the actual spelling "add WHOAMI" would quietly reuse whoami's entry and
// rename the manifest record.
func actualName(dir, name string) (string, bool) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, e := range ents {
		if strings.EqualFold(e.Name(), name) {
			return e.Name(), true
		}
	}
	return "", false
}
