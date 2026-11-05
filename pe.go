package main

import (
	"encoding/binary"
	"errors"
	"io"
	"os"
)

// Only four fields of the PE header matter here; the first 4 KiB covers any
// realistic image.
const peProbe = 4096

var errNotPE = errors.New("not a valid PE image")

type peHeader struct {
	Machine   uint16
	Subsystem uint16
}

var machineNames = map[uint16]string{
	0x014c: "x86",
	0x8664: "x64",
	0xaa64: "arm64",
}

func readPE(path string) (peHeader, error) {
	f, err := os.Open(path)
	if err != nil {
		return peHeader{}, err
	}
	defer f.Close()

	buf := make([]byte, peProbe)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) && !errors.Is(err, io.EOF) {
		return peHeader{}, err
	}
	return parsePE(buf[:n])
}

// parsePE looks at exactly three places: the MZ signature, the PE\0\0 signature
// at e_lfanew, and Machine / Subsystem in the optional header.
// Offsets match wshim-probe: Subsystem sits at e_lfanew + 24 + 0x44.
func parsePE(b []byte) (peHeader, error) {
	if len(b) < 0x40 || b[0] != 'M' || b[1] != 'Z' {
		return peHeader{}, errNotPE
	}
	lf := int(binary.LittleEndian.Uint32(b[0x3c:]))
	if lf <= 0 || lf+24+0x44+2 > len(b) {
		if lf > 0 && lf+24+0x46 <= peProbe {
			return peHeader{}, errors.New("PE header truncated")
		}
		return peHeader{}, errors.New("PE header offset outside the read window")
	}
	if b[lf] != 'P' || b[lf+1] != 'E' || b[lf+2] != 0 || b[lf+3] != 0 {
		return peHeader{}, errNotPE
	}
	return peHeader{
		Machine:   binary.LittleEndian.Uint16(b[lf+4:]),
		Subsystem: binary.LittleEndian.Uint16(b[lf+24+0x44:]),
	}, nil
}

func subsystemName(s uint16) string {
	switch s {
	case subsystemGUI:
		return "gui"
	case subsystemCUI:
		return "console"
	default:
		return "unknown"
	}
}
