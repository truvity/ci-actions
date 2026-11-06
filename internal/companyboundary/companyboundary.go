// Package companyboundary is the local, offline half of a company
// repository's gate: it refuses what a company repository must never hold.
//
// Two kinds of repository are judged. A "config" repository holds one
// company's authored configuration (and, for objects it owns, its own
// infrastructure code). A "delivery" repository holds that company's release
// pins and values only. Neither may hold:
//
//   - an object of the platform's delivery tooling (the Argo CD and Kargo API
//     groups), because the platform alone decides what is deployed where;
//   - a Namespace, because a namespace is a platform-owned scope;
//   - a Secret that carries data, because a secret value is never committed;
//   - a symlink that leaves the tree, because a checkout is judged as files.
//
// A config repository additionally holds no Go `replace` directive, and no
// requirement on, import of or vendored copy of a package under a prefix the
// caller names, because the platform's internal packages are not an
// interface. These rules are not applied to a delivery repository, which
// holds no Go code to speak of.
//
// A delivery repository additionally holds no infrastructure-as-code project
// (Pulumi.yaml and its stack files) and no tenant tree, and its pins/ and
// values/ directories keep to the envelope the delivery tooling reads:
// pins/<service>/<cluster>.yaml is `pins:` mapping names to string versions
// (the empty string means dormant), values/<service>/<cluster>.yaml is
// `values:`, and the names are safe path segments.
//
// The check is a deny-list, not a path allow-list: runbooks, a Justfile and
// validation tooling are not manifests and pass. It makes no network call,
// executes no repository code, follows no symlink and reads only the files
// it judges. Run it right after checkout, before anything generates files
// into the tree.
//
// A refusal is `::error::company-boundary: <kind>: <path>: <rule>`, one line
// per violation in a stable order, then one `::notice::` line per rule that
// fired saying what the rule is for, and ErrFailed. The rule is a fixed token;
// nothing read from the repository is echoed except its path, which is
// percent-escaped so a file name cannot start a workflow command.
package companyboundary

import (
	"bytes"
	"errors"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// ErrFailed marks a refusal whose ::error:: lines have already been written.
var ErrFailed = errors.New("company-boundary: failed")

// The two kinds of company repository.
const (
	KindConfig   = "config"
	KindDelivery = "delivery"
)

// The rules. A rule is a fixed token, safe to print.
const (
	RuleArgoObject      = "argo-object"
	RuleKargoObject     = "kargo-object"
	RuleNamespace       = "namespace"
	RuleSecretData      = "secret-data"
	RulePulumiProject   = "pulumi-project"
	RuleTenantDir       = "tenant-dir"
	RuleSymlinkEscape   = "symlink-escape"
	RulePinsMalformed   = "pins-malformed"
	RuleValuesMalformed = "values-malformed"
	RuleNameUnsafe      = "name-unsafe"
	RuleEnvelopeLayout  = "envelope-layout"
	RuleGoReplace       = "go-replace"
	RuleForeignImport   = "foreign-import"
	RuleForeignRequire  = "foreign-require"
	RuleForeignVendor   = "foreign-vendor"
	RuleTooLarge        = "manifest-too-large"
)

var errTooLarge = errors.New("too large")

// maxManifest bounds what is parsed: a larger YAML or JSON file is refused
// rather than read, since it cannot be judged.
const maxManifest = 16 << 20

// Options are the action's inputs.
type Options struct {
	Kind string // KindConfig or KindDelivery
	Dir  string // the checkout; empty means the current directory
	// ForbiddenImportPrefixes are Go module path prefixes, separated by
	// commas or whitespace. Empty switches the three foreign-* rules off
	// (the caller decides what is foreign; nothing is built in).
	ForbiddenImportPrefixes string
	Out                     io.Writer

	disabled map[string]bool // tests only: rules switched off
}

type violation struct {
	path string
	rule string
}

type checker struct {
	opts       Options
	root       string
	prefixes   []string
	violations []violation
	files      int
}

func (c *checker) add(rel, rule string) {
	if c.opts.disabled[rule] {
		return
	}
	c.violations = append(c.violations, violation{rel, rule})
}

// hints say, once per rule, what the rule is for. They are static text.
var hints = map[string]string{
	RuleArgoObject:      "an Argo CD or Argo object belongs to the platform",
	RuleKargoObject:     "a Kargo object belongs to the platform",
	RuleNamespace:       "a Namespace is a platform-owned scope",
	RuleSecretData:      "a Secret must carry no data",
	RulePulumiProject:   "a delivery repository holds no infrastructure project",
	RuleTenantDir:       "a delivery repository holds no tenant tree",
	RuleSymlinkEscape:   "a symlink must stay inside the repository",
	RulePinsMalformed:   "a pins file is `pins:` mapping names to string versions",
	RuleValuesMalformed: "a values file is `values:` and nothing else",
	RuleNameUnsafe:      "a service or cluster name is a lowercase DNS label",
	RuleEnvelopeLayout:  "pins/ and values/ hold <service>/<cluster>.yaml regular files only",
	RuleGoReplace:       "a replace directive redirects a module",
	RuleForeignImport:   "this import is under a forbidden prefix",
	RuleForeignRequire:  "this requirement is under a forbidden prefix",
	RuleForeignVendor:   "a copy of a package under a forbidden prefix",
	RuleTooLarge:        "too large to be judged",
}

var (
	dnsLabel = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	pinKey   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
	pinValue = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+:@-]*$`)
	pulumiRe = regexp.MustCompile(`(?i)^pulumi(\.[^/]+)?\.ya?ml$`)
	prefixRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._~-]*(/[A-Za-z0-9._~-]+)*$`)
)

// Run judges the checkout. It returns ErrFailed when it refused something,
// and another error when it could not judge at all (bad inputs, no such
// directory); the second is a usage error, never a verdict.
func Run(opts Options) error {
	if opts.Out == nil {
		opts.Out = io.Discard
	}
	if opts.Kind != KindConfig && opts.Kind != KindDelivery {
		return fmt.Errorf("kind %q is neither %q nor %q", opts.Kind, KindConfig, KindDelivery)
	}
	dir := opts.Dir
	if dir == "" {
		dir = "."
	}
	root, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	st, err := os.Stat(root)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	// Symlinks are judged against the real root.
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	prefixes, err := parsePrefixes(opts.ForbiddenImportPrefixes)
	if err != nil {
		return err
	}
	c := &checker{opts: opts, root: real, prefixes: prefixes}
	if err := c.walk(); err != nil {
		return err
	}
	sort.SliceStable(c.violations, func(i, j int) bool {
		a, b := c.violations[i], c.violations[j]
		if a.path != b.path {
			return a.path < b.path
		}
		return a.rule < b.rule
	})
	if len(c.violations) == 0 {
		fmt.Fprintf(opts.Out, "company-boundary: %s: %d entries judged, nothing refused\n", opts.Kind, c.files)
		return nil
	}
	seen := map[violation]bool{}
	rules := map[string]bool{}
	n := 0
	for _, v := range c.violations {
		if seen[v] {
			continue
		}
		seen[v] = true
		n++
		rules[v.rule] = true
		fmt.Fprintf(opts.Out, "::error::company-boundary: %s: %s: %s\n", opts.Kind, escape(v.path), v.rule)
	}
	// What a rule is for is said once per rule, on a line of its own, so
	// the error line keeps the exact shape a consumer matches.
	names := make([]string, 0, len(rules))
	for r := range rules {
		names = append(names, r)
	}
	sort.Strings(names)
	for _, r := range names {
		fmt.Fprintf(opts.Out, "::notice::company-boundary: %s: %s\n", r, hints[r])
	}
	fmt.Fprintf(opts.Out, "company-boundary: %s: %d refusals\n", opts.Kind, n)
	return ErrFailed
}

func parsePrefixes(s string) ([]string, error) {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' || r == '\r' }) {
		p = strings.TrimSuffix(p, "/")
		if !prefixRe.MatchString(p) || strings.Contains(p, "..") {
			return nil, fmt.Errorf("forbidden import prefix %q is not a module path prefix", p)
		}
		out = append(out, p)
	}
	return out, nil
}

// escape makes a path safe inside a workflow command: the runner reads the
// line, so a newline or a % in a file name must not be able to forge one.
func escape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '%':
			b.WriteString("%25")
		case r == '\r':
			b.WriteString("%0D")
		case r == '\n':
			b.WriteString("%0A")
		case r < 0x20 || r == 0x7f:
			b.WriteString("%" + strings.ToUpper(strconv.FormatInt(int64(r)+0x100, 16)[1:]))
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (c *checker) walk() error {
	delivery := c.opts.Kind == KindDelivery
	return filepath.WalkDir(c.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == c.root {
			return nil
		}
		rel, _ := filepath.Rel(c.root, p)
		rel = filepath.ToSlash(rel)
		if rel == ".git" {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		c.files++
		// pins and values themselves are part of the envelope: a file or a
		// symlink in their place is judged, a real directory is walked.
		inEnvelope := delivery && (rel == "pins" || rel == "values" || strings.HasPrefix(rel, "pins/") || strings.HasPrefix(rel, "values/"))

		if d.Type()&fs.ModeSymlink != 0 {
			c.symlink(p, rel)
			if inEnvelope {
				c.add(rel, RuleEnvelopeLayout)
			}
			return nil
		}
		if d.IsDir() {
			if delivery && rel == "tenant" {
				c.add(rel, RuleTenantDir)
			}
			c.vendorDir(rel)
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		name := d.Name()
		if delivery && pulumiRe.MatchString(name) {
			c.add(rel, RulePulumiProject)
		}
		if inEnvelope {
			c.envelope(p, rel)
		}
		switch ext := strings.ToLower(path.Ext(rel)); {
		case ext == ".yaml" || ext == ".yml" || ext == ".json":
			return c.manifest(p, rel)
		case ext == ".go":
			return c.goSource(p, rel)
		case name == "go.mod" || name == "go.work":
			return c.goMod(p, rel)
		}
		return nil
	})
}

// symlink refuses a link that leaves the tree, judged twice: by its target
// as written (an absolute target, or one that climbs above the root once
// joined to the link's directory) and by where the filesystem resolves it.
func (c *checker) symlink(p, rel string) {
	target, err := os.Readlink(p)
	if err != nil {
		c.add(rel, RuleSymlinkEscape)
		return
	}
	if filepath.IsAbs(target) {
		c.add(rel, RuleSymlinkEscape)
		return
	}
	joined := path.Join(path.Dir(rel), filepath.ToSlash(target))
	if joined == ".." || strings.HasPrefix(joined, "../") {
		c.add(rel, RuleSymlinkEscape)
		return
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		if r, err := filepath.Rel(c.root, resolved); err != nil || r == ".." || strings.HasPrefix(r, "../") {
			c.add(rel, RuleSymlinkEscape)
		}
	}
}

// envelope judges one regular file under pins/ or values/ of a delivery
// repository: its place, its names and its shape.
func (c *checker) envelope(p, rel string) {
	parts := strings.Split(rel, "/")
	if filepath.Base(rel) == ".gitkeep" {
		return
	}
	if len(parts) != 3 || !strings.HasSuffix(parts[2], ".yaml") {
		c.add(rel, RuleEnvelopeLayout)
		return
	}
	if !dnsLabel.MatchString(parts[1]) || !dnsLabel.MatchString(strings.TrimSuffix(parts[2], ".yaml")) {
		c.add(rel, RuleNameUnsafe)
		return
	}
	b, err := readLimited(p)
	pins := parts[0] == "pins"
	bad := RuleValuesMalformed
	if pins {
		bad = RulePinsMalformed
	}
	if err != nil {
		c.add(rel, bad)
		return
	}
	top, ok := singleDocument(b)
	if !ok || top.Kind != yaml.MappingNode || len(top.Content) != 2 || hasAlias(top) {
		c.add(rel, bad)
		return
	}
	key, val := top.Content[0], top.Content[1]
	if key.Kind != yaml.ScalarNode || key.Value != parts[0] || val.Kind != yaml.MappingNode || hasDuplicateKeys(val) {
		c.add(rel, bad)
		return
	}
	if !pins {
		return
	}
	for i := 0; i+1 < len(val.Content); i += 2 {
		k, v := val.Content[i], val.Content[i+1]
		if k.Kind != yaml.ScalarNode || !pinKey.MatchString(k.Value) {
			c.add(rel, bad)
			return
		}
		if v.Kind != yaml.ScalarNode || v.Tag != "!!str" || (v.Value != "" && !pinValue.MatchString(v.Value)) {
			c.add(rel, bad)
			return
		}
	}
}

// singleDocument parses exactly one YAML document and returns its root.
func singleDocument(b []byte) (*yaml.Node, bool) {
	dec := yaml.NewDecoder(bytes.NewReader(b))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil || doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 {
		return nil, false
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, false
	}
	return doc.Content[0], true
}

func hasAlias(n *yaml.Node) bool {
	if n.Kind == yaml.AliasNode || n.Anchor != "" || n.Tag == "!!merge" {
		return true
	}
	for _, c := range n.Content {
		if hasAlias(c) {
			return true
		}
	}
	return false
}

func hasDuplicateKeys(n *yaml.Node) bool {
	if n.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i].Value
			if seen[k] {
				return true
			}
			seen[k] = true
		}
	}
	for _, c := range n.Content {
		if hasDuplicateKeys(c) {
			return true
		}
	}
	return false
}

func readLimited(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxManifest+1))
	if err != nil {
		return nil, err
	}
	if len(b) > maxManifest {
		return nil, errTooLarge
	}
	return b, nil
}

// manifest judges a YAML or JSON file for the objects no company repository
// may hold. A file that is not valid YAML (a template, say) is not a
// manifest the platform could apply, but is still read for the same words,
// so that breaking the syntax is not a way past the rule.
func (c *checker) manifest(p, rel string) error {
	b, err := readLimited(p)
	if err != nil {
		if errors.Is(err, errTooLarge) {
			c.add(rel, RuleTooLarge)
			return nil
		}
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	for {
		var doc any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			c.textual(rel, b)
			return nil
		}
		c.object(rel, doc, 0)
	}
}

// normalize turns every mapping into map[string]any. yaml.v3 decodes a
// mapping with a non-string key (an integer, a bool) into map[any]any; the
// tools that apply a manifest read such a key as its text, so a type
// assertion on map[string]any alone would let that object through.
func normalize(v any, depth int) any {
	if depth > 64 {
		return v
	}
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = normalize(e, depth+1)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[fmt.Sprint(k)] = normalize(e, depth+1)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, e := range x {
			out[i] = normalize(e, depth+1)
		}
		return out
	}
	return v
}

func (c *checker) object(rel string, doc any, depth int) {
	if depth > 8 {
		return
	}
	if list, ok := doc.([]any); ok {
		// A top-level (or nested) list of objects: judge each element.
		for _, it := range list {
			c.object(rel, it, depth+1)
		}
		return
	}
	m, ok := normalize(doc, 0).(map[string]any)
	if !ok {
		return
	}
	api, _ := m["apiVersion"].(string)
	kind, _ := m["kind"].(string)
	group := ""
	if i := strings.Index(api, "/"); i >= 0 {
		group = api[:i]
	}
	switch {
	case group == "argoproj.io" || strings.HasSuffix(group, ".argoproj.io"):
		c.add(rel, RuleArgoObject)
	case group == "kargo.akuity.io":
		c.add(rel, RuleKargoObject)
	case group == "" && api == "v1" && kind == "Namespace":
		c.add(rel, RuleNamespace)
	case group == "" && api == "v1" && kind == "Secret":
		if nonEmpty(m["data"]) || nonEmpty(m["stringData"]) {
			c.add(rel, RuleSecretData)
		}
	}
	if items, ok := m["items"].([]any); ok && strings.HasSuffix(kind, "List") {
		for _, it := range items {
			c.object(rel, it, depth+1)
		}
	}
}

func nonEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case map[string]any:
		return len(x) > 0
	case string:
		return x != ""
	default:
		return true
	}
}

var (
	textArgo   = regexp.MustCompile(`(?m)^\s*"?apiVersion"?\s*:\s*["']?([A-Za-z0-9.-]*\.)?argoproj\.io/`)
	textKargo  = regexp.MustCompile(`(?m)^\s*"?apiVersion"?\s*:\s*["']?kargo\.akuity\.io/`)
	textNS     = regexp.MustCompile(`(?m)^\s*"?kind"?\s*:\s*["']?Namespace\b`)
	textSecret = regexp.MustCompile(`(?m)^\s*"?kind"?\s*:\s*["']?Secret\b`)
	textData   = regexp.MustCompile(`(?m)^\s*"?(data|stringData)"?\s*:\s*\S`)
)

func (c *checker) textual(rel string, b []byte) {
	if textArgo.Match(b) {
		c.add(rel, RuleArgoObject)
	}
	if textKargo.Match(b) {
		c.add(rel, RuleKargoObject)
	}
	if textNS.Match(b) {
		c.add(rel, RuleNamespace)
	}
	if textSecret.Match(b) && textData.Match(b) {
		c.add(rel, RuleSecretData)
	}
}

// under reports whether the import or module path p is the prefix or below it.
func (c *checker) under(p string) bool {
	for _, pre := range c.prefixes {
		if p == pre || strings.HasPrefix(p, pre+"/") {
			return true
		}
	}
	return false
}

var quoted = regexp.MustCompile("\"([^\"\\n]+)\"|`([^`]+)`")

func (c *checker) goSource(p, rel string) error {
	if c.opts.Kind != KindConfig || len(c.prefixes) == 0 {
		return nil
	}
	b, err := readLimited(p)
	if err != nil {
		if errors.Is(err, errTooLarge) {
			c.add(rel, RuleTooLarge)
			return nil
		}
		return err
	}
	f, err := parser.ParseFile(token.NewFileSet(), rel, b, parser.ImportsOnly)
	if err == nil {
		for _, im := range f.Imports {
			if s, err := strconv.Unquote(im.Path.Value); err == nil && c.under(s) {
				c.add(rel, RuleForeignImport)
			}
		}
		return nil
	}
	// Not valid Go: read every quoted string, so a syntax error is not a way past.
	for _, m := range quoted.FindAllSubmatch(b, -1) {
		s := string(m[1]) + string(m[2])
		if c.under(s) {
			c.add(rel, RuleForeignImport)
		}
	}
	return nil
}

// goMod reads a go.mod or go.work: any replace is refused, and (with
// prefixes) so is a requirement under one.
func (c *checker) goMod(p, rel string) error {
	if c.opts.Kind != KindConfig {
		return nil
	}
	b, err := readLimited(p)
	if err != nil {
		if errors.Is(err, errTooLarge) {
			c.add(rel, RuleTooLarge)
			return nil
		}
		return err
	}
	block := ""
	for _, line := range strings.Split(string(b), "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		f := strings.Fields(line)
		if len(f) == 0 {
			continue
		}
		if block != "" {
			if f[0] == ")" {
				block = ""
				continue
			}
			if block == "require" && c.under(f[0]) {
				c.add(rel, RuleForeignRequire)
			}
			continue
		}
		verb := strings.TrimSuffix(f[0], "(")
		switch verb {
		case "replace":
			c.add(rel, RuleGoReplace)
		case "require":
			if len(f) >= 2 && f[1] != "(" && c.under(f[1]) {
				c.add(rel, RuleForeignRequire)
			}
		}
		if strings.HasSuffix(line, "(") || (len(f) >= 2 && f[len(f)-1] == "(") {
			block = verb
		}
	}
	return nil
}

// vendorDir refuses a directory that mirrors a forbidden module path
// (vendor/<prefix>, third_party/<prefix>, a copied tree), once, at its top.
func (c *checker) vendorDir(rel string) {
	if c.opts.Kind != KindConfig {
		return
	}
	for _, pre := range c.prefixes {
		if strings.HasSuffix("/"+rel, "/"+pre) {
			c.add(rel, RuleForeignVendor)
		}
	}
}
