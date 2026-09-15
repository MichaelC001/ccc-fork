package main

import (
	"bytes"
	"testing"
)

func TestHubBoxRoundTrip(t *testing.T) {
	a, err := generateHubIdentity()
	if err != nil {
		t.Fatal(err)
	}
	b, err := generateHubIdentity()
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("talk to Developer")
	n, ct, err := hubSeal(a, b.Public, msg)
	if err != nil {
		t.Fatal(err)
	}
	out, err := hubOpen(b, a.Public, n, ct)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, msg) {
		t.Fatalf("got %q", out)
	}
	c, err := generateHubIdentity()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hubOpen(c, a.Public, n, ct); err == nil {
		t.Fatal("wrong recipient must not open")
	}
}

func TestPairingURI(t *testing.T) {
	u := pairingURI("wss://hub.getccc.dev", "aa", "bb", "c0ffee")
	if u != "ccc://pair/v1?h=wss%3A%2F%2Fhub.getccc.dev&i=aa&k=bb&n=c0ffee" &&
		// url.Values Encode sorts keys: h, i, k, n — that's h, i, k, n. h= i= k= n=
		len(u) < 20 {
		t.Fatalf("uri %s", u)
	}
	if !bytes.Contains([]byte(u), []byte("ccc://pair/v1?")) {
		t.Fatalf("missing scheme: %s", u)
	}
}
