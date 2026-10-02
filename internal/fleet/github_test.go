package fleet

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const secret = "ghs_super_secret_token_value"

func TestClientPaginationAndAuth(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		switch {
		case r.URL.Path == "/orgs/acme/repos" && r.URL.Query().Get("page") == "":
			w.Header().Set("Link", fmt.Sprintf(`<%s/orgs/acme/repos?per_page=100&type=all&page=2>; rel="next"`, srv.URL))
			fmt.Fprint(w, `[{"name":"a","full_name":"acme/a","default_branch":"main"}]`)
		case r.URL.Path == "/orgs/acme/repos":
			fmt.Fprint(w, `[{"name":"b","full_name":"acme/b","default_branch":"main","archived":true}]`)
		case r.URL.Path == "/orgs/person/repos":
			http.NotFound(w, r)
		case r.URL.Path == "/users/person/repos":
			fmt.Fprint(w, `[{"name":"c","full_name":"person/c","default_branch":"main"}]`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL, secret)

	repos, err := c.ListRepos(context.Background(), "acme")
	if err != nil || len(repos) != 2 || repos[1].FullName != "acme/b" || !repos[1].Archived {
		t.Fatalf("repos = %+v, err = %v", repos, err)
	}
	repos, err = c.ListRepos(context.Background(), "person")
	if err != nil || len(repos) != 1 || repos[0].FullName != "person/c" {
		t.Fatalf("a user account falls back to /users: %+v, %v", repos, err)
	}
	if files, err := c.ListDir(context.Background(), "acme/a", ".github/workflows", "main"); err != nil || files != nil {
		t.Errorf("a missing directory is empty, not an error: %v, %v", files, err)
	}
	if _, err := c.File(context.Background(), "acme/a", "x.yaml", "main"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v", err)
	}
}

func TestClientErrorsNeverCarryTheToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// a misbehaving proxy that echoes the credential back
		http.Error(w, "bad credentials: "+r.Header.Get("Authorization"), http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, secret)
	_, err := c.File(context.Background(), "acme/a", "x.yaml", "main")
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("the token leaked into an error: %v", err)
	}
	// and a transport error that embeds the URL
	c2 := NewClient("http://127.0.0.1:1/"+secret, secret)
	c2.HTTP.Timeout = 1
	_, err = c2.File(context.Background(), "acme/a", "x.yaml", "main")
	if err != nil && strings.Contains(err.Error(), secret) {
		t.Errorf("the token leaked into a transport error: %v", err)
	}
}

func TestClientTagsAndRateLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/limited/tags") {
			w.Header().Set("X-RateLimit-Remaining", "0")
			w.Header().Set("X-RateLimit-Reset", "1")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		fmt.Fprint(w, `[{"name":"v1.0.0","commit":{"sha":"abc"}}]`)
	}))
	defer srv.Close()
	c := NewClient(srv.URL, secret)
	tags, err := c.Tags(context.Background(), "acme/lib")
	if err != nil || len(tags) != 1 || tags[0] != (Tag{"v1.0.0", "abc"}) {
		t.Fatalf("%+v %v", tags, err)
	}
	if _, err := c.Tags(context.Background(), "acme/limited"); err == nil || !strings.Contains(err.Error(), "rate limit") {
		t.Errorf("err = %v", err)
	}
}

func TestClientTree(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/acme/a/git/trees/main":
			fmt.Fprint(w, `{"tree":[{"path":".github/actions/x/action.yaml","type":"blob"},{"path":".github/actions","type":"tree"}],"truncated":false}`)
		case "/repos/acme/big/git/trees/main":
			fmt.Fprint(w, `{"tree":[],"truncated":true}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL, secret)
	got, err := c.Tree(context.Background(), "acme/a", "main")
	if err != nil || len(got) != 1 || got[0] != ".github/actions/x/action.yaml" {
		t.Errorf("tree = %v, %v", got, err)
	}
	if got, err := c.Tree(context.Background(), "acme/none", "main"); err != nil || got != nil {
		t.Errorf("a missing repository is an empty tree: %v, %v", got, err)
	}
	if _, err := c.Tree(context.Background(), "acme/big", "main"); err == nil {
		t.Error("a truncated tree is an error, not a silent pass")
	}
}
