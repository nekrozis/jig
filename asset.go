package main

import (
	_ "embed"
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
//	tag      v0.2.0
//	commit   9635fe2b46b5277aae17a7143cb97d7f64d39a07
//	workflow .github/workflows/release-build.yml @ refs/tags/v0.2.0
//	run      https://github.com/nekrozis/wshim/actions/runs/41659827568
//
// The digests below are pinned in source on purpose: an embedded blob that no
// longer matches them means the build picked up the wrong file, and that must
// fail loudly at startup instead of producing shims that silently forward
// somewhere unexpected.
const (
	upstreamRepo   = "nekrozis/wshim"
	upstreamTag    = "v0.2.0"
	upstreamCommit = "9635fe2b46b5277aae17a7143cb97d7f64d39a07"

	sumCUI = "65027fea0dd526e98301184e070468adf9352d7cd99e0ff0af572a640fe488e3"
	sumGUI = "b3cdb49fbbeba420de0b85a4d121623661ec5b285ef1825d5741950501cf8deb"
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
		return "", "", fail(exitPartial, "cannot write template %s: %v", name, err)
	}
	return path, want, nil
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
