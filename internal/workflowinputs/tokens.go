// Package workflowinputs holds two checks the fleet workflows of
// truvity/ci-workflows used to carry as inline shell: that the token inputs
// of a caller agree with its token source, and reading an enrolment list out
// of the caller's repositories file.
package workflowinputs

import (
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrInvalid marks a failure whose ::error:: lines are already written.
var ErrInvalid = errors.New("workflow inputs: invalid")

// ExtraKey is a second App whose client id and private key must come as a
// pair, checked only with token-source app-key.
type ExtraKey struct {
	IDInput string // the input that names the client id, e.g. approver-client-id
	Secret  string // the secret that holds the private key
	HasKey  bool
	ID      string
}

// Tokens is the caller's token inputs.
type Tokens struct {
	Source string // app-key or access-roster
	Issuer string // access-roster-issuer
	App    string // github-app
	ID     string // client-id or app-id
	IDName string // which input ID is, for the message ("client-id" by default)
	Secret string // the secret that holds the App key
	HasKey bool
	Extras []ExtraKey

	// WarnAppKey is printed as a warning, with token-source app-key, when
	// WarnAppKeyIf is not empty; WarnRoster likewise with access-roster.
	WarnAppKeyIf string
	WarnAppKey   string
	WarnRosterIf string
	WarnRoster   string
}

// ParseExtras reads one ExtraKey per line: `id-input|SECRET|has|id`. The id is
// last because it is the only free-form field.
func ParseExtras(s string) ([]ExtraKey, error) {
	var out []ExtraKey
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.SplitN(line, "|", 4)
		if len(f) != 4 {
			return nil, fmt.Errorf("extra-keys line %q is not id-input|SECRET|has-key|id", line)
		}
		out = append(out, ExtraKey{IDInput: f[0], Secret: f[1], HasKey: f[2] == "true", ID: f[3]})
	}
	return out, nil
}

// CheckTokens prints one ::error:: per fault (all of them, not the first) and
// returns ErrInvalid when there was any.
func CheckTokens(t Tokens, out io.Writer) error {
	if t.IDName == "" {
		t.IDName = "client-id"
	}
	fail := false
	bad := func(format string, a ...any) {
		fmt.Fprintf(out, "::error::"+format+"\n", a...)
		fail = true
	}
	switch t.Source {
	case "app-key":
		if t.ID == "" {
			bad("token-source app-key needs %s", t.IDName)
		}
		if !t.HasKey {
			bad("token-source app-key needs the %s secret", t.Secret)
		}
		for _, e := range t.Extras {
			if e.ID != "" && !e.HasKey {
				bad("%s is set but %s is not", e.IDInput, e.Secret)
			}
		}
		if t.WarnAppKeyIf != "" {
			fmt.Fprintf(out, "::warning::%s\n", t.WarnAppKey)
		}
	case "access-roster":
		if t.Issuer == "" {
			bad("token-source access-roster needs access-roster-issuer")
		}
		if t.App == "" {
			bad("token-source access-roster needs github-app")
		}
		if t.WarnRosterIf != "" {
			fmt.Fprintf(out, "::warning::%s\n", t.WarnRoster)
		}
	default:
		bad("token-source must be app-key or access-roster (got '%s')", t.Source)
	}
	if fail {
		return ErrInvalid
	}
	return nil
}
