package workflowinputs

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"
)

var dottedPath = regexp.MustCompile(`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$`)

// Enrolment reads the list at the dotted path list inside the YAML file and
// returns it as compact JSON, the form `yq -o=json -I=0` printed. A missing
// or null path is an empty list, which is an error: an enrolment that names
// nothing would silently process nothing.
//
// Writes the `names=<json>` line to output (GITHUB_OUTPUT) when it is set, and
// the `<list>: a, b` line to out.
func Enrolment(file, list, output string, out io.Writer) error {
	fail := func(format string, a ...any) error {
		fmt.Fprintf(out, "::error::"+format+"\n", a...)
		return ErrInvalid
	}
	b, err := os.ReadFile(file)
	if err != nil {
		if os.IsNotExist(err) {
			return fail("%s not found in the caller repository", file)
		}
		return err
	}
	if !dottedPath.MatchString(list) {
		return fail("%s is not a dotted path (a.b.c)", list)
	}
	var doc any
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return fail("%s is not valid YAML: %v", file, err)
	}
	cur := doc
	for _, k := range strings.Split(list, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			cur = nil
			break
		}
		cur = m[k]
	}
	items, ok := cur.([]any)
	if cur != nil && !ok {
		return fail("%s holds %T under %s, not a list", file, cur, list)
	}
	if len(items) == 0 {
		return fail("%s has no entries under %s", file, list)
	}
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(items); err != nil {
		return fail("%s under %s cannot be written as JSON: %v", file, list, err)
	}
	names := strings.TrimSuffix(buf.String(), "\n")
	if output != "" {
		f, err := os.OpenFile(output, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
		if err != nil {
			return err
		}
		fmt.Fprintf(f, "names=%s\n", names)
		if err := f.Close(); err != nil {
			return err
		}
	}
	strs := make([]string, len(items))
	for i, it := range items {
		strs[i] = fmt.Sprint(it)
	}
	fmt.Fprintf(out, "%s: %s\n", list, strings.Join(strs, ", "))
	return nil
}
