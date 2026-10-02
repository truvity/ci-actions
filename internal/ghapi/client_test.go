package ghapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const secret = "ghs_secret_value"

func TestRawKeepsStatusAndBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/nf":
			w.WriteHeader(404)
			fmt.Fprint(w, `{"message":"Branch not protected"}`)
		case "/post":
			b, _ := io.ReadAll(r.Body)
			if r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" {
				t.Errorf("%s %q", r.Method, r.Header.Get("Content-Type"))
			}
			fmt.Fprintf(w, "echo:%s", b)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	c := New(srv.URL+"/", secret)
	st, body, err := c.Raw(context.Background(), "GET", c.BaseURL+"/nf", nil)
	if err != nil || st != 404 || !strings.Contains(string(body), "Branch not protected") {
		t.Errorf("a 4xx is an answer, not an error: %d %q %v", st, body, err)
	}
	st, body, _ = c.Raw(context.Background(), "POST", c.BaseURL+"/post", []byte(`{"a":1}`))
	if st != 200 || string(body) != `echo:{"a":1}` {
		t.Errorf("post: %d %q", st, body)
	}
	_, _, err = New("http://127.0.0.1:1/"+secret, secret).Raw(context.Background(), "GET", "http://127.0.0.1:1/"+secret, nil)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("a transport error never carries the token: %v", err)
	}
	if _, _, err := c.Get(context.Background(), c.BaseURL+"/missing", "application/json"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get maps 404 to ErrNotFound: %v", err)
	}
}
