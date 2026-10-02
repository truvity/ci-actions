package fleet

import (
	"reflect"
	"testing"
)

const sha1 = "3bc59e38c759738677ed02ea2faa216377559fa3"

func TestParseUses(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want []Use
	}{
		{
			name: "step with a version comment",
			in:   "      - uses: truvity/ci-actions/setup-devbox@" + sha1 + " # v1.6.1\n",
			want: []Use{{Owner: "truvity", Repo: "ci-actions", Path: "setup-devbox", Ref: sha1, Hint: "v1.6.1", Line: 1}},
		},
		{
			name: "job-level reusable workflow, no dash",
			in:   "    uses: truvity/ci-workflows/.github/workflows/check.yaml@" + sha1 + "\n",
			want: []Use{{Owner: "truvity", Repo: "ci-workflows", Path: ".github/workflows/check.yaml", Ref: sha1, Line: 1}},
		},
		{
			name: "quoted values",
			in:   "- uses: 'truvity/ci-actions/recipe@" + sha1 + "'\n- uses: \"actions/checkout@v4\"\n",
			want: []Use{
				{Owner: "truvity", Repo: "ci-actions", Path: "recipe", Ref: sha1, Line: 1},
				{Owner: "actions", Repo: "checkout", Ref: "v4", Line: 2},
			},
		},
		{
			name: "a commented-out uses does not count",
			in:   "      # - uses: truvity/ci-actions/setup-devbox@" + sha1 + "\n",
			want: nil,
		},
		{
			name: "local and docker references",
			in:   "- uses: ./setup-devbox\n- uses: docker://alpine:3\n",
			want: []Use{{Local: "./setup-devbox", Line: 1}},
		},
		{
			name: "a repo-root action has no path",
			in:   "- uses: truvity/access-roster@" + sha1 + " # v1.39.1\n",
			want: []Use{{Owner: "truvity", Repo: "access-roster", Ref: sha1, Hint: "v1.39.1", Line: 1}},
		},
		{
			name: "CRLF line endings",
			in:   "- uses: truvity/ci-actions/recipe@" + sha1 + "\r\n",
			want: []Use{{Owner: "truvity", Repo: "ci-actions", Path: "recipe", Ref: sha1, Line: 1}},
		},
		{
			name: "prose mentioning uses: is not a reference",
			in:   "description: this action uses: nothing\nname: x\n",
			want: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ParseUses(tc.in); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got  %+v\nwant %+v", got, tc.want)
			}
		})
	}
}
