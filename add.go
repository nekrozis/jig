package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func cmdAdd(l layout, args []string) error {
	var pos, aliases []string
	var force, allowScript bool

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--force" || a == "-f":
			force = true
		case a == "--allow-script":
			allowScript = true
		case a == "--as":
			if i+1 >= len(args) {
				return usageErr("--as needs a name")
			}
			aliases = append(aliases, args[i+1])
			i++
		case strings.HasPrefix(a, "--as="):
			aliases = append(aliases, strings.TrimPrefix(a, "--as="))
		case strings.HasPrefix(a, "-"):
			return usageErr("unknown option %s", a)
		default:
			pos = append(pos, a)
		}
	}
	var name, targetArg string
	switch len(pos) {
	case 1:
		targetArg = pos[0]
		derived, err := defaultName(targetArg)
		if err != nil {
			return fail(exitInvalid,
				"cannot derive a name from %s: %v\n  use: jig add <name> <target>", stripLong(targetArg), err)
		}
		name = derived
	case 2:
		name, targetArg = pos[0], pos[1]
	default:
		return usageErr("usage: jig add [<name>] <target> [--as <alias>]... [--force] [--allow-script]")
	}

	names := append([]string{name}, aliases...)
	seen := map[string]bool{}
	for _, n := range names {
		if err := validateName(n); err != nil {
			return fail(exitInvalid, "invalid name %q: %v", n, err)
		}
		k := strings.ToLower(n)
		if seen[k] {
			return fail(exitInvalid, "duplicate name (Windows compares case-insensitively): %s", n)
		}
		seen[k] = true
	}

	target, subsystem, err := inspectTarget(targetArg, allowScript)
	if err != nil {
		return err
	}
	// Cycle check: a target living inside the shim directory means a -> b -> a,
	// or a -> a, and the chain would never terminate.
	if within(l.ref(), target) || within(l.bin(), target) {
		return fail(exitInvalid, "target is inside the shim directory and would form a cycle: %s",
			stripLong(target))
	}
	// Pointing at a template just replays the same forward once more; it also
	// cannot work, because templates\_ref does not exist.
	if within(l.tpl(), target) {
		return fail(exitInvalid, "target is a shim template, not a runnable program: %s",
			stripLong(target))
	}

	if err := l.ensure(); err != nil {
		return err
	}
	// Templates go first: every action that can fail happens before
	// bin\<name>.exe exists.
	if _, _, err := l.ensureTemplate(subsystem); err != nil {
		return err
	}

	unlock, err := l.acquire()
	if err != nil {
		return err
	}
	defer unlock()

	st, _, err := l.loadState()
	if err != nil {
		return err
	}

	var firstErr error
	failed := 0
	for _, name := range names {
		_, registered := st.lookup(name)
		if err := addOne(l, name, target, subsystem, force, registered); err != nil {
			fmt.Fprintf(os.Stderr, "error: %s: %v\n", name, err)
			failed++
			if firstErr == nil {
				firstErr = already(codeOf(err))
			}
		}
	}
	if failed == 0 {
		return nil
	}
	if failed == len(names) {
		return firstErr
	}
	return already(exitPartial)
}

// addOne must keep this order: _ref first, bin second. The moment
// bin\<name>.exe becomes visible the user can invoke it, so "visible" has to
// mean "usable".
func addOne(l layout, name, target string, subsystem uint16, force bool, registered bool) error {
	entry := l.entry(name)
	binPath := l.shim(name)
	linkOK := false

	// jig may live in bin itself. Adding a shim under its own name would
	// overwrite the running manager, so refuse before anything is written.
	if samePath(binPath, selfPath()) {
		return fail(exitInvalid, "refusing to overwrite the running jig executable: %s", stripLong(binPath))
	}

	// Differing only in case means the same file. Refuse outright, otherwise
	// the manifest entry would be silently renamed under the user.
	for _, dir := range []string{l.ref(), l.bin()} {
		if actual, ok := actualName(dir, name+".exe"); ok && actual != name+".exe" {
			return fail(exitInvalid,
				"entry %s already exists (differs only in case; Windows treats it as the same file)", actual)
		}
	}

	// A file under bin that jig never wrote belongs to whoever put it there.
	// bin is allowed to be shared with another package manager, so replacing it
	// silently would destroy someone else's shim: the same reasoning that makes
	// check leave such a file alone and rm refuse it. Checked before the _ref
	// write so a refusal leaves nothing behind.
	if got, err := fileSHA256(binPath); err == nil && !isShimBytes(got) && !force {
		return fail(exitInvalid,
			"%s already exists and was not written by jig (digest matches no template)\n"+
				"  add --force if you really want to replace it", stripLong(binPath))
	}

	if _, err := os.Lstat(entry); err == nil {
		if cur, err := resolveFinal(entry); err == nil && samePath(cur, target) {
			linkOK = true
		} else if !force {
			cur, err := os.Readlink(entry)
			if err != nil {
				return fail(exitInvalid,
					"%s exists but is not a symlink; use --force to replace it", stripLong(entry))
			}
			return fail(exitInvalid, "%s already points at %s (use --force to replace)", name, cur)
		}
	}

	if !linkOK {
		tmp := filepath.Join(l.ref(), name+".exe.tmp."+randHex(4))
		os.Remove(tmp)
		if err := createSymlink(target, tmp); err != nil {
			return err
		}
		if err := os.Rename(tmp, entry); err != nil {
			os.Remove(tmp)
			return fail(exitPartial, "cannot replace %s: %v", entry, err)
		}
	}

	want := templateSum(subsystem)
	got, err := fileSHA256(binPath)
	if err != nil || got != want {
		if err := os.MkdirAll(l.bin(), 0o755); err != nil {
			return fail(exitPartial, "cannot create %s: %v", l.bin(), err)
		}
		if err := l.placeShim(name, subsystem); err != nil {
			return fail(exitPartial, "cannot write %s: %v", binPath, err)
		}
	}

	if linkOK && got == want && registered {
		fmt.Printf("%s: already points at %s, nothing to do\n", name, stripLong(target))
		return nil
	}

	r := newRecord("add", name)
	r.Target = stripLong(target)
	r.Link = "symlink"
	r.Subsystem = subsystem
	r.SHA256 = want
	if err := l.appendRecord(r); err != nil {
		return err
	}

	fmt.Printf("added %s -> %s [%s]\n", name, stripLong(target), subsystemName(subsystem))
	return nil
}

// inspectTarget checks, in order: exists, regular file, .exe, PE, machine,
// subsystem. The order carries meaning - the first failure gives the most
// specific explanation, so the user sees the real cause.
func inspectTarget(raw string, allowScript bool) (string, uint16, error) {
	final, err := resolveFinal(raw)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", 0, fail(exitInvalid, "target does not exist: %s", raw)
		}
		return "", 0, fail(exitInvalid, "target cannot be resolved: %v", err)
	}

	info, err := os.Stat(final)
	if err != nil {
		return "", 0, fail(exitInvalid, "cannot read target: %v", err)
	}
	if !info.Mode().IsRegular() {
		return "", 0, fail(exitInvalid, "target is not a regular file: %s", stripLong(final))
	}

	switch strings.ToLower(filepath.Ext(final)) {
	case ".bat", ".cmd":
		if !allowScript {
			return "", 0, fail(exitInvalid,
				"%s is a batch file: its arguments get re-parsed by cmd.exe, so %% ^ & are eaten\n"+
					"  pass --allow-script if you really need this", stripLong(final))
		}
		return final, subsystemCUI, nil
	case ".exe":
		// fine
	default:
		return "", 0, fail(exitInvalid,
			"target must carry an .exe extension: %s\n"+
				"  without one, std's resolve_exe tries %s.exe first and may quietly launch a neighbour",
			stripLong(final), stripLong(final))
	}

	h, err := readPE(final)
	if err != nil {
		return "", 0, fail(exitInvalid, "%s: %v", stripLong(final), err)
	}
	if h.Machine == 0xaa64 {
		return "", 0, fail(exitInvalid,
			"arm64 target is not supported: the embedded shim is x64 and would fail to start: %s",
			stripLong(final))
	}
	if _, ok := machineNames[h.Machine]; !ok {
		return "", 0, fail(exitInvalid, "unsupported machine 0x%04X: %s", h.Machine, stripLong(final))
	}
	if h.Subsystem != subsystemCUI && h.Subsystem != subsystemGUI {
		return "", 0, fail(exitInvalid, "unsupported subsystem %d (only 2=GUI / 3=console): %s",
			h.Subsystem, stripLong(final))
	}
	return final, h.Subsystem, nil
}

// validateName rejects names that look acceptable but would be silently
// rewritten by Win32.
func defaultName(target string) (string, error) {
	base := filepath.Base(target)
	for _, ext := range []string{".exe", ".bat", ".cmd"} {
		if strings.EqualFold(filepath.Ext(base), ext) {
			base = strings.TrimSuffix(base, filepath.Ext(base))
			break
		}
	}
	if base == "" {
		return "", errors.New("the target has no file name")
	}
	if err := validateName(base); err != nil {
		return "", err
	}
	return base, nil
}

func validateName(n string) error {
	if n == "" || len(n) > 127 {
		return errors.New("length must be between 1 and 127")
	}
	if strings.EqualFold(filepath.Ext(n), ".exe") {
		return errors.New("do not include .exe; it is appended automatically")
	}
	if strings.ContainsAny(n, `\/:*?"<>|`) {
		return errors.New(`must not contain \ / : * ? " < > |`)
	}
	for _, r := range n {
		if r < 0x20 {
			return errors.New("must not contain control characters")
		}
	}
	if strings.HasSuffix(n, " ") || strings.HasSuffix(n, ".") {
		return errors.New("must not end with a space or a dot (Win32 trims them silently)")
	}

	base := n
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	switch strings.ToUpper(base) {
	case "CON", "PRN", "AUX", "NUL":
		return errors.New("is a reserved device name")
	}
	if len(base) == 4 && base[3] >= '1' && base[3] <= '9' &&
		(strings.EqualFold(base[:3], "COM") || strings.EqualFold(base[:3], "LPT")) {
		return errors.New("is a reserved device name")
	}
	return nil
}
