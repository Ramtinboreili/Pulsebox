package web

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesEmbeddedDashboard(t *testing.T) {
	srv := httptest.NewServer(Handler())
	defer srv.Close()

	for _, path := range []string{"/", "/index.html", "/app.js", "/style.css"} {
		res, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET %s: status %d", path, res.StatusCode)
		}
		if len(body) == 0 {
			t.Fatalf("GET %s: empty body", path)
		}
	}

	// Sanity: the served HTML must not reference any external origin.
	res, _ := http.Get(srv.URL + "/index.html")
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if strings.Contains(string(body), "http://") || strings.Contains(string(body), "https://") {
		t.Fatal("dashboard HTML references an external URL")
	}
}
