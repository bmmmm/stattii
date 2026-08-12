// SPDX-License-Identifier: GPL-3.0-or-later

package httpapi

import (
	"net/http/httptest"
	"testing"
)

func TestClientIP(t *testing.T) {
	cases := []struct {
		name    string
		trusted string // ParseTrustedProxies input
		remote  string
		xff     string
		want    string
	}{
		{"no proxies configured: peer wins, XFF ignored",
			"", "203.0.113.7:1234", "10.0.0.1", "203.0.113.7"},
		{"untrusted peer: spoofed XFF ignored",
			"172.16.0.0/12", "203.0.113.7:1234", "198.51.100.9", "203.0.113.7"},
		{"trusted peer: client from XFF",
			"172.16.0.0/12", "172.18.0.2:1234", "198.51.100.9", "198.51.100.9"},
		{"trusted peer, proxy chain: rightmost untrusted hop wins",
			"172.16.0.0/12", "172.18.0.2:1234", "6.6.6.6, 198.51.100.9, 172.18.0.3", "198.51.100.9"},
		{"trusted peer, empty XFF: fall back to peer",
			"172.16.0.0/12", "172.18.0.2:1234", "", "172.18.0.2"},
		{"trusted peer, garbage XFF entry skipped",
			"172.16.0.0/12", "172.18.0.2:1234", "198.51.100.9,  , 172.18.0.3", "198.51.100.9"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			nets, err := ParseTrustedProxies(c.trusted)
			if err != nil {
				t.Fatal(err)
			}
			s := &Server{trustedProxies: nets}
			r := httptest.NewRequest("GET", "/a/x", nil)
			r.RemoteAddr = c.remote
			if c.xff != "" {
				r.Header.Set("X-Forwarded-For", c.xff)
			}
			if got := s.clientIP(r); got != c.want {
				t.Fatalf("clientIP = %q, want %q", got, c.want)
			}
		})
	}
}

func TestParseTrustedProxiesRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"not-an-ip", "10.0.0.0/33", "172.16.0.0/12,nope"} {
		if _, err := ParseTrustedProxies(bad); err == nil {
			t.Fatalf("ParseTrustedProxies(%q) = nil error, want failure", bad)
		}
	}
	nets, err := ParseTrustedProxies("172.16.0.0/12, 100.64.0.3")
	if err != nil {
		t.Fatal(err)
	}
	if len(nets) != 2 {
		t.Fatalf("want 2 networks, got %d", len(nets))
	}
}
