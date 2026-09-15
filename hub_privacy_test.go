package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHubPrivacyPage(t *testing.T) {
	ts := httptest.NewServer(newHubHandler())
	defer ts.Close()

	for _, path := range []string{"/privacy", "/privacy/"} {
		res, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("%s: status %d", path, res.StatusCode)
		}
		if ct := res.Header.Get("Content-Type"); !strings.Contains(ct, "text/html") {
			t.Fatalf("%s: content-type %q", path, ct)
		}
		html := string(body)
		if !strings.Contains(html, `id="delete"`) || !strings.Contains(html, "There is no cloud account") {
			t.Fatalf("%s: missing deletion section", path)
		}
		if !strings.Contains(html, "hub.mentasystems.com") {
			t.Fatalf("%s: missing hub name", path)
		}
	}

	res, err := http.Get(ts.URL + "/privacy/nope")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatalf("nested privacy path: %d", res.StatusCode)
	}
}
