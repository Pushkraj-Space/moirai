package main

import (
	"fmt"
	"os"
	"os/exec"

	moirai "github.com/october-dev/moirai"
)

func (a app) doctor(args []string) error {
	fs := newFlags("doctor", a.err)
	asJSON := fs.Bool("json", false, "emit JSON")
	if err := parseFlags(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("doctor takes no arguments")
	}
	registry, err := stores()
	if err != nil {
		return err
	}
	type status struct {
		Format       moirai.Format     `json:"format"`
		Capabilities moirai.Capability `json:"capabilities"`
		Executable   string            `json:"executable,omitempty"`
		Installed    bool              `json:"installed"`
		Store        string            `json:"store,omitempty"`
		Readable     bool              `json:"readable"`
		Note         string            `json:"note,omitempty"`
	}
	rows := []status{}
	for _, info := range moirai.DefaultRegistry.Harnesses() {
		row := status{Format: info.Format, Capabilities: info.Capability}
		if command, err := moirai.CommandFor(info.Format, moirai.SessionRef{}); err == nil {
			row.Executable = command.Program
			_, err = exec.LookPath(command.Program)
			row.Installed = err == nil
		}
		if store, err := registry.Store(info.Format); err == nil {
			row.Store = store.Root()
			f, err := os.Open(row.Store)
			if err == nil {
				row.Readable = true
				f.Close()
			} else {
				row.Note = "Store is absent or unreadable; run the harness once or check its configuration."
			}
			if info.Capability.Save && row.Readable {
				row.Note = "Write permission is not probed; use continue --dry-run to preview conversion."
			}
		}
		rows = append(rows, row)
	}
	if *asJSON {
		return writeJSON(a.out, rows)
	}
	for _, row := range rows {
		fmt.Fprintf(a.out, "%-18s installed=%t readable=%t %s\n", row.Format, row.Installed, row.Readable, moirai.ScrubTerminal(row.Store))
		if row.Note != "" {
			fmt.Fprintln(a.out, "  "+row.Note)
		}
	}
	return nil
}
