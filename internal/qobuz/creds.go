// Package qobuz is a client for the API Qobuz's own web player uses (www.qobuz.com/api.json/0.2).
// Qobuz publishes no public API: the app id, and the secret that signs file requests, are read from
// the web player's script, as every third-party Qobuz player does. That reading is the fragile part,
// kept here on its own: when Qobuz ships a new player, the patterns below are what to check.
package qobuz

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
)

// Play is the web player; tests point it elsewhere.
var Play = "https://play.qobuz.com"

// Creds are what the web player's script holds: its app id and the candidate secrets, one per
// time zone, of which one signs file requests for the account's region (FindSecret).
type Creds struct {
	Bundle     string            `json:"bundle"` // the script's path, which changes with each release
	AppID      string            `json:"app_id"`
	PrivateKey string            `json:"private_key"` // exchanges a sign-in's code for a token
	Secrets    map[string]string `json:"secrets"`     // time zone: secret
}

var ErrCreds = errors.New("can't read the app id from Qobuz's web player")

var (
	bundleTag  = regexp.MustCompile(`<script[^>]+src="(/resources/[^"]+/bundle\.js)"`)
	appIDField = regexp.MustCompile(`production:\{api:\{appId:"(\d{9,})"`)
	privateKey = regexp.MustCompile(`authenticate\(\{privateKey:"(\w+)"`)
	seedCall   = regexp.MustCompile(`[a-z]\.initialSeed\("([\w=]+)",window\.utimezone\.([a-z]+)\)`)
)

func fetchText(ctx context.Context, c *http.Client, url string, max int64) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (Q2 Pod)")
	resp, err := c.Do(req)
	if err != nil {
		return "", bare(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max))
	return string(b), err
}

// FetchCreds reads the login page for the script it loads, and the script for the app id and
// secrets. cached is kept while the page still loads the same script.
func FetchCreds(ctx context.Context, c *http.Client, cached *Creds) (*Creds, error) {
	page, err := fetchText(ctx, c, Play+"/login", 1<<20)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCreds, err)
	}
	m := bundleTag.FindStringSubmatch(page)
	if m == nil {
		return nil, fmt.Errorf("%w: the login page names no bundle.js", ErrCreds)
	}
	if cached != nil && cached.Bundle == m[1] {
		return cached, nil
	}
	script, err := fetchText(ctx, c, Play+m[1], 32<<20)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCreds, err)
	}
	creds, err := ParseBundle(script)
	if err != nil {
		return nil, err
	}
	creds.Bundle = m[1]
	return creds, nil
}

// ParseBundle finds the app id and the secrets in the web player's script. Each secret is spread
// over three strings: a seed passed to initialSeed with its time zone's name, and that zone's info
// and extras. Joined, less their last 44 characters, they are the secret in URL-safe base64.
func ParseBundle(script string) (*Creds, error) {
	id := appIDField.FindStringSubmatch(script)
	if id == nil {
		return nil, fmt.Errorf("%w: no production appId in the script", ErrCreds)
	}
	creds := &Creds{AppID: id[1], Secrets: map[string]string{}}
	if k := privateKey.FindStringSubmatch(script); k != nil {
		creds.PrivateKey = k[1]
	}
	for _, s := range seedCall.FindAllStringSubmatch(script, -1) {
		seed, zone := s[1], strings.ToUpper(s[2][:1])+s[2][1:]
		zoneInfo := regexp.MustCompile(`name:"\w+/(` + regexp.QuoteMeta(zone) + `[a-z]?)",info:"([\w=]+)",extras:"([\w=]+)"`)
		for _, z := range zoneInfo.FindAllStringSubmatch(script, -1) {
			joined := seed + z[2] + z[3]
			if len(joined) <= 44 {
				continue
			}
			if secret, ok := decode(joined[:len(joined)-44]); ok {
				creds.Secrets[z[1]] = secret
			}
		}
	}
	if len(creds.Secrets) == 0 {
		return nil, fmt.Errorf("%w: no secrets in the script", ErrCreds)
	}
	return creds, nil
}

// decode reads base64 in any of its padded or unpadded, standard or URL-safe forms, as printable
// text.
func decode(s string) (string, bool) {
	for _, enc := range []*base64.Encoding{base64.URLEncoding, base64.RawURLEncoding, base64.StdEncoding, base64.RawStdEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) > 0 {
			for _, c := range b {
				if c < 0x20 || c > 0x7e {
					return "", false
				}
			}
			return string(b), true
		}
	}
	return "", false
}
