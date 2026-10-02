package fleet

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// WriteTable prints one row per repository that pins a shared library
// (every repository with all=true), then the totals and, when a gate was
// applied, the verdict.
func WriteTable(w io.Writer, rep *Report, all bool) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "REPOSITORY\tCI-WORKFLOWS\tCI-ACTIONS\tSETUP-DEVBOX\tVIA")
	pinning := 0
	for _, r := range rep.Repos {
		has := len(r.Pins) > 0
		if has {
			pinning++
		}
		if !has && !all && r.Error == "" {
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.Repo, dash(strings.Join(r.CIWorkflows, ",")), dash(strings.Join(r.CIActions, ",")), setupCell(r), viaCell(r))
	}
	tw.Flush()
	fmt.Fprintf(w, "\n%d repositories scanned, %d pin the shared CI libraries\n", len(rep.Repos), pinning)
	for _, e := range rep.Errors {
		fmt.Fprintf(w, "ERROR  %s\n", e)
	}
	if rep.MinSetupDevbox != "" {
		if len(rep.Below) == 0 {
			fmt.Fprintf(w, "every repository resolves setup-devbox at or above %s\n", rep.MinSetupDevbox)
		} else {
			fmt.Fprintf(w, "%d repositories resolve setup-devbox below %s:\n", len(rep.Below), rep.MinSetupDevbox)
			for _, b := range rep.Below {
				fmt.Fprintf(w, "  %s\n", b)
			}
		}
	}
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func setupCell(r RepoResult) string {
	if r.Error != "" {
		return "error"
	}
	if r.SetupDevbox == nil {
		return "-"
	}
	cell := r.SetupDevbox.Lowest
	seen := map[string]bool{cell: true}
	var also []string
	for _, p := range r.SetupDevbox.Pins {
		l := pinLabel(p)
		if !seen[l] {
			seen[l] = true
			also = append(also, l)
		}
	}
	if len(also) > 0 {
		cell += " (also " + strings.Join(also, ",") + ")"
	}
	if r.SetupDevbox.BelowMin {
		cell += " *"
	}
	return cell
}

// viaCell says where the LOWEST setup-devbox pin was read from.
func viaCell(r RepoResult) string {
	if r.SetupDevbox == nil {
		return "-"
	}
	for _, p := range r.SetupDevbox.Pins {
		if pinLabel(p) == r.SetupDevbox.Lowest {
			if p.Via == "" {
				return "direct"
			}
			return p.Via
		}
	}
	return "-"
}

// WriteJSON prints the machine-readable report.
func WriteJSON(w io.Writer, rep *Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}
