package main

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"time"
)

// record is one line of shim.db. The file is append-only and event-driven:
// add / rm mutate state, snapshot replaces it wholesale (after gc compacts,
// only that one line remains).
type record struct {
	V         int      `json:"v"`
	Op        string   `json:"op"` // add | rm | snapshot
	Name      string   `json:"name,omitempty"`
	Target    string   `json:"target,omitempty"`
	Link      string   `json:"link,omitempty"`
	Subsystem uint16   `json:"subsystem,omitempty"`
	SHA256    string   `json:"sha256,omitempty"`
	TS        string   `json:"ts"`
	Entries   []record `json:"entries,omitempty"`
}

// state is keyed by lower-cased name: the Windows filesystem is
// case-insensitive, so keying by the literal spelling would let Cargo and cargo
// coexist.
type state map[string]record

func newRecord(op, name string) record {
	return record{V: 1, Op: op, Name: name, TS: time.Now().Format(time.RFC3339)}
}

// loadState returns the live entries and the number of event lines read.
func (l layout) loadState() (state, int, error) {
	st := state{}
	f, err := os.Open(l.db())
	if err != nil {
		if os.IsNotExist(err) {
			return st, 0, nil
		}
		return nil, 0, fail(exitPartial, "cannot read %s: %v", l.db(), err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	events, line := 0, 0
	for sc.Scan() {
		line++
		text := strings.TrimSpace(sc.Text())
		if text == "" {
			continue
		}
		var r record
		if err := json.Unmarshal([]byte(text), &r); err != nil {
			return nil, 0, fail(exitPartial, "cannot parse line %d of %s: %v", line, l.db(), err)
		}
		events++
		switch r.Op {
		case "snapshot":
			st = state{}
			for _, e := range r.Entries {
				st[strings.ToLower(e.Name)] = e
			}
		case "add":
			st[strings.ToLower(r.Name)] = r
		case "rm":
			delete(st, strings.ToLower(r.Name))
		default:
			return nil, 0, fail(exitPartial, "unknown op %q on line %d of %s", r.Op, line, l.db())
		}
	}
	if err := sc.Err(); err != nil {
		return nil, 0, err
	}
	return st, events, nil
}

func (l layout) appendRecord(r record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(l.db(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fail(exitPartial, "cannot append to %s: %v", l.db(), err)
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return fail(exitPartial, "cannot write %s: %v", l.db(), err)
	}
	return f.Sync()
}

// compact collapses the current state into a single snapshot line and replaces
// the whole file atomically.
func (l layout) compact(st state) error {
	snap := newRecord("snapshot", "")
	for _, r := range st {
		snap.Entries = append(snap.Entries, r)
	}
	if snap.Entries == nil {
		snap.Entries = []record{}
	}
	b, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	if err := writeAtomic(l.db(), append(b, '\n'), 0o644); err != nil {
		return fail(exitPartial, "cannot compact the manifest: %v", err)
	}
	return nil
}

// lookup finds a live entry by name, case-insensitively.
func (st state) lookup(name string) (record, bool) {
	r, ok := st[strings.ToLower(name)]
	return r, ok
}

func (st state) names() []string {
	out := make([]string, 0, len(st))
	for _, r := range st {
		out = append(out, r.Name)
	}
	return out
}
