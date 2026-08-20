package doctor

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
)

// WriteJSON renders the report as indented JSON.
func WriteJSON(w io.Writer, r Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// WriteTable renders the report as a human-readable table followed by a
// one-line summary. Each check is "STATUS  NAME  DETAIL" and a non-OK check
// gets its fix hint on the next line, indented.
func WriteTable(w io.Writer, r Report) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "STATUS\tCHECK\tDETAIL")
	for _, c := range r.Results {
		fmt.Fprintf(tw, "%s\t%s\t%s\n", marker(c.Status), c.Name, c.Detail)
		if c.Fix != "" && c.Status != OK {
			fmt.Fprintf(tw, "\t\t  fix: %s\n", c.Fix)
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	ok, warn, fail := r.Counts()
	_, err := fmt.Fprintf(w, "\n%d ok, %d warning(s), %d failure(s)\n", ok, warn, fail)
	return err
}

// marker is the fixed-width status cell.
func marker(s Status) string {
	switch s {
	case OK:
		return "[ ok ]"
	case Warn:
		return "[warn]"
	default:
		return "[FAIL]"
	}
}
