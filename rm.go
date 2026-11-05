package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

func cmdRm(l layout, args []string) error {
	var names []string
	force := false

	for _, a := range args {
		switch {
		case a == "--force" || a == "-f":
			force = true
		case a == "--keep-target":
			// This is the default; accepted so scripts can be explicit.
		case strings.HasPrefix(a, "-"):
			return usageErr("unknown option %s", a)
		default:
			names = append(names, a)
		}
	}
	if len(names) == 0 {
		return usageErr("usage: jig rm <name>... [--force]")
	}

	st, _, err := l.loadState()
	if err != nil {
		return err
	}
	unlock, err := l.acquire()
	if err != nil {
		return err
	}
	defer unlock()

	failed := 0
	var firstErr error
	for _, n := range names {
		if err := rmOne(l, st, n, force); err != nil {
			fmt.Fprintf(os.Stderr, "error: %s: %v\n", n, err)
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

func rmOne(l layout, st state, name string, force bool) error {
	rec, ok := st.lookup(name)
	if !ok {
		return fail(exitInvalid,
			"not recorded in shim.db, refusing to delete (this keeps a stray argument from erasing someone else's file)")
	}

	binPath := l.shim(rec.Name)
	entry := l.entry(rec.Name)

	// Do not delete a file the user replaced: a digest mismatch means this is
	// no longer what jig put there.
	if got, err := fileSHA256(binPath); err == nil {
		if rec.SHA256 != "" && got != rec.SHA256 && !force {
			return fail(exitInvalid,
				"%s is no longer the shim jig wrote (digest mismatch)\n  add --force if you really want it gone",
				stripLong(binPath))
		}
	}

	// The order is the reverse of creation: remove bin first, unlink .ref
	// second. An interruption leaves a state that is not runnable but is
	// structurally complete, which check can recognise.
	if err := os.Remove(binPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail(exitPartial, "cannot remove %s: %v", stripLong(binPath), err)
	}
	// os.Remove on a symlink deletes the link only; it never follows.
	if err := os.Remove(entry); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fail(exitPartial, "cannot remove %s: %v", stripLong(entry), err)
	}

	r := newRecord("rm", rec.Name)
	if err := l.appendRecord(r); err != nil {
		return err
	}
	fmt.Printf("removed %s\n", rec.Name)
	return nil
}
