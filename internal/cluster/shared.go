package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SharedConnect (mode: shared) resolves the caller's own kubeconfig and
// aws.ini, the same files a laptop uses, and proves the cluster identity
// before anything else in the job spends a credential. It runs after
// ForkGuard has already refused a fork pull request.
func SharedConnect(ctx context.Context, o Options) error {
	o.defaults()
	if err := o.required("GITHUB_ENV", "GITHUB_WORKSPACE", "RUNNER_TEMP"); err != nil {
		return err
	}
	kcInput := o.Getenv("KUBECONFIG_INPUT")
	if kcInput == "" {
		return o.fail("kubeconfig is required for mode: shared")
	}
	awsInput := o.Getenv("AWS_CONFIG_FILE_INPUT")
	if awsInput == "" {
		return o.fail("aws-config-file is required for mode: shared")
	}
	ws := o.Getenv("GITHUB_WORKSPACE")
	kubeconfig := ws + "/" + kcInput
	awsConfig := ws + "/" + awsInput
	if st, err := os.Stat(kubeconfig); err != nil || st.IsDir() {
		return o.fail("kubeconfig not found at %s", kubeconfig)
	}
	if st, err := os.Stat(awsConfig); err != nil || st.IsDir() {
		return o.fail("aws-config-file not found at %s", awsConfig)
	}

	if err := appendLine(o.Getenv("GITHUB_ENV"), "KUBECONFIG="+kubeconfig); err != nil {
		return err
	}
	if err := appendLine(o.Getenv("GITHUB_ENV"), "AWS_CONFIG_FILE="+awsConfig); err != nil {
		return err
	}

	expected := o.Getenv("EXPECTED_IDENTITY")
	if expected == "" {
		o.printf("expected-identity is empty — skipping the identity proof\n")
		return nil
	}

	// Three tries, with stderr kept so a real refusal is readable, and stderr
	// to a FILE rather than merged into the capture: the failure this shape
	// caught (a bare "exit status 1" that lost a whole lane to devbox's own
	// banner landing in front of the JSON) has no reason to reappear just
	// because the code moved repositories. This calls kubectl directly, so
	// that particular banner cannot recur here, but the shape (three tries,
	// stderr kept, exit before any build) is kept exactly.
	errFile := filepath.Join(o.Getenv("RUNNER_TEMP"), "whoami.err")
	env := []string{"KUBECONFIG=" + kubeconfig, "AWS_CONFIG_FILE=" + awsConfig}
	var out bytes.Buffer
	for attempt := 1; attempt <= 3; attempt++ {
		out.Reset()
		ef, err := os.Create(errFile)
		if err != nil {
			return err
		}
		err = o.Exec(ctx, Cmd{Env: env, Name: "kubectl", Args: []string{"auth", "whoami", "-o", "json"}, Stdout: &out, Stderr: ef})
		ef.Close()
		if err == nil {
			break
		}
		o.printf("attempt %d:\n", attempt)
		if b, rerr := os.ReadFile(errFile); rerr == nil {
			o.Out.Write(b)
		}
		if attempt == 3 {
			return o.fail("kubectl auth whoami failed three times")
		}
		o.Sleep(10 * time.Second)
	}

	who := whoami(out.String())
	o.printf("cluster identity: %s\n", who)
	if who != expected {
		return o.fail("expected cluster identity %s, got %s", expected, who)
	}
	return nil
}

// whoami is `sed -n '/^{/,$p' | jq -r .status.userInfo.username`: from the
// first line that starts a JSON object, to the end; "null" when the answer
// has no username.
func whoami(out string) string {
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "{") {
			out = strings.Join(lines[i:], "\n")
			break
		}
	}
	var doc struct {
		Status struct {
			UserInfo struct {
				Username *string `json:"username"`
			} `json:"userInfo"`
		} `json:"status"`
	}
	dec := json.NewDecoder(strings.NewReader(out))
	if dec.Decode(&doc) != nil || doc.Status.UserInfo.Username == nil {
		return "null"
	}
	return *doc.Status.UserInfo.Username
}

// SharedFinish (mode: shared, last step) emits the same five things mode:
// kind emits, now that identity is proved and ECR (if asked for) is logged
// into.
func SharedFinish(o Options) error {
	o.defaults()
	if err := o.required("GITHUB_ENV", "GITHUB_OUTPUT"); err != nil {
		return err
	}
	if o.Getenv("KUBECONFIG") == "" {
		fmt.Fprintln(o.Err, "KUBECONFIG: shared-connect.sh should have set this")
		return &ExitError{Code: 1}
	}
	namespace := orDefault(o.Getenv("NAMESPACE"), "e2e")
	release := o.release()

	// amazon-ecr-login's own `registry` output: a comma-delimited list when
	// `ecr-registries` named more than one account. "The ECR registry host"
	// is singular, matching how SNAPSHOT_REGISTRY is used everywhere else
	// (one push target), so the first one wins. Empty when ecr-registries was
	// empty: this mode's caller then has no SNAPSHOT_REGISTRY, the same as if
	// it never ran a login step at all.
	registry, _, _ := strings.Cut(o.Getenv("ECR_REGISTRY"), ",")

	for _, e := range [][3]string{
		{"KUBECONFIG", o.Getenv("KUBECONFIG"), "kubeconfig"},
		{"SNAPSHOT_REGISTRY", registry, "snapshot-registry"},
		// "shared", not unset: gemaal's harness.DetectTier reads GEMAAL_TIER
		// "when set (any value; TierKind is the only one the harness treats
		// specially today)", so a non-"kind" value falls back to exactly the
		// shared-cluster behaviour every caller already gets from leaving it
		// unset, and a caller of THIS action gets to branch on GEMAAL_TIER
		// without a third "empty means shared" rule to remember.
		{"GEMAAL_TIER", "shared", "gemaal-tier"},
		{"GEMAAL_NAMESPACE", namespace, "gemaal-namespace"},
		{"GEMAAL_RELEASE", release, "gemaal-release"},
	} {
		if err := o.emit(e[0], e[1], e[2]); err != nil {
			return err
		}
	}
	return nil
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// release is the name the caller installs under. The default matches
// truvity/ci-workflows' integration.yaml: `<job>-r<run id>-a<run attempt>`,
// unique per lane and per attempt so parallel jobs and re-runs never collide.
func (o Options) release() string {
	if r := o.Getenv("RELEASE"); r != "" {
		return r
	}
	return orDefault(o.Getenv("GITHUB_JOB"), "e2e") + "-r" + orDefault(o.Getenv("GITHUB_RUN_ID"), "0") + "-a" + orDefault(o.Getenv("GITHUB_RUN_ATTEMPT"), "1")
}
