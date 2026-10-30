// Package tlsroots is HTTPS for a card app. The Q2's firmware has no CA bundle, so the app carries
// Mozilla's, as curl publishes it (cacert.pem, MPL-2.0, https://curl.se/docs/caextract.html).
//
// A Q2 whose clock was never set reads a date years in the past, and every certificate would be
// "not yet valid". Until the clock looks right, certificates are checked at the time a plain-HTTP
// Date header gives instead (DateURL), asked at most once a minute.
package tlsroots

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	_ "embed"
	"net"
	"net/http"
	"sync"
	"time"
)

//go:embed cacert.pem
var bundle []byte

// DateURL answers plain HTTP with a Date header.
var DateURL = "http://itunes.apple.com/"

var roots = sync.OnceValue(func() *x509.CertPool {
	p := x509.NewCertPool()
	p.AppendCertsFromPEM(bundle)
	return p
})

var clock struct {
	sync.Mutex
	offset time.Duration
	asked  time.Time
}

// Now is the time certificates are checked at: the system clock once it looks set, else the clock
// corrected by the last Date header heard.
func Now() time.Time {
	now := time.Now()
	if now.Year() >= 2025 {
		return now
	}
	clock.Lock()
	defer clock.Unlock()
	if clock.offset == 0 && now.Sub(clock.asked) > time.Minute || clock.asked.IsZero() {
		clock.asked = now
		c := &http.Client{Timeout: 10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		if resp, err := c.Head(DateURL); err == nil {
			resp.Body.Close()
			if d, err := http.ParseTime(resp.Header.Get("Date")); err == nil {
				clock.offset = time.Until(d)
			}
		}
	}
	return time.Now().Add(clock.offset)
}

// Config verifies servers against the bundle, at Now.
func Config() *tls.Config {
	return &tls.Config{RootCAs: roots(), Time: Now, MinVersion: tls.VersionTLS12}
}

// Transport is an HTTP transport for the Q2: the bundle, IPv4 only (the kernel has no IPv6), and
// timeouts for each step, so a dead Wi-Fi is noticed. There is no total timeout: a client sets one
// when it wants it, and a download watches for stalls itself.
func Transport() *http.Transport {
	d := &net.Dialer{Timeout: 15 * time.Second, KeepAlive: 30 * time.Second}
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			return d.DialContext(ctx, "tcp4", addr)
		},
		TLSClientConfig:       Config(),
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		IdleConnTimeout:       60 * time.Second,
		MaxIdleConnsPerHost:   2,
		ForceAttemptHTTP2:     true,
	}
}
