package main

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
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

func TestStatusTagColorByClass(t *testing.T) {
	defer func(prev bool) { logColor = prev }(logColor)

	logColor = false
	for _, code := range []int{200, 301, 404, 500} {
		if got := statusTag(code); got != strconv.Itoa(code) {
			t.Errorf("logColor=false: statusTag(%d) = %q, want plain %d", code, got, code)
		}
	}

	logColor = true
	wantBg := map[int]int{200: 42, 301: 46, 404: 43, 500: 41, 100: 100}
	for code, bg := range wantBg {
		got := statusTag(code)
		if !strings.Contains(got, "\x1b["+strconv.Itoa(bg)+";30m") {
			t.Errorf("statusTag(%d) = %q, want background %d", code, got, bg)
		}
		if !strings.HasSuffix(got, "\x1b[0m") {
			t.Errorf("statusTag(%d) = %q, want ANSI reset", code, got)
		}
	}
}

func TestRoundDur(t *testing.T) {
	cases := map[time.Duration]string{
		312851874 * time.Nanosecond:  "313ms",
		1096022131 * time.Nanosecond: "1.1s",
		1034283548 * time.Nanosecond: "1.03s",
		45200000 * time.Nanosecond:   "45.2ms",
		500 * time.Nanosecond:        "500ns",
	}
	for in, want := range cases {
		if got := roundDur(in).String(); got != want {
			t.Errorf("roundDur(%v) = %q, want %q", in, got, want)
		}
	}
}
