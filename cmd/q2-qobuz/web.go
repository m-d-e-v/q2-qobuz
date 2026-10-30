package main

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"fmt"
	"html"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"q2-qobuz/internal/dl"
	"q2-qobuz/internal/q2"
	"q2-qobuz/internal/qobuz"
)

//go:embed web/index.html
var indexHTML []byte

const cookieName = "q2qobuz"

// web is the phone's page: index.html and a small JSON API behind the key the QR code carries (or
// the PIN on the Q2's screen), kept in a cookie, every change carrying X-Q2: 1. Sign In with Qobuz
// sends the phone to Qobuz's own sign-in, which returns it to /signin/<nonce> with a code. That
// return carries no cookie (SameSite=Strict isn't sent on a navigation from qobuz.com), so the
// nonce, made for that one sign-in and good for 15 minutes, is what admits it.
type web struct {
	a *app

	mu      sync.Mutex
	fails   int
	locks   int // lockouts since the PIN was last right, each twice as long as the one before
	lockout time.Time
}

func serveWeb(a *app, l net.Listener) {
	srv := &http.Server{Handler: newWeb(a), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second}
	srv.Serve(l)
}

func newWeb(a *app) http.Handler {
	w := &web{a: a}
	mux := http.NewServeMux()
	mux.HandleFunc("/", w.index)
	mux.HandleFunc("/signin/", w.signedIn)
	mux.HandleFunc("/api/login", w.login)
	mux.HandleFunc("/api/", w.api)
	return mux
}

func (w *web) authed(r *http.Request) bool {
	c, err := r.Cookie(cookieName)
	return err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(w.a.settings().Key)) == 1
}

func (w *web) setCookie(rw http.ResponseWriter) {
	http.SetCookie(rw, &http.Cookie{Name: cookieName, Value: w.a.settings().Key, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, MaxAge: 400 * 24 * 3600})
}

func headers(rw http.ResponseWriter) {
	h := rw.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", "default-src 'self'; script-src 'unsafe-inline'; style-src 'unsafe-inline'")
}

func (w *web) index(rw http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(rw, r)
		return
	}
	if k := r.URL.Query().Get("k"); k != "" {
		if subtle.ConstantTimeCompare([]byte(k), []byte(w.a.settings().Key)) == 1 {
			w.a.touch(true)
			w.setCookie(rw)
		}
		http.Redirect(rw, r, "/", http.StatusFound)
		return
	}
	if w.authed(r) { // a page no one has signed in to keeps nothing awake
		w.a.touch(true)
	}
	headers(rw)
	rw.Write(indexHTML)
}

// signedIn is where Qobuz's sign-in returns, with code_autorisation.
func (w *web) signedIn(rw http.ResponseWriter, r *http.Request) {
	nonce := strings.TrimPrefix(r.URL.Path, "/signin/")
	w.a.mu.Lock()
	at, ok := w.a.nonces[nonce]
	if ok {
		delete(w.a.nonces, nonce)
	}
	w.a.mu.Unlock()
	headers(rw)
	page := func(title, msg string) {
		fmt.Fprintf(rw, `<!doctype html><meta name="viewport" content="width=device-width"><title>Qobuz on the Q2</title>`+
			`<body style="font:17px -apple-system,sans-serif;margin:15vh 24px;text-align:center"><h2>%s</h2><p>%s</p>`+
			`<p><a href="/">Back to the Q2's page</a></p>`, html.EscapeString(title), html.EscapeString(msg))
	}
	code := r.URL.Query().Get("code_autorisation")
	if code == "" {
		code = r.URL.Query().Get("code")
	}
	switch {
	case !ok || time.Since(at) > 15*time.Minute:
		rw.WriteHeader(http.StatusForbidden)
		page("That sign-in has expired", "Press Sign In with Qobuz again.")
		return
	case code == "":
		page("Qobuz didn't sign you in", "Press Sign In with Qobuz to try again.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	s, err := w.a.signIn(ctx, code)
	if err != nil {
		page("Qobuz didn't take the sign-in", say(err))
		return
	}
	w.a.touch(true)
	name := s.Name
	if name == "" {
		name = "your account"
	}
	page("Signed in", "The Q2 now has "+name+"'s Qobuz library. You can close this page.")
}

func reply(rw http.ResponseWriter, code int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.Header().Set("Cache-Control", "no-store")
	rw.WriteHeader(code)
	json.NewEncoder(rw).Encode(v)
}

func refuse(rw http.ResponseWriter, code int, msg string) {
	reply(rw, code, map[string]string{"error": msg})
}

// login takes the Q2's PIN. Ten wrong ones lock it for a minute, the next ten for two, then four…
func (w *web) login(rw http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.Header.Get("X-Q2") != "1" {
		refuse(rw, http.StatusBadRequest, "bad request")
		return
	}
	var body struct{ PIN string }
	json.NewDecoder(http.MaxBytesReader(rw, r.Body, 1024)).Decode(&body)
	w.mu.Lock()
	wait := time.Until(w.lockout)
	ok := wait <= 0 && subtle.ConstantTimeCompare([]byte(strings.ReplaceAll(strings.TrimSpace(body.PIN), " ", "")),
		[]byte(w.a.settings().PIN)) == 1
	if !ok && wait <= 0 {
		if w.fails++; w.fails >= 10 {
			w.lockout = time.Now().Add(time.Minute << min(w.locks, 10))
			w.fails, w.locks = 0, w.locks+1
		}
	}
	if ok {
		w.fails, w.locks = 0, 0
	}
	w.mu.Unlock()
	switch {
	case wait > 0:
		refuse(rw, http.StatusTooManyRequests, "Too many tries. Wait "+plural(int((wait+time.Minute-1)/time.Minute), "minute")+".")
	case !ok:
		refuse(rw, http.StatusForbidden, "That's not the PIN on the Q2's screen.")
	default:
		w.a.touch(true)
		w.setCookie(rw)
		reply(rw, http.StatusOK, map[string]bool{"ok": true})
	}
}

var states = map[dl.State]string{dl.Waiting: "waiting", dl.Active: "active", dl.Failed: "failed"}

func (w *web) api(rw http.ResponseWriter, r *http.Request) {
	route := strings.TrimPrefix(r.URL.Path, "/api/")
	post := r.Method == http.MethodPost
	if post && r.Header.Get("X-Q2") != "1" || !post && r.Method != http.MethodGet {
		refuse(rw, http.StatusBadRequest, "bad request")
		return
	}
	if !w.authed(r) {
		refuse(rw, http.StatusUnauthorized, "sign in")
		return
	}
	w.a.touch(true)
	a := w.a
	var body struct {
		Quality       int    `json:"quality"`
		StreamQuality int    `json:"stream_quality"`
		LimitMB       int    `json:"limit_mb"`
		Ctx           string `json:"ctx"`
		ID            string `json:"id"`
	}
	if post {
		json.NewDecoder(http.MaxBytesReader(rw, r.Body, 4096)).Decode(&body)
	}
	switch {
	case route == "state" && !post:
		s, set := a.account(), a.settings()
		used, files := a.cache.Usage()
		jobs := []map[string]any{}
		for _, j := range a.dl.Jobs() {
			jobs = append(jobs, map[string]any{"ctx": j.Ctx, "id": j.Track.ID.String(), "title": qobuz.FullTitle(j.Track.Title, j.Track.Version),
				"artist": j.Track.Performer.Name, "state": states[j.State], "error": j.Err, "done": j.Done, "size": j.Size})
		}
		reply(rw, http.StatusOK, map[string]any{"version": version, "signedIn": s.Token != "", "name": s.Name, "plan": s.Plan,
			"hires": s.HiRes, "since": s.SignedIn, "quality": set.Quality, "streamQuality": set.StreamQuality,
			"limitMB": set.LimitMB, "used": used, "files": files,
			"jobs": jobs, "online": q2.LocalIP() != ""})
	case route == "signin" && post:
		creds, err := a.appCreds(r.Context())
		if err != nil {
			refuse(rw, http.StatusBadGateway, "The Q2 can't reach Qobuz: "+err.Error())
			return
		}
		nonce := q2.Key(20)
		a.mu.Lock()
		for k, at := range a.nonces {
			if time.Since(at) > 15*time.Minute {
				delete(a.nonces, k)
			}
		}
		a.nonces[nonce] = time.Now()
		a.mu.Unlock()
		back := fmt.Sprintf("http://%s/signin/%s", r.Host, nonce)
		reply(rw, http.StatusOK, map[string]string{"url": qobuz.SignInURL(creds.AppID, back)})
	case route == "signout" && post:
		a.dl.CancelContext("")
		a.saveSession(nil)
		reply(rw, http.StatusOK, map[string]bool{"ok": true})
	case route == "settings" && post:
		err := a.update(func(s *settings) {
			if validQuality(body.Quality) {
				s.Quality = body.Quality
			}
			if validQuality(body.StreamQuality) {
				s.StreamQuality = body.StreamQuality
			}
			if body.LimitMB >= 512 && body.LimitMB <= 1<<20 {
				s.LimitMB = body.LimitMB
			}
		})
		if err != nil {
			refuse(rw, http.StatusInternalServerError, "Couldn't save the setting")
			return
		}
		if used, _ := a.cache.Usage(); used > int64(a.settings().LimitMB)<<20 {
			go a.cache.Trim(int64(a.settings().LimitMB)<<20, "")
		}
		reply(rw, http.StatusOK, map[string]bool{"ok": true})
	case route == "cancel" && post:
		if body.Ctx == "" && body.ID == "" {
			a.dl.CancelContext("")
		} else {
			a.dl.Cancel(body.Ctx, body.ID)
		}
		reply(rw, http.StatusOK, map[string]bool{"ok": true})
	case route == "retry" && post:
		reply(rw, http.StatusOK, map[string]int{"retried": a.dl.RetryFailed()})
	default:
		http.NotFound(rw, r)
	}
}
