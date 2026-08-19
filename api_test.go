package main

import (
	"net/http"
	"testing"
)

func TestCloakHeadersStripsAndForwards(t *testing.T) {
	in := http.Header{
		"User-Agent":              {"Mozilla/5.0 ... Chrome/148"},
		"Accept":                  {"*/*"},
		"Accept-Language":         {"en-US,en;q=0.9"},
		"Accept-Encoding":         {"gzip, deflate, br"},
		"Priority":                {"u=1, i"},
		"Sec-Ch-Ua":               {`"Chromium";v="148"`},
		"Sec-Ch-Ua-Platform":      {`"Linux"`},
		"Sec-Fetch-Mode":          {"navigate"},
		"Sec-Fetch-Site":          {"none"},
		"Origin":                  {"https://evil.example"},
		"Referer":                 {"https://evil.example/x"},
		"Connection":              {"keep-alive"},
		"Authorization":           {"Bearer AAAA"},
		"Cookie":                  {"ct0=abc; auth_token=def"},
		"Content-Type":            {"application/json"},
		"X-Csrf-Token":            {"abc"},
		"X-Twitter-Active-User":   {"yes"},
		"X-Twitter-Auth-Type":     {"OAuth2Session"},
		"X-Client-Transaction-Id": {"deadbeef"},
	}

	got := cloakHeaders(in, "api.x.com")

	for _, k := range []string{"user-agent", "accept", "accept-language", "accept-encoding", "priority", "sec-ch-ua", "sec-ch-ua-platform", "connection"} {
		if _, ok := got[k]; ok {
			t.Errorf("header %q should have been stripped", k)
		}
	}
	for k, want := range map[string]string{
		"authorization":           "Bearer AAAA",
		"cookie":                  "ct0=abc; auth_token=def",
		"content-type":            "application/json",
		"x-csrf-token":            "abc",
		"x-twitter-active-user":   "yes",
		"x-twitter-auth-type":     "OAuth2Session",
		"x-client-transaction-id": "deadbeef",
	} {
		if len(got[k]) != 1 || got[k][0] != want {
			t.Errorf("header %q = %v, want [%q]", k, got[k], want)
		}
	}
	if got["origin"][0] != "https://x.com" {
		t.Errorf("origin = %v, want https://x.com", got["origin"])
	}
	if got["referer"][0] != "https://x.com/" {
		t.Errorf("referer = %v, want https://x.com/", got["referer"])
	}
	if got["sec-fetch-mode"][0] != "cors" || got["sec-fetch-dest"][0] != "empty" {
		t.Errorf("sec-fetch mode/dest = %v/%v, want cors/empty", got["sec-fetch-mode"], got["sec-fetch-dest"])
	}
}

func TestCloakHeadersSiteProjection(t *testing.T) {
	tests := []struct {
		host     string
		wantSite string
		wantOrig bool
	}{
		{"x.com", "same-origin", false},
		{"api.x.com", "same-site", true},
		{"api.twitter.com", "cross-site", true},
	}
	for _, tt := range tests {
		got := cloakHeaders(http.Header{}, tt.host)
		if got["sec-fetch-site"][0] != tt.wantSite {
			t.Errorf("host %q: sec-fetch-site = %q, want %q", tt.host, got["sec-fetch-site"][0], tt.wantSite)
		}
		if _, hasOrigin := got["origin"]; hasOrigin != tt.wantOrig {
			t.Errorf("host %q: origin present = %v, want %v", tt.host, hasOrigin, tt.wantOrig)
		}
	}
}
