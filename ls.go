package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"text/tabwriter"
)

type lsRow struct {
	Name      string `json:"name"`
	Subsystem string `json:"subsystem"`
	Target    string `json:"target"`
	State     string `json:"state"`
}

func cmdLs(l layout, args []string) error {
	asJSON := false
	for _, a := range args {
		switch a {
		case "--json":
			asJSON = true
		default:
			return usageErr("unknown option %s", a)
		}
	}

	st, _, err := l.loadState()
	if err != nil {
		return err
	}
	names := st.names()
	sort.Slice(names, func(i, j int) bool { return strings.ToLower(names[i]) < strings.ToLower(names[j]) })

	rows := make([]lsRow, 0, len(names))
	for _, n := range names {
		s := probeName(l, n)
		row := lsRow{Name: s.Name, Subsystem: "-", Target: "-", State: "ok"}
		switch {
		case s.HasRef && s.LinkErr != nil:
			row.Target = "(not a symlink)"
			row.State = "bad-link"
		case s.HasRef && s.FinalErr != nil:
			row.Target = s.LinkRaw
			row.State = "dangling"
		case s.HasRef:
			row.Target = stripLong(s.Final)
			if s.Subsystem != 0 {
				row.Subsystem = subsystemName(s.Subsystem)
			}
		}
		if !s.HasShim {
			row.State = "missing-shim"
		}
		if !s.HasRef {
			row.State = "missing-ref"
		}
		rows = append(rows, row)
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rows)
	}
	if len(rows) == 0 {
		fmt.Println("(empty)")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "NAME\tSUBSYS\tSTATE\tTARGET")
	for _, r := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.Name, r.Subsystem, r.State, r.Target)
	}
	return w.Flush()
}
