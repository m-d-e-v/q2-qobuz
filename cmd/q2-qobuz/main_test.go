package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"q2-qobuz/internal/q2"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"q2-qobuz/internal/dl"
	"q2-qobuz/internal/q2spot"
	"q2-qobuz/internal/qobuz"
)

const secret = "0123456789abcdef0123456789abcdef"

var coverURL string // the fake's, absolute as Qobuz's are

// fakeQobuz is the web player's script, the API and the file servers, as far as the app uses them.
type fakeQobuz struct {
	*httptest.Server
	dead     atomic.Bool // the token no longer taken
	badSigs  atomic.Int32
	files    atomic.Int32
	covers   atomic.Int32
	logins   atomic.Int32 // the web player's page read
	noFile   sync.Map     // track ids answered without a file, as for a rights holder's restriction
	noFiles  atomic.Int32
	albums   int      // liked albums
	playlist []string // track ids, in order
}

func flacFile(id string) []byte {
	b := []byte("fLaC")
	si := make([]byte, 34)
	binary.BigEndian.PutUint64(si[10:], uint64(96000)<<44|uint64(1)<<41|uint64(23)<<36|uint64(96000*200))
	b = append(b, 0x00, 0, 0, 34)
	b = append(b, si...)
	b = append(b, 0x81, 0, 0, 16)
	b = append(b, make([]byte, 16)...)
	return append(b, bytes.Repeat([]byte("frame-"+id+"-"), 20000)...)
}

func track(id, title string, n int, album string) map[string]any {
	return map[string]any{"id": id, "title": title, "duration": 200, "track_number": n, "media_number": 1,
		"streamable": true, "maximum_bit_depth": 24, "maximum_sampling_rate": 96, "favorited_at": 1700000000 + n,
		"performer": map[string]any{"id": 1, "name": "The Band"},
		"album": map[string]any{"id": album, "title": "Record", "media_count": 1, "tracks_count": 3,
			"artist": map[string]any{"id": 1, "name": "The Band"}, "image": map[string]any{"large": coverURL}}}
}

func newFake(t *testing.T) *fakeQobuz {
	f := &fakeQobuz{albums: 70, playlist: []string{"t2", "t1"}}
	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		f.logins.Add(1)
		fmt.Fprint(w, `<html><script src="/resources/9.9.9-x001/bundle.js"></script>`)
	})
	mux.HandleFunc("/resources/9.9.9-x001/bundle.js", func(w http.ResponseWriter, r *http.Request) {
		enc := base64.URLEncoding.EncodeToString([]byte(secret)) + strings.Repeat("A", 44)
		fmt.Fprintf(w, `a={production:{api:{appId:"123456789",appSecret:"x"}}};n.authenticate({privateKey:"pk1",code:t});`+
			`f.initialSeed("%s",window.utimezone.london);{name:"Europe/London",info:"%s",extras:"%s"}`, enc[:10], enc[10:30], enc[30:])
	})
	mux.HandleFunc("/cover.jpg", func(w http.ResponseWriter, r *http.Request) {
		f.covers.Add(1)
		w.Write([]byte{0xff, 0xd8, 0xff, 0xd9})
	})
	mux.HandleFunc("/mp3/", func(w http.ResponseWriter, r *http.Request) {
		frames := bytes.Repeat([]byte{0xff, 0xfb, 0x90, 0x64, 1, 2, 3, 4}, 20000)
		http.ServeContent(w, r, "a.mp3", time.Time{}, bytes.NewReader(append([]byte("ID3\x04\x00\x00\x00\x00\x00\x02QQ"), frames...)))
	})
	mux.HandleFunc("/audio/", func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "a.flac", time.Time{}, bytes.NewReader(flacFile(strings.TrimPrefix(r.URL.Path, "/audio/"))))
	})
	mux.HandleFunc("/api.json/0.2/", func(w http.ResponseWriter, r *http.Request) {
		ep, q := strings.TrimPrefix(r.URL.Path, "/api.json/0.2/"), r.URL.Query()
		out := func(v any) { json.NewEncoder(w).Encode(v) }
		if r.Header.Get("X-App-Id") != "123456789" {
			w.WriteHeader(400)
			out(map[string]any{"message": "Invalid or missing app_id parameter"})
			return
		}
		if ep == "oauth/callback" {
			if q.Get("code") != "good" || q.Get("private_key") != "pk1" {
				w.WriteHeader(401)
				out(map[string]any{"message": "bad code"})
				return
			}
			out(map[string]any{"token": "T0K", "user_id": "77"})
			return
		}
		if r.Header.Get("X-User-Auth-Token") != "T0K" || f.dead.Load() {
			w.WriteHeader(401)
			out(map[string]any{"message": "User authentication is required."})
			return
		}
		page := func(items []any) map[string]any {
			return map[string]any{"limit": 500, "offset": 0, "total": len(items), "items": items}
		}
		switch ep {
		case "user/get":
			out(map[string]any{"display_name": "Matt", "credential": map[string]any{"label": "streaming-studio",
				"parameters": map[string]any{"hires_streaming": true, "short_label": "Studio"}}})
		case "favorite/getUserFavorites":
			switch q.Get("type") {
			case "albums":
				var items []any
				for i := 0; i < f.albums; i++ {
					items = append(items, map[string]any{"id": fmt.Sprintf("X%d", i), "title": fmt.Sprintf("%c album %02d", 'A'+i%26, i),
						"artist": map[string]any{"name": "Someone"}, "release_date_original": "2016-07-01", "maximum_bit_depth": 24})
				}
				items[0] = map[string]any{"id": "A1", "title": "Record", "artist": map[string]any{"name": "The Band"}, "maximum_bit_depth": 24}
				out(map[string]any{"albums": page(items)})
			case "tracks":
				out(map[string]any{"tracks": page([]any{track("t1", "First", 1, "A1"), track("t9", "Lone", 9, "A9")})})
			case "artists":
				out(map[string]any{"artists": page([]any{map[string]any{"id": 5, "name": "The Band", "albums_count": 1}})})
			}
		case "playlist/getUserPlaylists":
			out(map[string]any{"playlists": page([]any{map[string]any{"id": 555, "name": "Mix", "tracks_count": 2,
				"owner": map[string]any{"id": 77, "name": "Matt"}}})})
		case "playlist/get":
			var items []any
			for i, id := range f.playlist {
				items = append(items, track(id, "Song "+id, i+1, "A1"))
			}
			out(map[string]any{"id": 555, "name": "Mix", "tracks": page(items)})
		case "album/get":
			tracks := []any{track("t1", "First", 1, "A1"), track("t2", "Second", 2, "A1"), track("t3", "Third", 3, "A1")}
			switch q.Get("album_id") {
			case "A7": // one Qobuz doesn't stream here
				locked := track("t4", "Fourth", 4, "A7")
				locked["streamable"] = false
				tracks = append(tracks, locked)
			case "A8": // two in a row Qobuz doesn't stream here, and one more
				t5, t6, t7 := track("t5", "Fifth", 2, "A8"), track("t6", "Sixth", 3, "A8"), track("t7", "Seventh", 5, "A8")
				t5["streamable"], t6["streamable"], t7["streamable"] = false, false, false
				tracks = []any{track("t1", "First", 1, "A8"), t5, t6, track("t2", "Second", 4, "A8"), t7}
			case "A100":
				tracks = nil
				for i := 1; i <= 100; i++ {
					tracks = append(tracks, track(fmt.Sprintf("b%d", i), fmt.Sprintf("Track %d", i), i, "A100"))
				}
			}
			out(map[string]any{"id": q.Get("album_id"), "title": "Record", "media_count": 1, "tracks_count": len(tracks),
				"artist": map[string]any{"name": "The Band"}, "image": map[string]any{"large": coverURL},
				"genre": map[string]any{"name": "Pop"}, "release_date_original": "2016-07-01", "tracks": page(tracks)})
		case "track/get":
			out(track(q.Get("track_id"), "Second", 2, "A1"))
		case "artist/get":
			out(map[string]any{"id": 1, "name": "The Band", "albums": page([]any{
				map[string]any{"id": "A1", "title": "Record", "artist": map[string]any{"id": 1, "name": "The Band"}, "release_date_original": "2016-07-01"},
				map[string]any{"id": "A2", "title": "Not in the Library", "artist": map[string]any{"id": 1, "name": "The Band"}, "release_date_original": "2019-01-01"}})})
		case "track/getFileUrl":
			args := map[string]string{"format_id": q.Get("format_id"), "intent": q.Get("intent"), "track_id": q.Get("track_id")}
			if q.Get("request_sig") != qobuz.Sign("track", "getFileUrl", args, q.Get("request_ts"), secret) {
				f.badSigs.Add(1)
				w.WriteHeader(400)
				out(map[string]any{"message": "Invalid Request Signature parameter (request_sig)"})
				return
			}
			f.files.Add(1)
			if _, ok := f.noFile.Load(q.Get("track_id")); ok {
				f.noFiles.Add(1)
				out(map[string]any{"track_id": q.Get("track_id"), "restrictions": []any{map[string]any{"code": "TrackRestrictedByRightHolders"}}})
				return
			}
			fid, _ := strconv.Atoi(q.Get("format_id"))
			if fid == 5 {
				out(map[string]any{"url": f.URL + "/mp3/" + q.Get("track_id"), "format_id": 5, "mime_type": "audio/mpeg"})
				return
			}
			out(map[string]any{"url": f.URL + "/audio/" + q.Get("track_id"), "format_id": min(fid, 7), "mime_type": "audio/flac",
				"bit_depth": 24, "sampling_rate": 96})
		case "catalog/search":
			out(map[string]any{"albums": page([]any{map[string]any{"id": "A1", "title": "Record"}}),
				"tracks": page([]any{track("t3", "Third", 3, "A1")}), "artists": page(nil), "playlists": page(nil)})
		default:
			w.WriteHeader(404)
			out(map[string]any{"message": "no " + ep})
		}
	})
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	coverURL = f.URL + "/cover.jpg"
	qobuz.Play, qobuz.API = f.URL, f.URL+"/api.json/0.2/"
	return f
}

// startApp starts the app on a card and storage of its own, each of before given the app's folder
// and the storage first.
func startApp(t *testing.T, before ...func(dir, private string)) (*app, string, string) {
	card, private := t.TempDir(), t.TempDir()
	for _, fn := range before {
		os.MkdirAll(filepath.Join(card, ".qobuz"), 0o755)
		fn(filepath.Join(card, ".qobuz"), private)
	}
	socks, err := os.MkdirTemp("/tmp", "q2q")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(socks) })
	q2.CapsFile = filepath.Join(socks, "caps") // newer Q2 Pod's: the goto feature offered
	os.WriteFile(q2.CapsFile, []byte("playlist progress pages library goto\n"), 0o644)
	dl.RetryAfter = 10 * time.Millisecond
	a, err := start(options{dir: filepath.Join(card, ".qobuz"), private: private,
		statusFile: filepath.Join(socks, "state"), browse: filepath.Join(socks, "b.sock"), control: filepath.Join(socks, "c.sock"),
		playFile: filepath.Join(socks, "play"), playingFile: filepath.Join(socks, "playing"),
		port: 18400, ready: func() bool { return true }})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { a.run(); close(done) }()
	t.Cleanup(func() { a.shutdown(); <-done })
	for end := time.Now().Add(3 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if c, err := net.Dial("unix", a.opts.browse); err == nil {
			c.Close()
			break
		}
		if time.Now().After(end) {
			t.Fatal("no browse socket")
		}
	}
	return a, card, private
}

type answer struct {
	status, message, cursor string
	rows                    [][6]string
}

var reqID = 0

// spot asks as addon.c does and checks the reply as its spot_reply would.
func spot(t *testing.T, a *app, op string, args ...string) answer {
	t.Helper()
	reqID++
	c, err := net.Dial("unix", a.opts.browse)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	line := fmt.Sprintf("Q2SPOT/1\n%d\t%s", reqID, op)
	for _, s := range args {
		line += "\t" + q2spot.Escape(s)
	}
	c.Write([]byte(line + "\n"))
	c.(*net.UnixConn).CloseWrite()
	c.SetDeadline(time.Now().Add(20 * time.Second))
	raw, _ := io.ReadAll(c)
	if len(raw) > q2spot.MaxReply {
		t.Fatalf("%s: %d bytes", op, len(raw))
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	un := func(s string) string {
		u, err := q2spot.Unescape(s)
		if err != nil {
			t.Fatalf("%s: bad escape %q", op, s)
		}
		return u
	}
	head := strings.Split(lines[1], "\t")
	if lines[0] != "Q2SPOT/1" || len(head) != 4 || head[0] != fmt.Sprint(reqID) || len(lines)-2 > q2spot.MaxRows {
		t.Fatalf("%s: %q", op, raw)
	}
	r := answer{status: un(head[1]), message: un(head[2]), cursor: un(head[3])}
	for _, l := range lines[2:] {
		f := strings.Split(l, "\t")
		if len(f) != 6 {
			t.Fatalf("%s: row %q", op, l)
		}
		var row [6]string
		for i := range f {
			row[i] = un(f[i])
		}
		if len(row[2]) > 255 || !utf8.ValidString(row[2]) {
			t.Fatalf("%s: %q", op, row)
		}
		r.rows = append(r.rows, row)
	}
	return r
}

func titles(r answer) []string {
	var out []string
	for _, row := range r.rows {
		out = append(out, row[2])
	}
	return out
}

func row(t *testing.T, r answer, title string) [6]string {
	t.Helper()
	for _, x := range r.rows {
		if x[2] == title {
			return x
		}
	}
	t.Fatalf("no %q in %q", title, titles(r))
	return [6]string{}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); !ok(); time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func until(t *testing.T, what string, ask func() answer, ok func(answer) bool) answer {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(30 * time.Millisecond) {
		r := ask()
		if ok(r) {
			return r
		}
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s: %s %q %q", what, r.status, r.message, titles(r))
		}
	}
}

// signIn goes through the phone page as a phone would: the PIN, Sign In with Qobuz, and Qobuz
// sending it back with a code.
func signIn(t *testing.T, a *app) {
	t.Helper()
	page := httptest.NewServer(newWeb(a))
	t.Cleanup(page.Close)
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	post := func(path, body string) *http.Response {
		req, _ := http.NewRequest("POST", page.URL+path, strings.NewReader(body))
		req.Header.Set("X-Q2", "1")
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if resp := post("/api/signin", "{}"); resp.StatusCode != 401 {
		t.Fatalf("sign-in without the PIN: %d", resp.StatusCode)
	}
	post("/api/login", `{"PIN":"`+a.settings().PIN+`"}`)
	var out struct{ URL string }
	json.NewDecoder(post("/api/signin", "{}").Body).Decode(&out)
	u, _ := url.Parse(out.URL)
	back, _ := url.Parse(u.Query().Get("redirect_url"))
	if u.Query().Get("ext_app_id") != "123456789" || !strings.HasPrefix(back.Path, "/signin/") || back.Host != strings.TrimPrefix(page.URL, "http://") {
		t.Fatalf("sign-in address %s", out.URL)
	}
	nocookie := &http.Client{} // Qobuz's redirect brings no cookie
	resp, _ := nocookie.Get(page.URL + back.Path + "?code_autorisation=good")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !bytes.Contains(body, []byte("Signed in")) || !bytes.Contains(body, []byte("Matt")) {
		t.Fatalf("return %d %s", resp.StatusCode, body)
	}
	if resp, _ := nocookie.Get(page.URL + back.Path + "?code_autorisation=good"); resp.StatusCode != 403 {
		t.Fatalf("a nonce used twice: %d", resp.StatusCode)
	}
}

func TestQobuzOnTheQ2(t *testing.T) {
	f := newFake(t)
	a, card, private := startApp(t)

	menu := spot(t, a, "menu")
	if row(t, menu, "Sign In from Your Phone")[0] != "remote" {
		t.Fatal("signed-out menu")
	}
	if r := spot(t, a, "albums"); r.status != "error" {
		t.Fatalf("albums while signed out: %+v", r)
	}
	signIn(t, a)
	if _, err := os.Stat(filepath.Join(private, "session.json")); err != nil {
		t.Fatal("the sign-in isn't kept privately")
	}
	if _, err := os.Stat(filepath.Join(card, ".qobuz", "session.json")); err == nil {
		t.Fatal("the sign-in is on the card")
	}

	menu = until(t, "the lists", func() answer { return spot(t, a, "menu") },
		func(r answer) bool { return r.message != "refresh" })
	if row(t, menu, "Albums")[3] != "70 albums" || row(t, menu, "Songs")[3] != "2 liked songs" || row(t, menu, "Playlists")[3] != "1 playlist" {
		t.Fatalf("menu %q", menu.rows)
	}
	letters := spot(t, a, "albums")
	if len(letters.rows) != 26 || letters.rows[0][2] != "A" || letters.rows[0][1] != "albums" {
		t.Fatalf("letters %q", titles(letters))
	}
	rs := spot(t, a, "albums", "R")
	rec := row(t, rs, "Record")
	if rec[0] != "list" || rec[1] != "album" || rec[4] != "A1" || rec[5] != "dl:album-A1" {
		t.Fatalf("album row %q", rec)
	}

	page := until(t, "the album", func() answer { return spot(t, a, "album", "A1") }, func(r answer) bool { return r.message != "refresh" })
	dlRow := row(t, page, "Download Album")
	if dlRow[1] != "getall" || dlRow[4] != "album-A1" || !strings.Contains(dlRow[3], "3 tracks") {
		t.Fatalf("download row %q", dlRow)
	}
	// A track streams when pressed; held, Play/Pause downloads it.
	if first := row(t, page, "First"); first[0] != "action" || first[1] != "play" || first[4] != "album-A1:t1" || first[5] != "dl:album-A1:t1" {
		t.Fatalf("track row %q", first)
	}
	if r := spot(t, a, "getall", "album-A1"); r.message != "Downloading 3 tracks of Record" {
		t.Fatalf("getall %+v", r)
	}
	page = until(t, "the album on the card", func() answer { return spot(t, a, "album", "A1") },
		func(r answer) bool { return len(r.rows) > 1 && r.rows[0][2] == "All on the Q2" })
	want := filepath.Join(card, ".qobuz", "cache", "album-A1", "01 First [t1].flac")
	first := row(t, page, "First")
	if first[0] != "local" || first[1] != want || first[4] != "album" || first[5] != "rm:album-A1:t1" {
		t.Fatalf("downloaded row %q", first)
	}
	fh, _ := os.Open(want)
	info, err := qobuz.ReadFLAC(fh)
	fh.Close()
	if err != nil || info.Tags["TITLE"] != "First" || info.Tags["ALBUM"] != "Record" || info.Tags["TRACKNUMBER"] != "1" ||
		info.Tags["GENRE"] != "Pop" || !info.Picture {
		t.Fatalf("tags %v %+v", err, info)
	}
	if f.badSigs.Load() != 0 {
		t.Fatalf("%d bad signatures", f.badSigs.Load())
	}
	if _, err := os.Stat(filepath.Join(card, ".qobuz", "covers")); f.covers.Load() != 1 || err == nil {
		t.Fatalf("cover fetched %d times; kept on the card: %v", f.covers.Load(), err == nil)
	}

	// A liked song already in the album's folder is copied, not fetched.
	files := f.files.Load()
	songs := spot(t, a, "songs", "recent")
	if r := spot(t, a, row(t, songs, "First")[5], row(t, songs, "First")[4]); r.message != "Downloading" { // held
		t.Fatalf("held on a liked song %+v", r)
	}
	until(t, "the copy", func() answer { return spot(t, a, "songs", "recent") },
		func(r answer) bool { return row(t, r, "First")[0] == "local" })
	if f.files.Load() != files {
		t.Error("fetched again")
	}

	// A playlist's files follow its order.
	spot(t, a, "getall", "playlist-555")
	until(t, "the playlist", func() answer { return spot(t, a, "playlist", "555") },
		func(r answer) bool { return len(r.rows) > 1 && r.rows[0][2] == "All on the Q2" })
	f.playlist = []string{"t1", "t2"}
	a.forget("playlist:555")
	pl := until(t, "the reordered playlist", func() answer { return spot(t, a, "playlist", "555") },
		func(r answer) bool { return r.message != "refresh" && len(r.rows) > 3 })
	if p := row(t, pl, "Song t1")[1]; !strings.HasSuffix(p, "0001 Song t1 [t1].flac") {
		t.Fatalf("renumbered %q", p)
	}

	// Held Play/Pause removes a download, the second hold confirming.
	if r := spot(t, a, "rm:album-A1:t1", "album"); !strings.HasPrefix(r.message, "Hold again") {
		t.Fatalf("%+v", r)
	}
	if r := spot(t, a, "rm:album-A1:t1", "album"); r.message != "Removed from the Q2" {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(want); !os.IsNotExist(err) {
		t.Fatal("not removed")
	}

	found := until(t, "a search", func() answer { return spot(t, a, "search", "third") }, func(r answer) bool { return r.message != "refresh" })
	if s := row(t, found, "Songs"); s[1] != "found" || s[4] != "tracks:third" {
		t.Fatalf("search %q", found.rows)
	}
	if third := row(t, spot(t, a, "found", "tracks:third"), "Third"); third[0] != "local" {
		t.Fatalf("found track %q", third) // already in its album's folder
	}

	// Settings: each quality opens its choices, MP3 the smallest; a press picks one, Selected after.
	set := spot(t, a, "settings")
	if sq := row(t, set, "Streaming Quality"); sq[0] != "list" || sq[1] != "pick" || sq[4] != "stream" || sq[3] != "CD (16-bit 44.1 kHz)" {
		t.Fatalf("settings %q", set.rows)
	}
	picks := spot(t, a, "pick", "stream")
	if strings.Join(titles(picks), "|") != "MP3 (320 kbps)|CD (16-bit 44.1 kHz)|Up to 24-bit 96 kHz|Up to 24-bit 192 kHz" ||
		row(t, picks, "CD (16-bit 44.1 kHz)")[3] != "Selected" {
		t.Fatalf("picks %q", picks.rows)
	}
	if r := spot(t, a, "set", "stream:5"); r.message != "Streaming in MP3 (320 kbps)" || a.settings().StreamQuality != qobuz.MP3 {
		t.Fatalf("%+v", r)
	}
	if row(t, spot(t, a, "pick", "stream"), "MP3 (320 kbps)")[3] != "Selected" {
		t.Fatal("MP3 not selected")
	}
	if r := spot(t, a, "set", "download:27"); r.message != "Downloads in Up to 24-bit 192 kHz" || a.settings().Quality != qobuz.HiRes192 {
		t.Fatalf("%+v", r)
	}
	if r := spot(t, a, "set", "stream:3"); r.status != "bad" {
		t.Fatalf("a quality Qobuz hasn't: %+v", r)
	}
	if !strings.HasPrefix(row(t, spot(t, a, "settings"), "Signed in as Matt")[3], "Studio · Hi-Res") {
		t.Fatal("account row")
	}

	// A token Qobuz no longer takes signs the Q2 out; downloads wait.
	f.dead.Store(true)
	spot(t, a, "search", "anything new") // asks Qobuz (a download here would be copied from another folder)
	until(t, "the sign-out", func() answer { return spot(t, a, "menu") },
		func(r answer) bool { return len(r.rows) > 0 && r.rows[0][2] == "Sign In from Your Phone" })
	if _, err := os.Stat(filepath.Join(private, "session.json")); !os.IsNotExist(err) {
		t.Fatal("the dead token kept")
	}
}

// A list too short for letters pages with More…, which sends only its cursor.
func TestAlbumsMore(t *testing.T) {
	f := newFake(t)
	f.albums = 50
	a, _, _ := startApp(t)
	signIn(t, a)
	first := until(t, "the albums", func() answer { return spot(t, a, "albums") }, func(r answer) bool { return r.message != "refresh" })
	if len(first.rows) != pageSize || first.cursor != "45" {
		t.Fatalf("%d rows, cursor %q", len(first.rows), first.cursor)
	}
	if more := spot(t, a, "albums", first.cursor); len(more.rows) != 5 || more.cursor != "" {
		t.Fatalf("More… %q", titles(more))
	}
}

// Downloaded lists page with More… rather than stop at a page's rows.
func TestDownloadedMore(t *testing.T) {
	newFake(t)
	a, card, _ := startApp(t)
	for i := 0; i < 60; i++ {
		dir := filepath.Join(card, ".qobuz", "cache", fmt.Sprintf("album-%d", i))
		os.MkdirAll(dir, 0o755)
		os.WriteFile(filepath.Join(dir, fmt.Sprintf("%02d T%d [t%d].flac", i, i, i)), []byte("x"), 0o644)
		os.WriteFile(filepath.Join(card, ".qobuz", "cache", "album-0", fmt.Sprintf("%02d S%d [s%d].mp3", i, i, i)), []byte("x"), 0o644)
	}
	if r := spot(t, a, "downloaded"); len(r.rows) != pageSize || spot(t, a, "downloaded", r.cursor).cursor != "" {
		t.Fatalf("%d rows, cursor %q", len(r.rows), r.cursor)
	}
	r := spot(t, a, "dctx", "album-0")
	if len(r.rows) != pageSize+1 || r.rows[0][1] != "rmctx" || r.rows[1][2] != "S0" || r.rows[2][2] != "T0" {
		t.Fatalf("%q", titles(r))
	}
	if more := spot(t, a, "dctx", "album-0", r.cursor); len(more.rows) != 61-pageSize || more.rows[0][1] == "rmctx" {
		t.Fatalf("More… %q", titles(more))
	}
}

func TestSettingsPersist(t *testing.T) {
	newFake(t)
	a, _, _ := startApp(t)
	spot(t, a, "set", "space:4096")
	if a.settings().LimitMB != 4096 {
		t.Fatal(a.settings().LimitMB)
	}
	s := loadSettings(a.opts.dir)
	if s.LimitMB != 4096 || s.Quality != qobuz.HiRes96 || len(s.PIN) != 6 {
		t.Fatalf("%+v", s)
	}
}

// Streaming: a press fills a session folder with the list from that track, in play order, each
// track whole and tagged before it appears; .play names it once its first track is there; the
// session keeps two tracks past the one Q2 Pod plays (.playing); a new press is a new session.
func TestStreaming(t *testing.T) {
	f := newFake(t)
	a, card, _ := startApp(t)
	signIn(t, a)
	page := until(t, "the album", func() answer { return spot(t, a, "album", "A9") }, func(r answer) bool { return r.message != "refresh" })
	second := row(t, page, "Second")
	if r := spot(t, a, second[1], second[4]); r.message != "Starting Second as soon as it is ready" {
		t.Fatalf("play %+v", r)
	}
	var seq, dir string
	until(t, "the session", func() answer { return spot(t, a, "menu") }, func(r answer) bool {
		b, err := os.ReadFile(a.opts.playFile)
		if err != nil {
			return false
		}
		seq, dir, _ = strings.Cut(strings.TrimSpace(string(b)), " ")
		_, err = os.Stat(filepath.Join(dir, "002 Third [t3].flac"))
		return err == nil
	})
	if !strings.HasPrefix(dir, filepath.Join(card, ".qobuz", "stream")+"/") || filepath.Base(dir) != seq {
		t.Fatalf("session %q %q", seq, dir)
	}
	ents, _ := os.ReadDir(dir)
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	if strings.Join(names, "|") != "001 Second [t2].flac|002 Third [t3].flac" {
		t.Fatalf("session files %q", names) // no .part left, no First: the list starts at the press
	}
	fh, _ := os.Open(filepath.Join(dir, "001 Second [t2].flac"))
	info, err := qobuz.ReadFLAC(fh)
	fh.Close()
	if err != nil || info.Tags["TITLE"] != "Second" || info.Tags["ALBUM"] != "Record" || !info.Picture {
		t.Fatalf("streamed track's tags: %v %+v", err, info)
	}
	// Q2 Pod plays it; nothing is downloaded (the cache stays empty).
	os.WriteFile(a.opts.playingFile, []byte(filepath.Join(dir, "001 Second [t2].flac")+"\n"), 0o644)
	if r := spot(t, a, "menu"); r.rows[0][2] != "Streaming Record" || r.rows[0][3] != "Track 1 of 2" {
		t.Fatalf("menu %q", r.rows[0])
	}
	if used, n := a.cache.Usage(); n != 0 || used != 0 {
		t.Fatalf("streaming filled the cache: %d files", n)
	}
	// A new press: a new session, the old one's folder kept while Q2 Pod still plays from it.
	first := row(t, spot(t, a, "album", "A9"), "First")
	spot(t, a, first[1], first[4])
	until(t, "the second session", func() answer { return spot(t, a, "menu") }, func(r answer) bool {
		b, _ := os.ReadFile(a.opts.playFile)
		return !strings.HasPrefix(string(b), seq+" ")
	})
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("the folder Q2 Pod plays from was swept")
	}
	if f.badSigs.Load() != 0 {
		t.Fatal("bad signatures")
	}
}

// Streamed as MP3: the session's files are .mp3, tagged with ID3 in place of their own tag.
func TestStreamingMP3(t *testing.T) {
	newFake(t)
	a, _, _ := startApp(t)
	signIn(t, a)
	spot(t, a, "set", "stream:5")
	page := until(t, "the album", func() answer { return spot(t, a, "album", "A7") }, func(r answer) bool { return r.message != "refresh" })
	third := row(t, page, "Third")
	if r := spot(t, a, third[1], third[4]); r.status != "ok" {
		t.Fatalf("press %q: %+v", third, r)
	}
	var p string
	defer func() {
		b, _ := os.ReadFile(a.opts.playFile)
		t.Logf(".play %q", b)
		filepath.Walk(filepath.Join(a.opts.dir, "stream"), func(p string, fi os.FileInfo, err error) error { t.Logf("  %s", p); return nil })
		if s := a.streamNow(); s != nil {
			t.Logf("stream next=%d err=%q failed=%d names=%q over=%v", s.next, s.err, s.failed, s.names, s.over)
		}
	}()
	until(t, "the MP3", func() answer { return spot(t, a, "menu") }, func(r answer) bool {
		b, _ := os.ReadFile(a.opts.playFile)
		if _, dir, ok := strings.Cut(strings.TrimSpace(string(b)), " "); ok {
			p = filepath.Join(dir, "001 Third [t3].mp3")
		}
		_, err := os.Stat(p)
		return err == nil
	})
	got, _ := os.ReadFile(p)
	tags, pic, err := qobuz.ReadID3(bytes.NewReader(got))
	if err != nil || tags["TIT2"] != "Third" || tags["TPE1"] != "The Band" || !pic || bytes.Contains(got, []byte("QQ")) {
		t.Fatalf("%v %v %v", err, tags, pic)
	}
}

// A long title is cut so the toast still says it will start.
func TestStartToast(t *testing.T) {
	if got := startToast("First"); got != "Starting First as soon as it is ready" {
		t.Fatal(got)
	}
	got := startToast(strings.Repeat("Très long ", 20))
	if len(got) > q2spot.MaxMessage || !strings.HasSuffix(got, "… as soon as it is ready") || !utf8.ValidString(got) {
		t.Fatalf("%d %q", len(got), got)
	}
}

// Ten wrong PINs lock the page, each lockout twice as long as the last; the right PIN starts again.
func TestPINLockout(t *testing.T) {
	newFake(t)
	a, _, _ := startApp(t)
	w := &web{a: a}
	try := func(pin string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"PIN":"`+pin+`"}`))
		r.Header.Set("X-Q2", "1")
		rec := httptest.NewRecorder()
		w.login(rec, r)
		return rec
	}
	wrong := "000000"
	if a.settings().PIN == wrong {
		wrong = "111111"
	}
	for _, want := range []string{"Wait 1 minute.", "Wait 2 minutes.", "Wait 4 minutes."} {
		for i := 0; i < 10; i++ {
			if c := try(wrong).Code; c != http.StatusForbidden {
				t.Fatal(want, i, c)
			}
		}
		if rec := try(a.settings().PIN); rec.Code != http.StatusTooManyRequests || !strings.Contains(rec.Body.String(), want) {
			t.Fatal(rec.Code, rec.Body.String())
		}
		w.mu.Lock()
		w.lockout = time.Time{} // the wait over
		w.mu.Unlock()
	}
	if try(a.settings().PIN).Code != http.StatusOK || w.locks != 0 {
		t.Fatal("the right PIN")
	}
}

// An album with a track Qobuz doesn't stream here is all on the Q2 once the others are.
func TestAlbumWithUnstreamable(t *testing.T) {
	newFake(t)
	a, _, _ := startApp(t)
	signIn(t, a)
	page := until(t, "the album", func() answer { return spot(t, a, "album", "A7") }, func(r answer) bool { return r.message != "refresh" })
	if r := page.rows[0]; r[2] != "Download Album" || !strings.HasPrefix(r[3], "3 tracks") {
		t.Fatalf("%q", r)
	}
	spot(t, a, "getall", "album-A7")
	page = until(t, "the album on the Q2", func() answer { return spot(t, a, "album", "A7") },
		func(r answer) bool { return len(r.rows) > 0 && r.rows[0][1] != "stopctx" && r.message != "refresh" })
	if r := page.rows[0]; r[2] != "All on the Q2" || !strings.Contains(r[3], "1 track Qobuz doesn't stream here") {
		t.Fatalf("%q", r)
	}
}

// Q2 Pod's Go to Album and Go to Artist on a Qobuz song (its addon_goto) open the song's album and
// artist from the track id in its name, whether or not the library has them; the setting turns that
// off, q2app's goto line following it.
func TestGoto(t *testing.T) {
	newFake(t)
	a, card, _ := startApp(t)
	manifest := filepath.Join(card, ".qobuz", "q2app")
	os.WriteFile(manifest, []byte("name=Qobuz\nsearch=Search Qobuz\nqr=0\ngoto=1\n"), 0o644)
	signIn(t, a)
	song := filepath.Join(card, ".qobuz", "stream", "1", "001 Second [t2].flac")
	album := until(t, "the album", func() answer { return spot(t, a, "goto", "album", song) }, func(r answer) bool { return r.message != "refresh" })
	if strings.Join(titles(album), "|") != "Download Album|First|Second|Third" {
		t.Fatalf("%q", titles(album))
	}
	artist := until(t, "the artist", func() answer { return spot(t, a, "goto", "artist", song) }, func(r answer) bool { return r.message != "refresh" })
	if strings.Join(titles(artist), "|") != "Not in the Library|Record" {
		t.Fatalf("%q", titles(artist))
	}
	if r := spot(t, a, "goto", "album", "/mnt/mmc/Music/x.flac"); r.status != "bad" {
		t.Fatalf("%+v", r)
	}
	if row(t, spot(t, a, "settings"), "Go to Qobuz Pages: On")[4] != "goto:0" {
		t.Fatal("settings row")
	}
	if r := spot(t, a, "set", "goto:0"); r.message != "Go to opens your Library's pages" || !a.settings().GotoLibrary {
		t.Fatalf("%+v", r)
	}
	if b, _ := os.ReadFile(manifest); string(b) != "name=Qobuz\nsearch=Search Qobuz\nqr=0\ngoto=0\n" {
		t.Fatalf("%q", b)
	}
	spot(t, a, "set", "goto:1")
	if b, _ := os.ReadFile(manifest); !strings.HasSuffix(string(b), "\ngoto=1\n") {
		t.Fatalf("%q", b)
	}
}

// A session keeps two tracks past the one playing, not counting those left out.
func TestStreamAhead(t *testing.T) {
	s := &stream{list: make([]entry, 8), left: []bool{false, true, true, false, false, true, false, false}, playing: -1}
	for _, c := range []struct{ playing, want int }{{-1, 4}, {0, 4}, {3, 6}, {4, 7}, {6, 7}, {7, 7}} {
		if s.playing = c.playing; s.ahead() != c.want {
			t.Errorf("playing %d: ahead to %d, want %d", c.playing, s.ahead(), c.want)
		}
	}
}

// A session isn't idle while a long track plays: its length counts.
func TestStreamIdle(t *testing.T) {
	s := &stream{list: []entry{{T: qobuz.Track{Duration: 3600}}}, playing: 0, seen: time.Now().Add(-50 * time.Minute)}
	if s.idle() > streamIdle {
		t.Fatal("an hour-long track ended the session after 50 minutes")
	}
	s.seen = time.Now().Add(-2 * time.Hour)
	if s.idle() <= streamIdle {
		t.Fatal("never idle")
	}
}

// queue.json keeps each track's album without the album's tracks, so it grows with an album's
// tracks, not their square.
func TestQueueLinear(t *testing.T) {
	newFake(t)
	a, _, _ := startApp(t)
	signIn(t, a)
	_, es, err := a.await("album-A100")
	if err != nil || len(es) != 100 {
		t.Fatal(err, len(es))
	}
	var jobs []dl.Job
	for _, e := range es {
		jobs = append(jobs, dl.Job{Ctx: "album-A100", Name: e.Name, Track: e.T})
	}
	dir := t.TempDir()
	d := dl.New(dl.Options{Dir: dir, Cache: a.cache}) // not started: the queue stays whole
	if n := d.Queue(jobs...); n != 100 {
		t.Fatal(n)
	}
	st, err := os.Stat(filepath.Join(dir, "queue.json"))
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() > 100*2<<10 {
		t.Fatalf("queue.json is %d bytes for 100 tracks", st.Size())
	}
	if al := d.Jobs()[99].Track.Album; al == nil || al.Title != "Record" || al.TracksCount != 100 || al.Tracks != nil {
		t.Fatalf("%+v", al)
	}
}

// Tracks Qobuz doesn't stream are left out at once, and two in a row don't stop the session: it
// still keeps two tracks ahead. One it doesn't stream but the Q2 has is copied in.
func TestStreamTwoLeftOut(t *testing.T) {
	newFake(t)
	a, card, _ := startApp(t)
	signIn(t, a)
	had := filepath.Join(card, ".qobuz", "cache", "album-A8", "05 Seventh [t7].flac")
	os.MkdirAll(filepath.Dir(had), 0o755)
	os.WriteFile(had, flacFile("t7"), 0o644)
	page := until(t, "the album", func() answer { return spot(t, a, "album", "A8") }, func(r answer) bool { return r.message != "refresh" })
	first := row(t, page, "First")
	spot(t, a, first[1], first[4])
	var dir string
	until(t, "the tracks after those left out", func() answer { return spot(t, a, "menu") }, func(r answer) bool {
		b, err := os.ReadFile(a.opts.playFile)
		if err != nil {
			return false
		}
		_, dir, _ = strings.Cut(strings.TrimSpace(string(b)), " ")
		_, err = os.Stat(filepath.Join(dir, "005 Seventh [t7].flac"))
		return err == nil
	})
	ents, _ := os.ReadDir(dir)
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	if strings.Join(names, "|") != "001 First [t1].flac|004 Second [t2].flac|005 Seventh [t7].flac" {
		t.Fatalf("session files %q", names)
	}
	if r := spot(t, a, "menu"); !strings.HasSuffix(r.rows[0][3], "2 tracks left out") {
		t.Fatalf("menu %q", r.rows[0])
	}
}

// A track Qobuz answers without a file is one it won't send: the web player isn't read again, the
// secret is kept, and its download fails without trying again.
func TestNoURL(t *testing.T) {
	f := newFake(t)
	a, _, _ := startApp(t)
	signIn(t, a)
	waitFor(t, "the secret", func() bool { return a.account().Secret != "" })
	f.noFile.Store("t6", true)
	logins := f.logins.Load()
	_, err := a.File(context.Background(), "t6", qobuz.HiRes96)
	var qe *qobuz.Error
	if !errors.As(err, &qe) || qe.Status != 404 || f.logins.Load() != logins || f.noFiles.Load() != 1 ||
		f.badSigs.Load() != 0 || a.account().Secret == "" {
		t.Fatalf("%v; web player read %d times, asked %d times", err, f.logins.Load()-logins, f.noFiles.Load())
	}
	a.dl.Queue(dl.Job{Ctx: "album-A8", Name: "03 Sixth [t6].flac", Track: qobuz.Track{ID: "t6", Title: "Sixth", Album: &qobuz.Album{ID: "A8"}}})
	waitFor(t, "the failed download", func() bool { j := a.dl.Job("album-A8", "t6"); return j != nil && j.State == dl.Failed })
	if j := a.dl.Job("album-A8", "t6"); f.noFiles.Load() != 2 || strings.Contains(j.Err, "reach") {
		t.Fatalf("asked %d times: %q", f.noFiles.Load(), j.Err)
	}
}

// The secret is found on the newest liked song though Qobuz no longer sends it: a file request
// answered is a signature taken.
func TestProbeUnstreamable(t *testing.T) {
	f := newFake(t)
	a, _, _ := startApp(t)
	signIn(t, a)
	until(t, "the liked songs", func() answer { return spot(t, a, "songs", "recent") },
		func(r answer) bool { return r.message != "refresh" && len(r.rows) > 0 })
	waitFor(t, "the secret", func() bool { return a.account().Secret != "" })
	f.noFile.Store("t1", true) // the newest liked song
	s := a.account()
	s.Secret = ""
	a.saveSession(&s) // as after Qobuz ships a new web player
	if _, err := a.File(context.Background(), "t2", qobuz.HiRes96); err != nil || a.account().Secret == "" || f.noFiles.Load() == 0 {
		t.Fatal(err)
	}
}

// An app id Qobuz has retired, kept from an older web player, is read again once online, so a
// sign-in starts with the new one.
func TestRetiredAppID(t *testing.T) {
	newFake(t)
	a, _, _ := startApp(t, func(dir, private string) {
		writeJSON(filepath.Join(dir, "creds.json"), qobuz.Creds{Bundle: "/resources/9.0.0-x000/bundle.js", AppID: "111111111",
			PrivateKey: "pk0", Secrets: map[string]string{"London": "old"}})
	})
	waitFor(t, "the web player read again", func() bool {
		c, err := a.appCreds(context.Background())
		return err == nil && c.AppID == "123456789"
	})
	signIn(t, a)
}

// A setting config.json can't take says so, and stays as it was.
func TestSettingsUnsaved(t *testing.T) {
	newFake(t)
	a, _, _ := startApp(t)
	cfg := filepath.Join(a.opts.dir, "config.json")
	os.Remove(cfg)
	os.MkdirAll(filepath.Join(cfg, "x"), 0o755) // nothing is renamed onto it
	for _, set := range []string{"space:4096", "stream:5", "goto:0"} {
		if r := spot(t, a, "set", set); r.status != q2spot.Error {
			t.Fatalf("%s: %+v", set, r)
		}
	}
	if s := a.settings(); s.LimitMB != 2048 || s.StreamQuality != qobuz.CD || s.GotoLibrary {
		t.Fatalf("%+v", s)
	}
}

// Firmware that doesn't send Go to (no "goto" in its caps file, or no file: older Q2 Pod) gets no
// Go to setting.
func TestGotoNeedsFirmware(t *testing.T) {
	newFake(t)
	a, _, _ := startApp(t)
	for _, caps := range []string{"playlist progress pages library\n", ""} {
		os.Remove(q2.CapsFile)
		if caps != "" {
			os.WriteFile(q2.CapsFile, []byte(caps), 0o644)
		}
		for _, title := range titles(spot(t, a, "settings")) {
			if strings.HasPrefix(title, "Go to Qobuz Pages") {
				t.Fatalf("caps %q: %s offered", caps, title)
			}
		}
	}
}
