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
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/pbkdf2"

	_ "modernc.org/sqlite"
)

const (
	chromeCookieProfile = "Default"
	chromeSafeStorage   = "Chrome Safe Storage"
	chromeCookieName    = "yht_access_token"
)

type chromeCookie struct {
	name   string
	value  string
	domain string
}

func runBuiltInTokenSource(ctx context.Context, source string) (helperOutput, error) {
	switch source {
	case accessTokenSourceChrome:
		token, err := extractChromeYHTAccessToken(ctx)
		if err != nil {
			return helperOutput{}, err
		}
		return helperOutput{token: token}, nil
	default:
		return helperOutput{}, fmt.Errorf("unknown built-in token source %q", source)
	}
}

func extractChromeYHTAccessToken(ctx context.Context) (string, error) {
	cookies, err := extractChromeCookies(ctx, chromeCookieProfile)
	if err != nil {
		return "", err
	}
	for _, c := range cookies {
		if c.name == chromeCookieName && len(c.value) >= 10 {
			return c.value, nil
		}
	}
	return "", fmt.Errorf("Chrome has no valid %s cookie; log in to ymscloud.yonyoucloud.com in Chrome", chromeCookieName)
}

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

	const q = `SELECT host_key, name, hex(encrypted_value) FROM cookies WHERE host_key LIKE ? OR host_key LIKE ?`
	rows, err := db.QueryContext(ctx, q, "%yyuap%", "%yonyoucloud%")
	if err != nil {
		return nil, fmt.Errorf("query Chrome cookies snapshot: %w", err)
	}
	defer rows.Close()

	var cookies []chromeCookie
	for rows.Next() {
		var domain, name, encryptedHex string
		if err := rows.Scan(&domain, &name, &encryptedHex); err != nil {
			return nil, fmt.Errorf("scan Chrome cookie row: %w", err)
		}
		value, err := decryptChromeCookieValue(encryptedHex, key)
		if err != nil {
			continue
		}
		cookies = append(cookies, chromeCookie{name: name, value: value, domain: domain})
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
