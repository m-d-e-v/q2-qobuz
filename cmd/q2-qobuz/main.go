// q2-qobuz is a Q2 Pod card app (contrib/shanlingq2/README): Qobuz on the Shanling Q2. Sign in
// from a phone (the Q2 shows a QR code for its page, which sends you to Qobuz's own sign-in and
// back), then browse your favourite albums, artists and songs, your playlists and Qobuz's catalogue
// on the Q2, and download tracks, albums and playlists to the card in up to 24-bit 192 kHz FLAC,
// tagged and with their covers. Q2 Pod's own player plays them. Downloads live in a cache of a set
// size; the oldest go first. It runs from the card's .qobuz folder (copied to /tmp first, so the
// card is never kept busy) and draws the Q2's Qobuz page over Q2SPOT/1.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"q2-qobuz/internal/cache"
	"q2-qobuz/internal/dl"
	"q2-qobuz/internal/q2"
	"q2-qobuz/internal/q2spot"
	"q2-qobuz/internal/qobuz"
	"q2-qobuz/internal/qr"
	"q2-qobuz/internal/tlsroots"
)

const version = "q2-v0.7"

const (
	webHold    = 5 * time.Minute  // the phone's page used this recently keeps the Q2 awake
	listsStale = 30 * time.Minute // a favourites list older than this is fetched again when opened
	probeTrack = "64868955"       // a long-streamable track, to find the signing secret on
)

// settings is .qobuz/config.json.
type settings struct {
	Key           string `json:"web_key"`
	PIN           string `json:"pin"`
	IdleMinutes   int    `json:"idle_minutes"`
	Quality       int    `json:"quality"`        // downloads': qobuz.MP3, CD, HiRes96 or HiRes192
	StreamQuality int    `json:"stream_quality"` // streaming's: CD by default, so a track starts soon
	LimitMB       int    `json:"limit_mb"`       // the cache's size
	GotoLibrary   bool   `json:"goto_library"`   // Go to Album and Artist open the Library's pages, not Qobuz's
}

// session is the sign-in, kept in the Q2's own storage (-private), not on the card.
type session struct {
	Token    string    `json:"token"`
	UserID   int64     `json:"user_id"`
	Name     string    `json:"name"`
	Plan     string    `json:"plan"`
	HiRes    bool      `json:"hires"`
	SignedIn time.Time `json:"signed_in"`
	Secret   string    `json:"secret,omitempty"`
	Bundle   string    `json:"bundle,omitempty"` // the web player whose secret that is
}

type options struct {
	dir, private, statusFile, browse, control string
	playFile, playingFile                     string // stream mode's: the session Q2 Pod plays, and the track it plays
	port                                      int
	ready                                     func() bool
}

type app struct {
	opts   options
	state  *q2.State
	cache  *cache.Cache
	dl     *dl.Downloader
	httpc  *http.Client // the API and covers, with a total timeout
	port   int
	ln     net.Listener
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	set    settings
	sess   *session
	creds  *qobuz.Creds
	used   time.Time
	webAt  time.Time
	ip     string
	qrFor  string
	qrHex  string
	qrSize int
	armed  map[string]time.Time
	nonces map[string]time.Time // sign-ins sent to Qobuz, each one returning once
	lists  map[string]*favs
	loads  map[string]*load
	stream *stream // the streaming session, nil before the first
	covers []cover // the last few albums' covers, newest first
	quit   chan struct{}
	once   sync.Once
	bg     sync.WaitGroup // warm, waited for on the way out
}

func main() {
	var o options
	flag.StringVar(&o.dir, "dir", "/mnt/mmc/.qobuz", "the app's folder on the card")
	flag.StringVar(&o.private, "private", "/mnt/data/q2-qobuz", "where the sign-in is kept: the Q2's own storage")
	flag.StringVar(&o.statusFile, "status-file", "/tmp/q2-qobuz.state", "the status file Q2 Pod reads")
	flag.StringVar(&o.browse, "browse-socket", "/tmp/q2-qobuz.browse.sock", "Q2SPOT/1 for the Q2's page")
	flag.StringVar(&o.control, "control-socket", "/tmp/q2-qobuz.sock", "datagrams: q quits")
	flag.StringVar(&o.playFile, "play-file", "/tmp/q2-qobuz.play", "stream mode: the session Q2 Pod plays")
	flag.StringVar(&o.playingFile, "playing-file", "/tmp/q2-qobuz.playing", "stream mode: the track Q2 Pod plays")
	flag.IntVar(&o.port, "port", 8300, "the phone page's port (the next free one if taken)")
	flag.Parse()
	debug.SetGCPercent(50)
	debug.SetMemoryLimit(48 << 20)
	q2.SetupLog("q2-qobuz")
	a, err := start(o)
	if err != nil {
		log.Fatal(err)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP)
	go func() { <-sig; a.shutdown() }()
	a.run()
	os.Exit(0)
}

func readJSON(path string, v any) bool {
	b, err := os.ReadFile(path)
	return err == nil && json.Unmarshal(b, v) == nil
}

func writeJSON(path string, v any) error {
	b, _ := json.MarshalIndent(v, "", "  ")
	return q2.WriteFile(path, b)
}

func loadSettings(dir string) settings {
	s := settings{IdleMinutes: 15, Quality: qobuz.HiRes96, StreamQuality: qobuz.CD, LimitMB: 2048}
	readJSON(filepath.Join(dir, "config.json"), &s)
	dirty := false
	if len(s.Key) < 12 {
		s.Key, dirty = q2.Key(12), true
	}
	if len(s.PIN) != 6 {
		s.PIN, dirty = q2.PIN(6), true
	}
	if !validQuality(s.Quality) {
		s.Quality = qobuz.HiRes96
	}
	if !validQuality(s.StreamQuality) {
		s.StreamQuality = qobuz.CD
	}
	if s.LimitMB < 256 {
		s.LimitMB = 2048
	}
	if dirty {
		writeJSON(filepath.Join(dir, "config.json"), s)
	}
	return s
}

func (a *app) settings() settings {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.set
}

func (a *app) update(fn func(*settings)) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.set
	fn(&s)
	if err := writeJSON(filepath.Join(a.opts.dir, "config.json"), s); err != nil {
		return err
	}
	a.set = s
	return nil
}

// manifest keeps q2app's goto line (Q2 Pod's addon_goto) as the setting says: 1 when Go to Album and
// Go to Artist on a Qobuz song open Qobuz's pages. A missing q2app (not installed as a card app) is
// left so.
func (a *app) manifest() {
	path := filepath.Join(a.opts.dir, "q2app")
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	want := "goto=1"
	if a.settings().GotoLibrary {
		want = "goto=0"
	}
	var lines []string
	for _, l := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if !strings.HasPrefix(l, "goto=") {
			lines = append(lines, l)
		}
	}
	out := strings.Join(append(lines, want), "\n") + "\n"
	if out != string(b) {
		if err := q2.WriteFile(path, []byte(out)); err != nil {
			log.Printf("q2app: %v", err)
		}
	}
}

func (a *app) touch(web bool) {
	a.mu.Lock()
	a.used = time.Now()
	if web {
		a.webAt = a.used
	}
	a.mu.Unlock()
}

func listen(port int) (net.Listener, int) {
	for p := port; p < port+10; p++ {
		if l, err := net.Listen("tcp4", fmt.Sprintf(":%d", p)); err == nil {
			return l, p
		}
	}
	return nil, 0
}

func start(o options) (*app, error) {
	if err := os.MkdirAll(o.dir, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(o.private, 0o700); err != nil { // no internal storage: the card it is
		o.private = o.dir
	}
	if o.ready == nil {
		o.ready = func() bool {
			_, err := os.Stat(o.dir)
			return err == nil && q2.LocalIP() != ""
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &app{opts: o, state: &q2.State{Path: o.statusFile}, ctx: ctx, cancel: cancel, used: time.Now(),
		httpc: &http.Client{Transport: tlsroots.Transport(), Timeout: 30 * time.Second},
		cache: &cache.Cache{Dir: filepath.Join(o.dir, "cache")}, armed: map[string]time.Time{},
		nonces: map[string]time.Time{}, lists: map[string]*favs{}, loads: map[string]*load{}, quit: make(chan struct{})}
	a.set = loadSettings(o.dir)
	var s session
	if readJSON(filepath.Join(o.private, "session.json"), &s) && s.Token != "" {
		a.sess = &s
	}
	var c qobuz.Creds
	if readJSON(filepath.Join(o.dir, "creds.json"), &c) && c.AppID != "" {
		a.creds = &c
	}
	a.dl = dl.New(dl.Options{Dir: o.dir, Cache: a.cache, Source: a, Fetch: &http.Client{Transport: tlsroots.Transport()},
		Ready:   func() bool { return o.ready() && a.signedIn() && !a.fetchingStream() },
		Quality: func() int { return a.settings().Quality }, Limit: func() int64 { return int64(a.settings().LimitMB) << 20 },
		OnAuth: a.lostSignIn})
	a.sweep("") // sessions from before, but the one Q2 Pod still plays
	a.manifest()
	os.RemoveAll(filepath.Join(o.dir, "covers")) // where v0.3 kept every cover
	if a.ln, a.port = listen(o.port); a.ln == nil {
		return nil, fmt.Errorf("no port for the phone's page from %d", o.port)
	}
	go serveWeb(a, a.ln)
	a.writeState()
	go func() {
		if err := q2spot.Serve(o.browse, a.device); err != nil {
			log.Printf("browse socket: %v", err)
		}
	}()
	if _, err := q2.Control(o.control, func(cmd []byte) {
		if len(cmd) > 0 && cmd[0] == 'q' {
			a.shutdown()
		}
	}); err != nil {
		log.Printf("control socket: %v", err)
	}
	a.dl.Start()
	a.bg.Add(1)
	go func() { defer a.bg.Done(); a.warm() }()
	log.Printf("%s: signed in %v, phone page on port %d", version, a.signedIn(), a.port)
	return a, nil
}

// warm reads the web player's app id once Wi-Fi is up, and the signing secret once signed in, so
// the first press needn't wait for them. The web player is read again once a run (its script only
// when it has changed), so an app id Qobuz has retired doesn't keep a sign-in from starting.
func (a *app) warm() {
	fresh := false
	for i := 0; i < 60 && a.ctx.Err() == nil; i++ {
		if q2.LocalIP() != "" {
			if !fresh {
				fresh = a.refreshCreds(a.ctx) == nil
			}
			if c, err := a.client(a.ctx); err == nil {
				if s := a.account(); s.Name == "" { // a sign-in from before names were kept
					if u, err := c.User(a.ctx); err == nil {
						s.Name, s.HiRes, s.Plan = u.DisplayName, u.Credential.Parameters.HiRes, u.Credential.Parameters.Label
						if s.Plan == "" {
							s.Plan = u.Credential.Label
						}
						a.saveSession(&s)
					}
				}
				a.secret(a.ctx, c)
				if fresh {
					return
				}
			} else if !errors.Is(err, errSignedOut) {
				log.Printf("warm: %v", err)
			} else if fresh {
				return
			}
		}
		select {
		case <-a.ctx.Done():
		case <-time.After(5 * time.Second):
		}
	}
}

func (a *app) shutdown() { a.once.Do(func() { close(a.quit) }) }

func (a *app) run() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-a.quit:
			a.cancel()
			a.stopStream()
			a.dl.Stop()
			a.bg.Wait()
			a.ln.Close()
			a.state.Remove()
			os.Remove(a.opts.browse)
			os.Remove(a.opts.control)
			log.Printf("stopped")
			return
		case <-t.C:
			a.writeState()
			s := a.settings()
			a.mu.Lock()
			idle := time.Since(a.used)
			a.mu.Unlock()
			if s.IdleMinutes > 0 && idle > time.Duration(s.IdleMinutes)*time.Minute && !a.working() {
				log.Printf("idle: stopping")
				a.shutdown()
			}
		}
	}
}

func (a *app) working() bool {
	return a.streaming() || a.dl.Busy() || a.dl.Pending() && a.signedIn() && q2.LocalIP() != ""
}

// --- the account ---

var errSignedOut = errors.New("sign in to Qobuz from your phone first")

func (a *app) signedIn() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sess != nil
}

func (a *app) account() session {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.sess == nil {
		return session{}
	}
	return *a.sess
}

func (a *app) saveSession(s *session) error {
	a.mu.Lock()
	a.sess = s
	a.mu.Unlock()
	if s == nil {
		os.Remove(filepath.Join(a.opts.private, "session.json"))
		return nil
	}
	return writeJSON(filepath.Join(a.opts.private, "session.json"), s)
}

// lostSignIn is a 401 from Qobuz: the token is gone, the lists kept; downloads wait for a new one.
func (a *app) lostSignIn() {
	if a.signedIn() {
		log.Printf("Qobuz no longer takes the sign-in")
		a.saveSession(nil)
	}
}

// appCreds is the web player's app id and secrets: read once and kept on the card, read again
// when its script changes.
func (a *app) appCreds(ctx context.Context) (*qobuz.Creds, error) {
	a.mu.Lock()
	cached := a.creds
	a.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	c, err := qobuz.FetchCreds(ctx, a.httpc, nil)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	a.creds = c
	a.mu.Unlock()
	writeJSON(filepath.Join(a.opts.dir, "creds.json"), c)
	return c, nil
}

// refreshCreds reads the web player again, after Qobuz refused the app id or a signature.
func (a *app) refreshCreds(ctx context.Context) error {
	a.mu.Lock()
	cached := a.creds
	a.mu.Unlock()
	c, err := qobuz.FetchCreds(ctx, a.httpc, cached)
	if err != nil {
		return err
	}
	a.mu.Lock()
	changed := a.creds == nil || c.Bundle != a.creds.Bundle
	a.creds = c
	a.mu.Unlock()
	if changed {
		writeJSON(filepath.Join(a.opts.dir, "creds.json"), c)
		if s := a.account(); s.Token != "" {
			s.Secret = ""
			a.saveSession(&s)
		}
	}
	return nil
}

// client is a signed-in client.
func (a *app) client(ctx context.Context) (*qobuz.Client, error) {
	creds, err := a.appCreds(ctx)
	if err != nil {
		return nil, err
	}
	s := a.account()
	if s.Token == "" {
		return nil, errSignedOut
	}
	secret := s.Secret
	if s.Bundle != creds.Bundle { // found for another web player: found again
		secret = ""
	}
	return &qobuz.Client{HTTP: a.httpc, Creds: creds, Token: s.Token, UserID: s.UserID, Secret: secret}, nil
}

// secret is the one of the web player's secrets that signs this account's file requests, found
// once and kept with the sign-in.
func (a *app) secret(ctx context.Context, c *qobuz.Client) (string, error) {
	if c.Secret != "" {
		return c.Secret, nil
	}
	probe := probeTrack // else the newest liked song Qobuz streams
	if l := a.listNow("tracks"); l != nil {
		a.mu.Lock() // fetchList replaces the list under it
		for _, t := range l.Tracks {
			if t.Streamable {
				probe = t.ID.String()
				break
			}
		}
		a.mu.Unlock()
	}
	secret, err := c.FindSecret(ctx, probe)
	if err != nil {
		return "", err
	}
	if s := a.account(); s.Token == c.Token {
		s.Secret, s.Bundle = secret, c.Creds.Bundle
		a.saveSession(&s)
	}
	c.Secret = secret
	return secret, nil
}

// signIn trades a sign-in's code for a session, with the account's name and plan.
func (a *app) signIn(ctx context.Context, code string) (session, error) {
	creds, err := a.appCreds(ctx)
	if err != nil {
		return session{}, err
	}
	c := &qobuz.Client{HTTP: a.httpc, Creds: creds}
	if err := c.Exchange(ctx, code); err != nil {
		return session{}, err
	}
	s := session{Token: c.Token, UserID: c.UserID, SignedIn: time.Now()}
	if u, err := c.User(ctx); err == nil {
		s.Name, s.HiRes = u.DisplayName, u.Credential.Parameters.HiRes
		s.Plan = u.Credential.Parameters.Label
		if s.Plan == "" {
			s.Plan = u.Credential.Label
		}
	}
	if err := a.saveSession(&s); err != nil {
		return s, err
	}
	a.mu.Lock()
	a.lists = map[string]*favs{} // another account's lists, if any, forgotten
	a.mu.Unlock()
	os.RemoveAll(filepath.Join(a.opts.dir, "lists"))
	go func() {
		if c, err := a.client(a.ctx); err == nil {
			a.secret(a.ctx, c)
		}
		a.dl.Wake()
	}()
	log.Printf("signed in")
	return s, nil
}

// --- dl.Source ---

// File is a track's address at quality, signed; a refused signature reads the web player again once.
func (a *app) File(ctx context.Context, id string, quality int) (*qobuz.File, error) {
	for attempt := 0; ; attempt++ {
		c, err := a.client(ctx)
		if err != nil {
			if errors.Is(err, errSignedOut) {
				return nil, &qobuz.Error{Status: 401, Message: err.Error()}
			}
			return nil, err
		}
		secret, err := a.secret(ctx, c)
		if err == nil {
			var f *qobuz.File
			if f, err = c.FileURL(ctx, id, quality, secret); err == nil {
				return f, nil
			}
		}
		// Stale: the app id or every secret refused, a 400 or 403 (a lost sign-in is a 401). Not the
		// network, nor a track Qobuz won't send (qobuz.ErrNoFile).
		var qe *qobuz.Error
		stale := errors.As(err, &qe) && (qe.Status == 400 || qe.Status == 403)
		if attempt == 0 && stale && ctx.Err() == nil {
			if a.refreshCreds(ctx) == nil {
				if s := a.account(); s.Secret != "" { // its secret refused: found again
					s.Secret = ""
					a.saveSession(&s)
				}
				continue
			}
		}
		return nil, err
	}
}

type cover struct {
	album string
	jpeg  []byte
}

const coversKept = 4

// Cover is an album's 600 px front cover, the last few kept in memory: an album's tracks share one,
// and each file carries its own.
func (a *app) Cover(ctx context.Context, al *qobuz.Album) []byte {
	if al == nil || al.Image.Large == "" {
		return nil
	}
	id := al.ID.String()
	a.mu.Lock()
	for _, c := range a.covers {
		if c.album == id {
			a.mu.Unlock()
			return c.jpeg
		}
	}
	a.mu.Unlock()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, al.Image.Large, nil)
	if err != nil {
		return nil
	}
	resp, err := a.httpc.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil || resp.StatusCode != http.StatusOK || len(b) < 4 || b[0] != 0xff || b[1] != 0xd8 {
		return nil
	}
	a.mu.Lock()
	a.covers = append([]cover{{id, b}}, a.covers[:min(len(a.covers), coversKept-1)]...)
	a.mu.Unlock()
	return b
}

// --- the status file ---

func (a *app) URL(withKey bool) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ip == "" {
		return ""
	}
	u := fmt.Sprintf("http://%s:%d/", a.ip, a.port)
	if withKey {
		u += "?k=" + a.set.Key
	}
	return u
}

func (a *app) addr() string {
	u := a.URL(false)
	if u == "" {
		return ""
	}
	return u[len("http://") : len(u)-1]
}

// writeState is what Q2 Pod reads: the phone page's address, PIN and QR code, and whether to hold
// standby off.
func (a *app) writeState() {
	ip := q2.LocalIP()
	a.mu.Lock()
	a.ip = ip
	a.mu.Unlock()
	url := a.URL(true)
	a.mu.Lock()
	if url != a.qrFor {
		a.qrFor, a.qrHex, a.qrSize = url, "", 0
		if url != "" {
			if c, err := qr.Encode([]byte(url)); err == nil {
				a.qrHex, a.qrSize = c.Hex(), c.Size
			}
		}
	}
	qrHex, qrSize, pin, web := a.qrHex, a.qrSize, a.set.PIN, time.Since(a.webAt) < webHold
	a.mu.Unlock()
	note := "Then press Sign In with Qobuz"
	if a.signedIn() {
		note = "Settings and downloads"
	}
	if ip == "" {
		note = "Connect the Q2 to Wi-Fi"
	}
	hold := "0"
	if web || a.working() || a.loading() {
		hold = "1"
	}
	a.state.Write(map[string]string{"addr": a.addr(), "pin": pin, "qr": qrHex, "qrsize": fmt.Sprint(qrSize),
		"note": note, "hold": hold, "version": version})
}
