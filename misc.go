package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

func cmdPath(l layout, args []string) error {
	if len(args) > 0 {
		return usageErr("path takes no arguments")
	}
	fmt.Println(l.bin())
	return nil
}

func cmdWhich(l layout, args []string) error {
	asJSON := false
	name := ""
	for _, a := range args {
		switch {
		case a == "--json":
			asJSON = true
		case strings.HasPrefix(a, "-"):
			return usageErr("unknown option %s", a)
		default:
			if name != "" {
				return usageErr("which takes exactly one name")
			}
			name = a
		}
	}
	if name == "" {
		return usageErr("usage: jig which <name> [--json]")
	}

	s := probeName(l, name)
	switch {
	case !s.HasRef:
		return fail(exitInvalid, "%s: no entry under _ref", name)
	case s.LinkErr != nil:
		return fail(exitInvalid, "%s: entry is not a symlink (%v)", name, s.LinkErr)
	case s.FinalErr != nil:
		return fail(exitInvalid, "%s: link is dangling (%s)", name, s.LinkRaw)
	}

	if asJSON {
		out := map[string]any{
			"name":      s.Name,
			"target":    stripLong(s.Final),
			"link":      s.LinkRaw,
			"subsystem": subsystemName(s.Subsystem),
			"shim":      stripLong(l.shim(name)),
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	fmt.Println(stripLong(s.Final))
	return nil
}

// cmdGc handles exactly the three cases on the rule list: leftover temporary
// files, stale manifest records, and manifest compaction. It defaults to a
// dry run - even a fully spelled-out command only prints.
func cmdGc(l layout, args []string) error {
	apply := false
	for _, a := range args {
		switch a {
		case "--dry-run":
			apply = false
		case "--apply":
			apply = true
		default:
			return usageErr("unknown option %s", a)
		}
	}

	if !apply {
		st, events, err := l.loadState()
		if err != nil {
			return err
		}
		plan, _, _, _ := planGc(l, st, events)
		if len(plan) == 0 {
			fmt.Println("nothing to do")
			return nil
		}
		for _, p := range plan {
			fmt.Println("  " + p)
		}
		fmt.Println("(dry run, nothing changed; add --apply to execute)")
		return nil
	}

	unlock, err := l.acquire()
	if err != nil {
		return err
	}
	defer unlock()

	// Compute the plan under the lock. Listing before acquiring it could
	// delete a temporary file another add is about to rename, or drop a
	// record that was written in between.
	st, events, err := l.loadState()
	if err != nil {
		return err
	}
	plan, tmpFiles, stale, needCompact := planGc(l, st, events)
	if len(plan) == 0 {
		fmt.Println("nothing to do")
		return nil
	}
	for _, p := range plan {
		fmt.Println("  " + p)
	}

	for _, p := range tmpFiles {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fail(exitPartial, "cannot remove %s: %v", p, err)
		}
	}
	for _, n := range stale {
		if err := l.appendRecord(newRecord("rm", n)); err != nil {
			return err
		}
	}
	if needCompact {
		cur, _, err := l.loadState()
		if err != nil {
			return err
		}
		if err := l.compact(cur); err != nil {
			return err
		}
	}
	fmt.Printf("done: %d item(s)\n", len(plan))
	return nil
}

func planGc(l layout, st state, events int) (plan, tmpFiles, stale []string, needCompact bool) {
	for _, dir := range []string{l.bin(), l.ref(), l.tpl()} {
		for _, n := range listTemps(dir) {
			tmpFiles = append(tmpFiles, filepath.Join(dir, n))
		}
	}
	sort.Strings(tmpFiles)
	for _, p := range tmpFiles {
		plan = append(plan, "remove leftover temporary file "+p)
	}

	for _, r := range st {
		if _, err := os.Lstat(l.shim(r.Name)); err != nil {
			if _, err := os.Lstat(l.entry(r.Name)); err != nil {
				stale = append(stale, r.Name)
			}
		}
	}
	sort.Strings(stale)
	for _, n := range stale {
		plan = append(plan, "drop stale manifest record "+n)
	}

	size := int64(0)
	if info, err := os.Stat(l.db()); err == nil {
		size = info.Size()
	}
	needCompact = events > 2000 || size > 1<<20
	if needCompact {
		plan = append(plan, fmt.Sprintf("compact shim.db (%d events, %d bytes)", events, size))
	}
	return plan, tmpFiles, stale, needCompact
}

// cmdInit creates the skeleton and writes both templates. It deliberately
// creates no shims. It is also the command that plants the root marker, so it
// is the only one that must be told where the root is on a virgin tree.
func cmdInit(l layout, args []string) error {
	force := false
	for _, a := range args {
		switch a {
		case "--force":
			force = true
		default:
			return usageErr("unknown option %s", a)
		}
	}
	if err := l.ensureDirs(); err != nil {
		return err
	}

	unlock, err := l.acquire()
	if err != nil {
		return err
	}
	defer unlock()

	if err := l.ensureMarker(); err != nil {
		return err
	}
	for _, sub := range []uint16{subsystemCUI, subsystemGUI} {
		if _, _, err := l.installTemplate(sub, force); err != nil {
			return err
		}
	}
	fmt.Printf("root      %s\n", l.root)
	fmt.Printf("marker    %s\n", l.marker())
	fmt.Printf("bin       %s\n", l.bin())
	fmt.Printf("ref       %s\n", l.ref())
	fmt.Printf("templates %s\n", l.tpl())
	fmt.Printf("\nAdd %s to PATH to use the shims.\n", l.bin())
	return nil
}

// cmdVersion prints the embedded template digests so they can be compared with
// whatever the upstream release published. This is the only self-identification
// a user can perform: "which two shims did I actually get?"
func cmdVersion(args []string) error {
	asJSON := false
	for _, a := range args {
		switch a {
		case "--json":
			asJSON = true
		case "-h", "--help":
			return usageErr("version [--json]")
		default:
			return usageErr("unknown option %s", a)
		}
	}

	type tpl struct {
		Name      string `json:"name"`
		SHA256    string `json:"sha256"`
		Size      int    `json:"size"`
		Subsystem uint16 `json:"subsystem"`
	}
	var out struct {
		Jig      string `json:"jig"`
		Go       string `json:"go"`
		Upstream struct {
			Repo   string `json:"repo"`
			Tag    string `json:"tag"`
			Commit string `json:"commit"`
		} `json:"upstream"`
		Templates []tpl `json:"templates"`
	}
	out.Jig, out.Go = version, runtime.Version()
	out.Upstream.Repo = upstreamRepo
	out.Upstream.Tag = upstreamTag
	out.Upstream.Commit = upstreamCommit
	for _, sub := range []uint16{subsystemCUI, subsystemGUI} {
		name, data := embeddedTemplate(sub)
		h, err := parsePE(data)
		if err != nil {
			return fail(exitPartial, "cannot parse embedded template %s: %v", name, err)
		}
		out.Templates = append(out.Templates, tpl{name, sha256Hex(data), len(data), h.Subsystem})
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(out)
	}
	fmt.Printf("jig %s (%s)\n", out.Jig, out.Go)
	fmt.Printf("templates from %s %s (%s)\n", upstreamRepo, upstreamTag, upstreamCommit[:12])
	for _, t := range out.Templates {
		fmt.Printf("  %-12s sha256=%s  size=%d  subsystem=%d\n", t.Name, t.SHA256, t.Size, t.Subsystem)
	}
	return nil
}
