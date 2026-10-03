package workflowinputs

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCheckTokens(t *testing.T) {
	extras := []ExtraKey{
		{IDInput: "approver-client-id", Secret: "APPROVER_APP_PRIVATE_KEY", ID: "x", HasKey: false},
		{IDInput: "go-modules-client-id", Secret: "GO_MODULES_APP_PRIVATE_KEY", ID: "y", HasKey: true},
	}
	for _, tc := range []struct {
		name string
		in   Tokens
		out  string
		fail bool
	}{
		{"app-key ok", Tokens{Source: "app-key", ID: "1", HasKey: true, Secret: "RENOVATE_APP_PRIVATE_KEY"}, "", false},
		{"app-key reports both faults", Tokens{Source: "app-key", Secret: "RENOVATE_APP_PRIVATE_KEY"},
			"::error::token-source app-key needs client-id\n::error::token-source app-key needs the RENOVATE_APP_PRIVATE_KEY secret\n", true},
		{"app-id naming", Tokens{Source: "app-key", IDName: "app-id", HasKey: true, Secret: "CI_AUTOMATION_PRIVATE_KEY"},
			"::error::token-source app-key needs app-id\n", true},
		{"extra key missing", Tokens{Source: "app-key", ID: "1", HasKey: true, Secret: "K", Extras: extras},
			"::error::approver-client-id is set but APPROVER_APP_PRIVATE_KEY is not\n", true},
		{"extra without id is fine", Tokens{Source: "app-key", ID: "1", HasKey: true, Secret: "K", Extras: []ExtraKey{{IDInput: "a", Secret: "S"}}}, "", false},
		{"app-key warning", Tokens{Source: "app-key", ID: "1", HasKey: true, Secret: "K", WarnAppKeyIf: "x", WarnAppKey: "ignored"},
			"::warning::ignored\n", false},
		{"warning and error", Tokens{Source: "app-key", Secret: "K", HasKey: true, WarnAppKeyIf: "x", WarnAppKey: "ignored"},
			"::error::token-source app-key needs client-id\n::warning::ignored\n", true},
		{"roster ok", Tokens{Source: "access-roster", Issuer: "i", App: "a"}, "", false},
		{"roster faults", Tokens{Source: "access-roster"},
			"::error::token-source access-roster needs access-roster-issuer\n::error::token-source access-roster needs github-app\n", true},
		{"roster warning", Tokens{Source: "access-roster", Issuer: "i", App: "a", WarnRosterIf: "x", WarnRoster: "w"}, "::warning::w\n", false},
		{"unknown source", Tokens{Source: "pat"}, "::error::token-source must be app-key or access-roster (got 'pat')\n", true},
		{"empty source", Tokens{}, "::error::token-source must be app-key or access-roster (got '')\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var b strings.Builder
			err := CheckTokens(tc.in, &b)
			if b.String() != tc.out {
				t.Errorf("output %q, want %q", b.String(), tc.out)
			}
			if tc.fail != errors.Is(err, ErrInvalid) || (!tc.fail && err != nil) {
				t.Errorf("err = %v, fail want %v", err, tc.fail)
			}
		})
	}
}

func TestParseExtras(t *testing.T) {
	got, err := ParseExtras("a-id|A_KEY|true|1\n\n b|B_KEY|false| \nc|C|true|x|y\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0] != (ExtraKey{"a-id", "A_KEY", true, "1"}) || got[1].HasKey || got[2].ID != "x|y" {
		t.Errorf("got %+v", got)
	}
	if _, err := ParseExtras("only|three|fields"); err == nil {
		t.Error("a short line must be refused")
	}
}

func TestEnrolment(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "fleet.yaml")
	body := "renovate:\n  public: [a, b]\n  private:\n    - c\n    - \"d&e\"\npkl:\n  public: []\nnull_list:\nscalar: x\nmap:\n  k: v\n"
	if err := os.WriteFile(file, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, file, list string
		out, names       string
		fail             bool
	}{
		{"flow list", file, "renovate.public", "renovate.public: a, b\n", `["a","b"]`, false},
		{"block list, no HTML escaping", file, "renovate.private", "renovate.private: c, d&e\n", `["c","d&e"]`, false},
		{"empty", file, "pkl.public", "::error::" + file + " has no entries under pkl.public\n", "", true},
		{"missing path", file, "nope.x", "::error::" + file + " has no entries under nope.x\n", "", true},
		{"null", file, "null_list", "::error::" + file + " has no entries under null_list\n", "", true},
		{"not a list", file, "scalar", "::error::" + file + " holds string under scalar, not a list\n", "", true},
		{"a map is not a list", file, "map", "::error::" + file + " holds map[string]interface {} under map, not a list\n", "", true},
		{"no file", filepath.Join(dir, "gone.yaml"), "a", "::error::" + filepath.Join(dir, "gone.yaml") + " not found in the caller repository\n", "", true},
		{"not a dotted path", file, "a[]", "::error::a[] is not a dotted path (a.b.c)\n", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outFile := filepath.Join(t.TempDir(), "out")
			var b strings.Builder
			err := Enrolment(tc.file, tc.list, outFile, &b)
			if b.String() != tc.out {
				t.Errorf("output %q, want %q", b.String(), tc.out)
			}
			if tc.fail != errors.Is(err, ErrInvalid) || (!tc.fail && err != nil) {
				t.Errorf("err = %v, fail want %v", err, tc.fail)
			}
			got, _ := os.ReadFile(outFile)
			want := ""
			if tc.names != "" {
				want = "names=" + tc.names + "\n"
			}
			if string(got) != want {
				t.Errorf("GITHUB_OUTPUT %q, want %q", got, want)
			}
		})
	}
}
