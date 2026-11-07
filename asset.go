package main

import (
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// The two templates are the unmodified official binaries from the upstream
// release. jig does not build them: reproducing a Rust/MSVC toolchain locally
// only produces a byte stream that nobody else can vouch for, whereas the
// upstream artifacts carry a SLSA provenance statement tying these exact
// digests to a tag, a commit, and a GitHub-hosted workflow run.
//
//	repo     github.com/nekrozis/wshim
//	tag      v0.3.0
//	commit   2a652f9222d9b5d3978a57c5025dd052bf30e98a
//	workflow .github/workflows/release-build.yml @ refs/tags/v0.3.0
//	run      https://github.com/nekrozis/wshim/actions/runs/42358215591
//
// The digests below are pinned in source on purpose: an embedded blob that no
// longer matches them means the build picked up the wrong file, and that must
// fail loudly at startup instead of producing shims that silently forward
// somewhere unexpected.
const (
	upstreamRepo   = "nekrozis/wshim"
	upstreamTag    = "v0.3.0"
	upstreamCommit = "2a652f9222d9b5d3978a57c5025dd052bf30e98a"

	sumCUI = "4b2cb490278bd226a92a2c40eb705464edc3c08ab536a969f24c3cb1899f7ce7"
	sumGUI = "a2943cc4a280512fbdc605ae69cdce5cac10c657d9d056cde522bb19751183c3"
)

//go:embed assets/wshim-c.exe
var templateCUI []byte

//go:embed assets/wshim-w.exe
var templateGUI []byte

const (
	templateCUIName = "wshim-c.exe"
	templateGUIName = "wshim-w.exe"

	subsystemGUI uint16 = 2
	subsystemCUI uint16 = 3
)

// embeddedSums is filled once by verifyEmbedded; every later comparison reuses
// it. Rehashing 300 KB per entry would make a hundred-entry check burn tens of
// milliseconds for nothing.
var embeddedSums = map[uint16]string{}

// verifyEmbedded runs once at startup so that "the wrong file got embedded" is
// caught before anything touches the disk.
func verifyEmbedded() error {
	for _, want := range []uint16{subsystemCUI, subsystemGUI} {
		name, data := embeddedTemplate(want)
		if len(data) == 0 {
			return fmt.Errorf("embedded template %s is empty", name)
		}
		h, err := parsePE(data)
		if err != nil {
			return fmt.Errorf("embedded template %s cannot be parsed: %w", name, err)
		}
		if h.Subsystem != want {
			return fmt.Errorf("embedded template %s has subsystem %d, want %d (files swapped?)",
				name, h.Subsystem, want)
		}
		if h.Machine != 0x8664 {
			return fmt.Errorf("embedded template %s targets %s, which does not match an amd64 manager",
				name, machineNames[h.Machine])
		}
		sum := sha256Hex(data)
		if pin := pinnedSum(want); sum != pin {
			return fmt.Errorf("embedded template %s has sha256 %s, want %s (upstream %s %s)",
				name, sum, pin, upstreamRepo, upstreamTag)
		}
		embeddedSums[want] = sum
	}
	return nil
}

func pinnedSum(subsystem uint16) string {
	if subsystem == subsystemGUI {
		return sumGUI
	}
	return sumCUI
}

func embeddedTemplate(subsystem uint16) (string, []byte) {
	if subsystem == subsystemGUI {
		return templateGUIName, templateGUI
	}
	return templateCUIName, templateCUI
}

// ensureTemplate materialises the embedded bytes under templates\ and leaves
// the file alone when it already matches. It returns the on-disk path plus the
// digest, which shim.db and check later compare against.
func (l layout) ensureTemplate(subsystem uint16) (string, string, error) {
	return l.installTemplate(subsystem, false)
}

func (l layout) installTemplate(subsystem uint16, force bool) (string, string, error) {
	name, data := embeddedTemplate(subsystem)
	path := filepath.Join(l.tpl(), name)
	want := sha256Hex(data)

	if !force {
		if got, err := fileSHA256(path); err == nil && got == want {
			return path, want, nil
		}
	}
	if err := os.MkdirAll(l.tpl(), 0o755); err != nil {
		return "", "", fail(exitPartial, "cannot create template directory: %v", err)
	}
	if err := writeAtomic(path, data, 0o755); err != nil {
		return "", "", fail(exitPartial,
			"cannot write template %s: %v\n  close any running shims and retry; Windows cannot replace an image that is in use", name, err)
	}
	return path, want, nil
}

// placeShim makes bin\<name>.exe hold the bytes of one template.
//
// A hard link is tried first, and anything that goes wrong simply falls back to
// a copy, so the observable result is the same everywhere: an executable whose
// digest equals the pinned template. What the link buys is that N shims occupy
// one 310 KB image instead of N, that add stops writing 310 KB per name, and
// that concurrently running shims map a single shared image section.
//
// It is only safe because templates\ is never written in place. Every write
// goes through writeAtomic, which builds a new file and renames it over - so an
// upgrade detaches the old inode instead of mutating the one the shims still
// point at. Writing 310 KB into the template path directly would rewrite every
// linked shim at once, and corrupt all of them if it failed halfway.
//
// The link is created beside the destination and renamed into place, never
// linked over it: CreateHardLinkW refuses an existing name, and going through a
// rename keeps the replacement atomic the same way a copy does.
//
// Known limits, all of which degrade to a copy rather than an error: a
// filesystem without hard links (exFAT, FAT32) and a bin\ that is a junction
// onto another volume.
//
// NTFS caps a file at 1024 links (measured: the 1024th returns
// ERROR_TOO_MANY_LINKS, 1142). That ceiling is handled here rather than
// tolerated, because beating it needs no new file: rewriting the template at
// the same path leaves a fresh inode holding the same bytes, and the shims that
// were linked to the previous one keep it alive. See the retry below.
func (l layout) placeShim(name string, subsystem uint16) error {
	tpl, _, err := l.ensureTemplate(subsystem)
	if err != nil {
		return err
	}

	if err := l.linkShim(tpl, name); err == nil {
		return nil
	} else if errors.Is(err, errTooManyLinks) {
		// One retry against a re-materialised template buys another 1023 slots
		// at the cost of a single 310 KB copy that is already part of the disk
		// contract. Sharding templates into extra files would reach the same
		// place, but every shard is a new class of file that has to be named,
		// verified, invalidated on upgrade and garbage collected - a bigger
		// source of bugs than the ceiling it removes.
		if fresh, _, ferr := l.installTemplate(subsystem, true); ferr == nil {
			if l.linkShim(fresh, name) == nil {
				return nil
			}
		}
	}
	return writeAtomic(l.shim(name), templateBytes(subsystem), 0o755)
}

// linkShim creates bin\<name>.exe as a hard link to tpl, keeping the
// replacement atomic by renaming a temporary link into place.
func (l layout) linkShim(tpl, name string) error {
	tmp := filepath.Join(l.bin(), name+".exe.tmp."+randHex(4))
	if err := os.Link(tpl, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, l.shim(name)); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// templateSum hashes in memory only, so check works even when templates\ is
// absent.
func templateSum(subsystem uint16) string {
	if s, ok := embeddedSums[subsystem]; ok {
		return s
	}
	_, data := embeddedTemplate(subsystem)
	return sha256Hex(data)
}

func templateBytes(subsystem uint16) []byte {
	_, data := embeddedTemplate(subsystem)
	return data
}

// isShimBytes reports whether a digest belongs to one of the templates. It is
// the only way to tell "a shim jig wrote" from "some other executable that
// happens to sit in bin" - which matters because bin may be a directory shared
// with another package manager.
func isShimBytes(sum string) bool {
	if sum == "" {
		return false
	}
	return sum == templateSum(subsystemCUI) || sum == templateSum(subsystemGUI)
}
