//go:build darwin

package youzone

import (
	"testing"
	"time"
)

func TestTokenSourceHost(t *testing.T) {
	cases := map[string]struct {
		baseURL string
		want    string
		wantErr bool
	}{
		"plain":         {"https://c2.yonyoucloud.com", "c2.yonyoucloud.com", false},
		"with path":     {"https://ymscloud.yonyoucloud.com/yonbip-ec-link", "ymscloud.yonyoucloud.com", false},
		"with port":     {"https://c2.yonyoucloud.com:8443", "c2.yonyoucloud.com", false},
		"uppercased":    {"https://C2.YonYouCloud.com", "c2.yonyoucloud.com", false},
		"empty":         {"", "", true},
		"relative path": {"/yonbip-ec-link", "", true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := tokenSourceHost(c.baseURL)
			if c.wantErr {
				if err == nil {
					t.Fatalf("tokenSourceHost(%q) error = nil, want failure", c.baseURL)
				}
				return
			}
			if err != nil {
				t.Fatalf("tokenSourceHost(%q) error = %v", c.baseURL, err)
			}
			if got != c.want {
				t.Errorf("tokenSourceHost(%q) = %q, want %q", c.baseURL, got, c.want)
			}
		})
	}
}

func TestCookieHostMatches(t *testing.T) {
	cases := map[string]struct {
		hostKey string
		host    string
		want    bool
	}{
		"exact":                  {"c2.yonyoucloud.com", "c2.yonyoucloud.com", true},
		"domain cookie parent":   {".yonyoucloud.com", "c2.yonyoucloud.com", true},
		"no-dot parent":          {"yonyoucloud.com", "c2.yonyoucloud.com", true},
		"case insensitive":       {".YonYouCloud.com", "C2.yonyoucloud.com", true},
		"different yonyou env":   {"ymscloud.yonyoucloud.com", "c2.yonyoucloud.com", false},
		"unrelated domain":       {".yyuap.com", "c2.yonyoucloud.com", false},
		"suffix-spoof not match": {"yonyoucloud.com", "evilyonyoucloud.com", false},
		"empty host key":         {"", "c2.yonyoucloud.com", false},
		"empty host":             {".yonyoucloud.com", "", false},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := cookieHostMatches(c.hostKey, c.host); got != c.want {
				t.Errorf("cookieHostMatches(%q, %q) = %v, want %v", c.hostKey, c.host, got, c.want)
			}
		})
	}
}

func TestChromeWebkitMicros(t *testing.T) {
	// The Unix epoch sits 11644473600 s after the 1601-01-01 FILETIME epoch.
	if got := chromeWebkitMicros(time.Unix(0, 0)); got != 11644473600*1_000_000 {
		t.Errorf("chromeWebkitMicros(unix epoch) = %d, want %d", got, 11644473600*1_000_000)
	}
	// One second later must advance by exactly 1e6 microseconds.
	base := chromeWebkitMicros(time.Unix(0, 0))
	if got := chromeWebkitMicros(time.Unix(1, 0)); got-base != 1_000_000 {
		t.Errorf("chromeWebkitMicros advanced by %d µs over 1 s, want 1000000", got-base)
	}
}
