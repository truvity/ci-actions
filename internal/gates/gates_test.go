package gates

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/truvity/ci-actions/internal/ghapi"
)

// The GraphQL fallback addresses the default branch, or a named one.
func TestClassicGraphQLFallback(t *testing.T) {
	var last string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/protection") {
			w.WriteHeader(403)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var q struct {
			Query     string
			Variables map[string]string
		}
		_ = json.Unmarshal(b, &q)
		last = q.Query + " " + q.Variables["b"]
		fmt.Fprint(w, `{"data":{"repository":{"defaultBranchRef":{"refUpdateRule":{"requiredStatusCheckContexts":["a","b"]}}}}}`)
	}))
	defer srv.Close()
	c := ghapi.New(srv.URL, "t")
	if n := Classic(context.Background(), c, "o/r", "main", false); n != 2 || !strings.Contains(last, "defaultBranchRef{refUpdateRule") || strings.Contains(last, "qualifiedName") {
		t.Errorf("default branch: %d %q", n, last)
	}
	if n := Classic(context.Background(), c, "o/r", "release/1", true); n != 2 || !strings.Contains(last, "ref(qualifiedName:$b)") || !strings.HasSuffix(last, "refs/heads/release/1") {
		t.Errorf("named branch: %d %q", n, last)
	}
	if Describe(Unknown) != "could not read" || Describe(0) != "none" || Describe(3) != "3" {
		t.Error("Describe")
	}
}
