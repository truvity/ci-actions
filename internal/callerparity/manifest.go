package callerparity

import (
	"os"

	"go.yaml.in/yaml/v3"
)

// manifest is kits/kits.yaml: where each kit lives in a consuming
// repository and how much of it is the shared thing. Most kits are a whole
// file at `.github/workflows/<name>`; the ones that are not (a BLOCK inside
// a lint configuration) say so here. It is not itself a kit.
type manifest struct {
	root *yaml.Node // the top-level mapping, nil when there is no manifest
}

func loadManifest(path string) (*manifest, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return &manifest{}, nil
	}
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, err
	}
	m := &manifest{}
	if len(doc.Content) > 0 && doc.Content[0].Kind == yaml.MappingNode {
		m.root = doc.Content[0]
	}
	return m, nil
}

func mapGet(n *yaml.Node, key string) *yaml.Node {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1]
		}
	}
	return nil
}

func isNull(n *yaml.Node) bool { return n == nil || (n.Kind == yaml.ScalarNode && n.Tag == "!!null") }

// setting is `yq -r '.[kit][field]'`, with the default for an absent
// answer. `// ""` is NOT how this is read, deliberately: yq's alternative
// operator treats `false` as absent, so `enabled: false` would read as unset
// and every kit turned off would be compared anyway, silently. Only null is
// the absent answer.
func (m *manifest) setting(kit, field, def string) string {
	n := mapGet(mapGet(m.root, kit), field)
	if isNull(n) || n.Kind != yaml.ScalarNode {
		return def
	}
	return n.Value
}

// paths is the candidate paths to try, in order. `path:` may be one scalar
// or a list: golangci-lint v2 reads either `.golangci.yaml` or
// `.golangci.yml`, and a repository that wrote the short extension is not
// "absent" for it.
func (m *manifest) paths(kit, def string) []string {
	n := mapGet(mapGet(m.root, kit), "path")
	var out []string
	switch {
	case isNull(n):
	case n.Kind == yaml.ScalarNode:
		out = []string{n.Value}
	case n.Kind == yaml.SequenceNode:
		for _, c := range n.Content {
			if c.Kind == yaml.ScalarNode && !isNull(c) {
				out = append(out, c.Value)
			}
		}
	}
	if len(out) == 0 {
		return []string{def}
	}
	return out
}

// exemptReason is the documented reason a repository is not compared
// against this kit, or "". Keyed on the repository's own name, not
// `owner/name`: kits.yaml is read once for every estate this action runs
// against, and a repository does not change name when it changes owner.
func (m *manifest) exemptReason(kit, name string) string {
	n := mapGet(mapGet(mapGet(m.root, kit), "exempt"), name)
	if isNull(n) || n.Kind != yaml.ScalarNode || n.Value == "false" {
		return ""
	}
	return n.Value
}
