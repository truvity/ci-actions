package runnerenv

import "testing"

func TestPickPrecedence(t *testing.T) {
	for _, c := range []struct {
		name, runner, caller string
		list                 bool
		want                 string
		src                  Source
		fail                 bool
	}{
		{"runner wins", "https://r.example", "https://c.example", false, "https://r.example", FromRunner, false},
		{"caller when the runner is silent", "", "https://c.example", false, "https://c.example", FromCaller, false},
		{"whitespace-only runner is silent", " \n", "https://c.example", false, "https://c.example", FromCaller, false},
		{"neither", "", "", false, "", Unset, false},
		{"whitespace-only caller is unset", "", " ", false, "", Unset, false},
		{"runner trimmed", "https://r.example\n", "", false, "https://r.example", FromRunner, false},
		{"caller kept verbatim", "", "https://c.example,direct", false, "https://c.example,direct", FromCaller, false},
		{"inner space refused", "a b", "https://c.example", false, "", FromRunner, true},
		{"inner newline refused", "a\nb", "https://c.example", false, "", FromRunner, true},
		{"control refused", "a\x01b", "", true, "", FromRunner, true},
		{"list may hold spaces", "a=b c=d", "x=y", true, "a=b c=d", FromRunner, false},
		{"list may hold newlines", "a=b\nc=d", "", true, "a=b\nc=d", FromRunner, false},
	} {
		got, src, err := Pick("CI_RUNNER_X", c.runner, c.caller, c.list)
		if (err != nil) != c.fail || got != c.want || src != c.src {
			t.Errorf("%s: got (%q, %s, %v), want (%q, %s, fail=%v)", c.name, got, src, err, c.want, c.src, c.fail)
		}
	}
}

func TestRedact(t *testing.T) {
	for in, want := range map[string]string{
		"https://u:p@host.example/x":              "https://host.example/x",
		"https://a.example,https://tok@b.example": "https://a.example,https://b.example",
		"https://host.example:443":                "https://host.example:443",
		"grpcs://host.example:9092":               "grpcs://host.example:9092",
		"direct":                                  "direct",
	} {
		if got := Redact(in); got != want {
			t.Errorf("Redact(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDescribeNamesTheSource(t *testing.T) {
	if got := Describe("goproxy", FromRunner, "https://u:p@h.example", "CI_RUNNER_GOPROXY"); got != "goproxy: https://h.example (from the runner's CI_RUNNER_GOPROXY)" {
		t.Error(got)
	}
	if got := Describe("goproxy", Unset, "", "CI_RUNNER_GOPROXY"); got == "" {
		t.Error("empty")
	}
}

func TestPickStore(t *testing.T) {
	for _, c := range []struct {
		name                   string
		rb, rr, cb, cr, ce, cp string
		want                   Store
		fail                   bool
	}{
		{"caller only", "", "", "devel-bucket", "eu-central-1", "", "", Store{Bucket: "devel-bucket", Region: "eu-central-1", BucketSource: FromCaller, RegionSource: FromCaller}, false},
		{"caller keeps its endpoint", "", "", "r2-bucket", "auto", "https://r2.example", "true", Store{Bucket: "r2-bucket", Region: "auto", Endpoint: "https://r2.example", PathStyle: "true", BucketSource: FromCaller, RegionSource: FromCaller}, false},
		{"runner bucket wins and drops the caller's endpoint", "kernel-bucket", "eu-central-1", "devel-bucket", "eu-west-1", "https://r2.example", "true", Store{Bucket: "kernel-bucket", Region: "eu-central-1", BucketSource: FromRunner, RegionSource: FromRunner}, false},
		{"runner bucket without region takes the caller's region", "kernel-bucket", "", "devel-bucket", "eu-central-1", "", "", Store{Bucket: "kernel-bucket", Region: "eu-central-1", BucketSource: FromRunner, RegionSource: FromCaller}, false},
		{"runner bucket and no caller at all", "kernel-bucket", "eu-central-1", "", "", "", "", Store{Bucket: "kernel-bucket", Region: "eu-central-1", BucketSource: FromRunner, RegionSource: FromRunner}, false},
		{"nothing", "", "", "", "", "", "", Store{BucketSource: Unset, RegionSource: Unset}, false},
		{"malformed runner bucket is refused", "a b", "", "devel-bucket", "", "", "", Store{}, true},
	} {
		got, err := PickStore(c.rb, c.rr, c.cb, c.cr, c.ce, c.cp)
		if (err != nil) != c.fail || (err == nil && got != c.want) {
			t.Errorf("%s: got (%+v, %v), want (%+v, fail=%v)", c.name, got, err, c.want, c.fail)
		}
	}
}
