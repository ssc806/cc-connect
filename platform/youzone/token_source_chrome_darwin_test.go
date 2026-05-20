//go:build darwin

package youzone

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/hex"
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

// encryptChromeCookieValue is the inverse of decryptChromeCookieValue: it
// builds a hex-encoded v10 encrypted_value blob from plaintext so the decrypt
// path can be exercised without a real Chrome cookie store.
func encryptChromeCookieValue(t *testing.T, plain, key []byte) string {
	t.Helper()
	block, err := aes.NewCipher(key)
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}
	bs := block.BlockSize()
	pad := bs - len(plain)%bs // PKCS#7
	padded := append(append([]byte{}, plain...), bytes.Repeat([]byte{byte(pad)}, pad)...)
	enc := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, bytes.Repeat([]byte{0x20}, bs)).CryptBlocks(enc, padded)
	return hex.EncodeToString(append([]byte("v10"), enc...))
}

func TestDecryptChromeCookieValueStripsDomainHashOnV24(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 16) // AES-128, as chromeSafeStorageKey derives
	domainHash := bytes.Repeat([]byte{0xAB}, chromeDomainHashLen)
	token := []byte("real-yht-access-token-value")

	enc := encryptChromeCookieValue(t, append(domainHash, token...), key)
	got, err := decryptChromeCookieValue(enc, key, true)
	if err != nil {
		t.Fatalf("decryptChromeCookieValue() error = %v", err)
	}
	if got != string(token) {
		t.Errorf("decrypted = %q, want %q", got, token)
	}
}

func TestDecryptChromeCookieValueEmptyV24CookieIsNotAToken(t *testing.T) {
	// A value-less domain-bound cookie decrypts to exactly the 32-byte hash on a
	// v24 DB. Version-aware stripping must return an empty string, not surface
	// the hash digest as a 32-byte "token".
	key := bytes.Repeat([]byte{0x42}, 16)
	domainHash := bytes.Repeat([]byte{0xAB}, chromeDomainHashLen)

	enc := encryptChromeCookieValue(t, domainHash, key)
	got, err := decryptChromeCookieValue(enc, key, true)
	if err != nil {
		t.Fatalf("decryptChromeCookieValue() error = %v", err)
	}
	if got != "" {
		t.Errorf("decrypted = %q (len %d), want empty", got, len(got))
	}
}

func TestDecryptChromeCookieValuePreV24DoesNotStrip(t *testing.T) {
	// On a pre-v24 DB the plaintext is the value itself; nothing is stripped,
	// even when the value happens to be longer than the v24 hash prefix.
	key := bytes.Repeat([]byte{0x42}, 16)
	token := []byte("a-token-value-that-is-clearly-longer-than-32-bytes")

	enc := encryptChromeCookieValue(t, token, key)
	got, err := decryptChromeCookieValue(enc, key, false)
	if err != nil {
		t.Fatalf("decryptChromeCookieValue() error = %v", err)
	}
	if got != string(token) {
		t.Errorf("decrypted = %q, want %q", got, token)
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
