//go:build darwin

package youzone

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/crypto/pbkdf2"

	_ "modernc.org/sqlite"
)

const (
	defaultChromeProfile = "Default"
	chromeSafeStorage    = "Chrome Safe Storage"
	chromeCookieName     = "yht_access_token"
)

type chromeCookie struct {
	value  string
	domain string // Chrome host_key, e.g. ".yonyoucloud.com" or "c2.yonyoucloud.com"
}

func runBuiltInTokenSource(ctx context.Context, source string, cfg tokenSourceConfig) (helperOutput, error) {
	switch source {
	case accessTokenSourceChrome:
		token, err := extractChromeYHTAccessToken(ctx, cfg)
		if err != nil {
			return helperOutput{}, err
		}
		return helperOutput{token: token}, nil
	default:
		return helperOutput{}, fmt.Errorf("unknown built-in token source %q", source)
	}
}

// extractChromeYHTAccessToken returns the freshest unexpired yht_access_token
// cookie Chrome holds for the configured base_url host. Scoping to that host
// keeps a browser logged in to several Yonyou environments from silently
// handing back a token minted for a different one.
func extractChromeYHTAccessToken(ctx context.Context, cfg tokenSourceConfig) (string, error) {
	host, err := tokenSourceHost(cfg.baseURL)
	if err != nil {
		return "", err
	}
	profile := cfg.chromeProfile
	if profile == "" {
		profile = defaultChromeProfile
	}
	cookies, err := extractChromeCookies(ctx, profile)
	if err != nil {
		return "", err
	}
	// cookies arrives ordered freshest-first; the first host match wins.
	for _, c := range cookies {
		if len(c.value) >= 10 && cookieHostMatches(c.domain, host) {
			return c.value, nil
		}
	}
	return "", fmt.Errorf("Chrome profile %q has no valid %s cookie for %s; log in to that host in Chrome (set chrome_profile if you use a non-default profile)",
		profile, chromeCookieName, host)
}

// tokenSourceHost extracts the lower-cased hostname from the configured
// base_url so cookie lookups can be scoped to it.
func tokenSourceHost(baseURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || u.Hostname() == "" {
		return "", fmt.Errorf("Chrome token source: cannot derive host from base_url %q", baseURL)
	}
	return strings.ToLower(u.Hostname()), nil
}

// cookieHostMatches reports whether a cookie stored under hostKey applies to
// host. It accepts an exact match and a parent-domain match — Chrome stores a
// domain cookie's host_key as ".example.com", which covers c2.example.com.
func cookieHostMatches(hostKey, host string) bool {
	hostKey = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(hostKey), "."))
	host = strings.ToLower(strings.TrimSpace(host))
	if hostKey == "" || host == "" {
		return false
	}
	return host == hostKey || strings.HasSuffix(host, "."+hostKey)
}

// chromeWebkitMicros converts a Go time to Chrome's cookie timestamp unit:
// microseconds since 1601-01-01 UTC (the Windows FILETIME epoch).
func chromeWebkitMicros(t time.Time) int64 {
	const webkitEpochOffsetMicros = 11644473600 * 1_000_000
	return t.UnixMicro() + webkitEpochOffsetMicros
}

// extractChromeCookies returns every yht_access_token cookie in the given
// Chrome profile that has not expired, ordered freshest-first (latest expiry,
// then most recently created) so the caller's pick is deterministic.
func extractChromeCookies(ctx context.Context, profile string) ([]chromeCookie, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolve home directory: %w", err)
	}
	cookiesDB := filepath.Join(home, "Library", "Application Support", "Google", "Chrome", profile, "Cookies")
	if _, err := os.Stat(cookiesDB); err != nil {
		return nil, fmt.Errorf("Chrome cookies file not found: %s: %w", cookiesDB, err)
	}

	snapshot, cleanup, err := copyChromeCookieDB(cookiesDB)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	key, err := chromeSafeStorageKey(ctx)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", snapshot)
	if err != nil {
		return nil, fmt.Errorf("open Chrome cookies snapshot: %w", err)
	}
	defer db.Close()

	// Filter by cookie name and drop already-expired rows (session cookies have
	// has_expires=0); order so the latest-expiring, newest cookie comes first.
	const q = `SELECT host_key, hex(encrypted_value) FROM cookies
		WHERE name = ? AND (has_expires = 0 OR expires_utc > ?)
		ORDER BY expires_utc DESC, creation_utc DESC`
	rows, err := db.QueryContext(ctx, q, chromeCookieName, chromeWebkitMicros(time.Now()))
	if err != nil {
		return nil, fmt.Errorf("query Chrome cookies snapshot: %w", err)
	}
	defer rows.Close()

	var cookies []chromeCookie
	for rows.Next() {
		var domain, encryptedHex string
		if err := rows.Scan(&domain, &encryptedHex); err != nil {
			return nil, fmt.Errorf("scan Chrome cookie row: %w", err)
		}
		value, err := decryptChromeCookieValue(encryptedHex, key)
		if err != nil {
			continue
		}
		cookies = append(cookies, chromeCookie{value: value, domain: domain})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate Chrome cookies: %w", err)
	}
	return cookies, nil
}

func copyChromeCookieDB(src string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "cc-connect-chrome-cookies-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("create Chrome cookie snapshot dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	dst := filepath.Join(dir, "Cookies")

	in, err := os.Open(src)
	if err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("open Chrome cookies file: %w", err)
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("create Chrome cookie snapshot: %w", err)
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		cleanup()
		return "", func() {}, fmt.Errorf("copy Chrome cookie snapshot: %w", err)
	}
	if err := out.Close(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("close Chrome cookie snapshot: %w", err)
	}
	return dst, cleanup, nil
}

func chromeSafeStorageKey(ctx context.Context) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "security", "find-generic-password", "-w", "-s", chromeSafeStorage)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("read %s from Keychain: %w", chromeSafeStorage, err)
	}
	password := strings.TrimSpace(string(out))
	if password == "" {
		return nil, fmt.Errorf("read %s from Keychain: empty password", chromeSafeStorage)
	}
	return pbkdf2.Key([]byte(password), []byte("saltysalt"), 1003, 16, sha1.New), nil
}

func decryptChromeCookieValue(encryptedHex string, key []byte) (string, error) {
	raw, err := hex.DecodeString(encryptedHex)
	if err != nil {
		return "", err
	}
	if len(raw) < 3 {
		return "", fmt.Errorf("encrypted cookie value is too short")
	}
	prefix := string(raw[:3])
	if prefix != "v10" && prefix != "v11" {
		return "", fmt.Errorf("unsupported encrypted cookie prefix %q", prefix)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	payload := raw[3:]
	if len(payload) == 0 || len(payload)%block.BlockSize() != 0 {
		return "", fmt.Errorf("encrypted cookie payload has invalid length")
	}
	plain := make([]byte, len(payload))
	cipher.NewCBCDecrypter(block, bytesRepeat(0x20, block.BlockSize())).CryptBlocks(plain, payload)
	plain, err = pkcs7Unpad(plain, block.BlockSize())
	if err != nil {
		return "", err
	}
	if len(plain) > 32 {
		plain = plain[32:]
	}
	return string(plain), nil
}

func bytesRepeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

func pkcs7Unpad(in []byte, blockSize int) ([]byte, error) {
	if len(in) == 0 || len(in)%blockSize != 0 {
		return nil, fmt.Errorf("invalid PKCS#7 payload length")
	}
	pad := int(in[len(in)-1])
	if pad == 0 || pad > blockSize || pad > len(in) {
		return nil, fmt.Errorf("invalid PKCS#7 padding")
	}
	for _, v := range in[len(in)-pad:] {
		if int(v) != pad {
			return nil, fmt.Errorf("invalid PKCS#7 padding")
		}
	}
	return in[:len(in)-pad], nil
}
