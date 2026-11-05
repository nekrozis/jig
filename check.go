package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"
)

type finding struct {
	Name   string `json:"name"`
	Level  string `json:"level"` // ok | warn | bad | fixed
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

type checker struct {
	l       layout
	fix     bool
	items   []finding
	unfixed int
}

// record reports one conclusion. When a fixFn is supplied and --fix is active,
// a failure rewrites the entry in place as fixed. One issue keeps one line, so
// bad/fixed pairs never double the output.
func (c *checker) report(name, level, code, detail, done string, fixFn func() error) {
	i := len(c.items)
	c.items = append(c.items, finding{Name: name, Level: level, Code: code, Detail: detail})
	if level == "bad" {
		c.unfixed++
	}
	if !c.fix || fixFn == nil {
		return
	}
	if err := fixFn(); err != nil {
		c.items[i].Detail = detail + "; fix failed: " + err.Error()
		return
	}
	c.items[i].Level = "fixed"
	c.items[i].Detail = done
	if level == "bad" {
		c.unfixed--
	}
}

func cmdCheck(l layout, args []string) error {
	var only []string
	asJSON, fix := false, false
	for _, a := range args {
		switch {
		case a == "--fix":
			fix = true
		case a == "--json":
			asJSON = true
		case strings.HasPrefix(a, "-"):
			return usageErr("unknown option %s", a)
		default:
			only = append(only, a)
		}
	}

	st, _, err := l.loadState()
	if err != nil {
		return err
	}
	c := &checker{l: l, fix: fix}
	for _, t := range collectNames(l, st, only) {
		c.checkName(t.Name, t.Registered)
	}
	c.checkTemps()
	c.checkTemplates()
	c.checkUnregistered(st, only)

	sort.SliceStable(c.items, func(i, j int) bool {
		if !strings.EqualFold(c.items[i].Name, c.items[j].Name) {
			return strings.ToLower(c.items[i].Name) < strings.ToLower(c.items[j].Name)
		}
		return c.items[i].Code < c.items[j].Code
	})

	if asJSON {
		if c.items == nil {
			c.items = []finding{}
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(c.items); err != nil {
			return err
		}
	} else {
		printFindings(c.items)
	}

	if c.unfixed > 0 {
		return &failure{code: exitInvalid}
	}
	return nil
}

// collectNames takes the union of "manifest entries, .ref entries, and *.exe
// files under bin" - anything that exists has to be checked. Registered
// distinguishes "in the manifest" from "merely named on the command line",
// because "neither file exists" means different things in the two cases.
type subject struct {
	Name       string
	Registered bool
}

func collectNames(l layout, st state, only []string) []subject {
	seen := map[string]subject{}
	self := selfBaseName()
	for k, r := range st {
		seen[k] = subject{Name: r.Name, Registered: true}
	}
	for _, n := range listNames(l.ref()) {
		if _, ok := seen[strings.ToLower(n)]; !ok {
			seen[strings.ToLower(n)] = subject{Name: n}
		}
	}
	for _, n := range listNames(l.bin()) {
		if self != "" && strings.EqualFold(n+".exe", self) {
			continue // jig managing itself is not an entry
		}
		if _, ok := seen[strings.ToLower(n)]; !ok {
			seen[strings.ToLower(n)] = subject{Name: n}
		}
	}

	if len(only) > 0 {
		out := make([]subject, 0, len(only))
		for _, n := range only {
			if s, ok := seen[strings.ToLower(n)]; ok {
				out = append(out, s)
			} else {
				out = append(out, subject{Name: n})
			}
		}
		return out
	}
	out := make([]subject, 0, len(seen))
	for _, s := range seen {
		out = append(out, s)
	}
	return out
}

func (c *checker) checkName(name string, registered bool) {
	s := probeName(c.l, name)

	if !registered && !s.HasShim && !s.HasRef {
		c.report(name, "bad", "missing", "no such entry", "", nil)
		return
	}

	switch {
	case !s.HasShim && !s.HasRef:
		c.report(name, "bad", "stale", "manifest has a record, but neither file exists on disk",
			"manifest record dropped", func() error {
				return c.l.appendRecord(newRecord("rm", name))
			})

	case s.HasShim && !s.HasRef:
		// Only bytes jig actually wrote may be deleted as an orphan. An
		// executable jig never created belongs to whoever put it there, and
		// deleting it would be data loss.
		if !isShimBytes(s.ShimSHA) {
			c.report(name, "warn", "foreign",
				"executable under bin was not written by jig (digest matches no template); left untouched",
				"", nil)
			break
		}
		c.report(name, "bad", "orphan", "shim exists under bin but .ref has no entry (invoking it fails)",
			"orphan shim removed", func() error {
				return os.Remove(c.l.shim(name))
			})

	case !s.HasShim && s.HasRef:
		c.report(name, "bad", "half", ".ref has an entry but bin has no shim (half-written)",
			"half-written entry removed", func() error {
				return os.Remove(c.l.entry(name))
			})

	case s.LinkErr != nil:
		c.report(name, "bad", "not-symlink",
			fmt.Sprintf("entry is not a symlink (%v); read_link at runtime is guaranteed to fail", s.LinkErr),
			"", nil)

	case s.FinalErr != nil:
		c.report(name, "bad", "dangling", "link target does not exist: "+s.LinkRaw, "", nil)

	case s.Cycle:
		c.report(name, "bad", "cycle", "target is inside the shim directory: "+stripLong(s.Final), "", nil)

	default:
		if s.Relative {
			c.report(name, "warn", "relative",
				"entry is a relative link and depends on the shape of .ref itself: "+s.LinkRaw,
				"rewritten as an absolute link", func() error {
					return c.rewriteLink(name, s.Final)
				})
		}
		switch {
		case s.Subsystem == 0:
			c.report(name, "bad", "not-pe", "target is not a recognisable PE image", "", nil)
		case !s.ShimOK:
			c.report(name, "bad", "template",
				fmt.Sprintf("shim copy differs from the %s template (replaced, or template updated)",
					subsystemName(s.Subsystem)),
				"shim copy rewritten from the template", func() error {
					return writeAtomic(c.l.shim(name), templateBytes(s.Subsystem), 0o755)
				})
		case s.Relative:
			// "relative" was already reported above; do not add a redundant ok.
		default:
			c.report(name, "ok", "ok", "-> "+stripLong(s.Final), "", nil)
		}
	}
}

func (c *checker) rewriteLink(name, target string) error {
	entry := c.l.entry(name)
	tmp := filepath.Join(c.l.ref(), name+".exe.tmp."+randHex(4))
	os.Remove(tmp)
	if err := createSymlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, entry); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func (c *checker) checkTemps() {
	for _, dir := range []string{c.l.bin(), c.l.ref(), c.l.tpl()} {
		for _, n := range listTemps(dir) {
			p := filepath.Join(dir, n)
			c.report(n, "bad", "tmp", "leftover temporary file: "+p, "leftover removed", func() error {
				return os.Remove(p)
			})
		}
	}
}

func (c *checker) checkTemplates() {
	if _, err := os.Stat(c.l.tpl()); err != nil {
		return // a missing templates\ is not a problem: add creates it on demand
	}
	for _, sub := range []uint16{subsystemCUI, subsystemGUI} {
		name, data := embeddedTemplate(sub)
		p := filepath.Join(c.l.tpl(), name)
		if got, err := fileSHA256(p); err == nil && got == sha256Hex(data) {
			continue
		}
		detail := "template does not match the version embedded in this jig (not refreshed after an upgrade)"
		if _, err := os.Stat(p); err != nil {
			detail = "template missing: " + name
		}
		c.report("templates/"+name, "warn", "template-stale", detail,
			"refreshed from the embedded copy", func() error {
				return writeAtomic(p, data, 0o755)
			})
	}
}

func (c *checker) checkUnregistered(st state, only []string) {
	if len(only) > 0 {
		return
	}
	// A file already covered by an entry-level finding must not also collect a
	// second "unregistered" warning; one file, one conclusion.
	seen := map[string]bool{}
	for _, f := range c.items {
		seen[strings.ToLower(f.Name)] = true
	}
	self := selfBaseName()
	for _, n := range listNames(c.l.bin()) {
		if self != "" && strings.EqualFold(n+".exe", self) {
			continue
		}
		if seen[strings.ToLower(n)] {
			continue
		}
		if _, ok := st.lookup(n); !ok {
			c.report(n, "warn", "unregistered",
				"executable under bin is not in the manifest (warned only, never deleted)", "", nil)
		}
	}
}

func printFindings(items []finding) {
	if len(items) == 0 {
		fmt.Println("OK: no entries to check")
		return
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	for _, f := range items {
		fmt.Fprintf(w, "[%s]\t%s\t%s\n", f.Level, f.Name, f.Detail)
	}
	w.Flush()
}
