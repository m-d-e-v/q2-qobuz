// qprobe tries Qobuz from a computer before any of it goes on the Q2: the sign-in the Q2 would use
// (Qobuz's page, opened on a phone, returning to this machine's Wi-Fi address), the library calls,
// and one track's file, downloaded and inspected.
//
//	qprobe login           sign in; scan the code with your phone
//	qprobe check           the account, the signing secret and the library's counts, every page read
//	qprobe get [track-id]  what each format gives for a track, then the best one downloaded and read
//	qprobe logout          forget the sign-in
//
// The sign-in is kept in ~/.config/q2-qobuz/session.json (yours alone to read). Nothing here prints
// the token or a file's address.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"q2-qobuz/internal/qobuz"
	"q2-qobuz/internal/qr"
	"q2-qobuz/internal/tlsroots"
)

type session struct {
	Token  string `json:"token"`
	UserID int64  `json:"user_id"`
	At     string `json:"signed_in"`
}

var (
	home, _  = os.UserHomeDir()
	stateDir = filepath.Join(home, ".config", "q2-qobuz")
	httpc    = &http.Client{Transport: tlsroots.Transport(), Timeout: 60 * time.Second}
)

func fail(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "qprobe: "+format+"\n", a...)
	os.Exit(1)
}

func step(format string, a ...any) { fmt.Printf("• "+format+"\n", a...) }

func creds(ctx context.Context) *qobuz.Creds {
	var cached *qobuz.Creds
	if b, err := os.ReadFile(filepath.Join(stateDir, "creds.json")); err == nil {
		json.Unmarshal(b, &cached)
	}
	c, err := qobuz.FetchCreds(ctx, httpc, cached)
	if err != nil {
		if cached != nil {
			step("Couldn't read the web player (%v); using the app id read before", err)
			return cached
		}
		fail("%v", err)
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	os.MkdirAll(stateDir, 0o700)
	os.WriteFile(filepath.Join(stateDir, "creds.json"), b, 0o600)
	return c
}

func client(ctx context.Context) *qobuz.Client {
	b, err := os.ReadFile(filepath.Join(stateDir, "session.json"))
	if err != nil {
		fail("not signed in: run qprobe login")
	}
	var s session
	if json.Unmarshal(b, &s) != nil || s.Token == "" {
		fail("the saved sign-in is unreadable: run qprobe login")
	}
	return &qobuz.Client{HTTP: httpc, Creds: creds(ctx), Token: s.Token, UserID: s.UserID}
}

// lanIP is this machine's Wi-Fi address: the first IPv4 address of an interface that is up and not
// the loopback, en0 (a Mac's Wi-Fi) first.
func lanIP() string {
	ifs, _ := net.Interfaces()
	sort.SliceStable(ifs, func(i, j int) bool { return ifs[i].Name == "en0" && ifs[j].Name != "en0" })
	for _, i := range ifs {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil && !n.IP.IsLinkLocalUnicast() {
				return n.IP.String()
			}
		}
	}
	return ""
}

// printQR draws a QR code in the terminal, two modules a character, light on dark.
func printQR(s string) {
	c, err := qr.Encode([]byte(s))
	if err != nil {
		return
	}
	dark := func(x, y int) bool { return x >= 0 && y >= 0 && x < c.Size && y < c.Size && c.Modules[y][x] }
	for y := -2; y < c.Size+2; y += 2 {
		var b strings.Builder
		b.WriteString("  ")
		for x := -2; x < c.Size+2; x++ {
			top, bottom := !dark(x, y), !dark(x, y+1) // light modules drawn
			switch {
			case top && bottom:
				b.WriteString("█")
			case top:
				b.WriteString("▀")
			case bottom:
				b.WriteString("▄")
			default:
				b.WriteString(" ")
			}
		}
		fmt.Println(b.String())
	}
}

func login(ctx context.Context) {
	cr := creds(ctx)
	step("Qobuz's web player %s: app id read, %d candidate secrets, sign-in key %v",
		strings.TrimSuffix(strings.TrimPrefix(cr.Bundle, "/resources/"), "/bundle.js"), len(cr.Secrets), cr.PrivateKey != "")
	ip := lanIP()
	if ip == "" {
		fail("no Wi-Fi address on this Mac")
	}
	ln, err := net.Listen("tcp4", ":0")
	if err != nil {
		fail("%v", err)
	}
	var nonce [6]byte
	rand.Read(nonce[:])
	path := "/q2-" + hex.EncodeToString(nonce[:])
	redirect := fmt.Sprintf("http://%s:%d%s", ip, ln.Addr().(*net.TCPAddr).Port, path)
	codes := make(chan string, 2)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code_autorisation")
		if code == "" {
			code = r.URL.Query().Get("code")
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if code == "" {
			fmt.Fprint(w, "<h2>No sign-in code came back.</h2>")
			step("Your browser came back to %s without a code (query keys: %v)", r.URL.Path, keys(r.URL.Query()))
			return
		}
		if r.URL.Path != path {
			step("Qobuz returned to %q, not the path asked for: a Q2 must accept any path", r.URL.Path)
		}
		fmt.Fprint(w, `<meta name="viewport" content="width=device-width"><h2>Signed in.</h2><p>You can close this page.</p>`)
		codes <- code
	})}
	go srv.Serve(ln)
	url := qobuz.SignInURL(cr.AppID, redirect)
	fmt.Println()
	fmt.Println("Scan this with your phone (on the same Wi-Fi as this Mac) and sign in to Qobuz there:")
	printQR(url)
	fmt.Println("Or press Enter to open it in this Mac's browser instead.")
	fmt.Println("If the page after signing in won't load, paste its whole address here and press Enter.")
	fmt.Println()
	go func() {
		in := bufio.NewScanner(os.Stdin)
		for in.Scan() {
			line := strings.TrimSpace(in.Text())
			if line == "" {
				exec.Command("open", url).Start()
				continue
			}
			if i := strings.Index(line, "code_autorisation="); i >= 0 {
				line = line[i+len("code_autorisation="):]
				if j := strings.IndexAny(line, "&#"); j >= 0 {
					line = line[:j]
				}
			}
			codes <- line
		}
	}()
	var code string
	select {
	case code = <-codes:
	case <-time.After(10 * time.Minute):
		fail("no sign-in within 10 minutes")
	}
	srv.Close()
	c := &qobuz.Client{HTTP: httpc, Creds: cr}
	if err := c.Exchange(ctx, code); err != nil {
		fail("Qobuz didn't take the sign-in: %v", err)
	}
	b, _ := json.MarshalIndent(session{Token: c.Token, UserID: c.UserID, At: time.Now().Format(time.RFC3339)}, "", "  ")
	os.MkdirAll(stateDir, 0o700)
	if err := os.WriteFile(filepath.Join(stateDir, "session.json"), b, 0o600); err != nil {
		fail("%v", err)
	}
	step("Signed in (account …%d). The sign-in returned to this Mac's Wi-Fi address, as a Q2's would.", c.UserID%1000)
}

func keys(v map[string][]string) []string {
	var out []string
	for k := range v {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// pages reads every page of a list, 100 at a time, checking the count against the total.
func pages(name string, fetch func(limit, offset int) (int, int, error)) int {
	got, total, calls := 0, -1, 0
	for {
		n, t, err := fetch(100, got)
		calls++
		if err != nil {
			step("%s: %v", name, err)
			return got
		}
		total = t
		got += n
		if n == 0 || got >= total || calls > 200 {
			break
		}
	}
	mark := "all read"
	if got != total {
		mark = fmt.Sprintf("MISMATCH: read %d", got)
	}
	step("%-18s %5d (%s, %d pages)", name, total, mark, calls)
	return total
}

func firstTrack(ctx context.Context, c *qobuz.Client) (qobuz.Track, error) {
	if p, err := c.FavoriteTracks(ctx, 1, 0); err == nil && len(p.Items) > 0 {
		return p.Items[0], nil
	}
	p, err := c.FavoriteAlbums(ctx, 1, 0)
	if err != nil {
		return qobuz.Track{}, err
	}
	if len(p.Items) == 0 {
		return qobuz.Track{}, errors.New("no favourite tracks or albums to try")
	}
	a, err := c.Album(ctx, p.Items[0].ID.String())
	if err != nil || a.Tracks == nil || len(a.Tracks.Items) == 0 {
		return qobuz.Track{}, fmt.Errorf("album %s: %v", p.Items[0].ID, err)
	}
	t := a.Tracks.Items[0]
	t.Album = &a
	return t, nil
}

func check(ctx context.Context) {
	c := client(ctx)
	if u, err := c.User(ctx); err == nil {
		step("Plan: %s (hi-res streaming %v, lossless %v)", u.Credential.Label, u.Credential.Parameters.HiRes, u.Credential.Parameters.Lossless)
	} else {
		step("Account: %v", err)
	}
	step("Library (every page read, 100 at a time):")
	pages("favourite albums", func(limit, offset int) (int, int, error) {
		p, err := c.FavoriteAlbums(ctx, limit, offset)
		return len(p.Items), p.Total, err
	})
	pages("favourite tracks", func(limit, offset int) (int, int, error) {
		p, err := c.FavoriteTracks(ctx, limit, offset)
		return len(p.Items), p.Total, err
	})
	pages("favourite artists", func(limit, offset int) (int, int, error) {
		p, err := c.FavoriteArtists(ctx, limit, offset)
		return len(p.Items), p.Total, err
	})
	var lists []qobuz.Playlist
	pages("playlists", func(limit, offset int) (int, int, error) {
		p, err := c.Playlists(ctx, limit, offset)
		lists = append(lists, p.Items...)
		return len(p.Items), p.Total, err
	})
	own := 0
	for _, p := range lists {
		if p.Owner.ID.String() == fmt.Sprint(c.UserID) {
			own++
		}
	}
	step("  %d your own, %d followed", own, len(lists)-own)
	if len(lists) > 0 {
		p := lists[0]
		pages(fmt.Sprintf("first playlist's tracks (%d)", p.TracksCount), func(limit, offset int) (int, int, error) {
			got, err := c.PlaylistTracks(ctx, p.ID.String(), limit, offset)
			if got.Tracks == nil {
				return 0, 0, err
			}
			return len(got.Tracks.Items), got.Tracks.Total, err
		})
	}
	t, err := firstTrack(ctx, c)
	if err != nil {
		fail("%v", err)
	}
	secret, err := c.FindSecret(ctx, t.ID.String())
	if err != nil {
		fail("%v", err)
	}
	for z, s := range c.Creds.Secrets {
		if s == secret {
			step("File requests are signed with the %s secret", z)
		}
	}
}

func get(ctx context.Context, arg string) {
	c := client(ctx)
	var t qobuz.Track
	var err error
	if arg != "" {
		t.ID, t.Title = qobuz.ID(arg), "track "+arg
	} else if t, err = firstTrack(ctx, c); err != nil {
		fail("%v", err)
	}
	album := ""
	if t.Album != nil {
		album = " · " + t.Album.Title
	}
	step("Track: %s — %s%s (%d:%02d long in the catalogue; best master %d-bit %g kHz)", t.Title, t.Performer.Name, album,
		t.Duration/60, t.Duration%60, t.MaximumBitDepth, t.MaximumRate)
	secret, err := c.FindSecret(ctx, t.ID.String())
	if err != nil {
		fail("%v", err)
	}
	var best *qobuz.File
	for _, f := range []int{qobuz.MP3, qobuz.CD, qobuz.HiRes96, qobuz.HiRes192} {
		file, err := c.FileURL(ctx, t.ID.String(), f, secret)
		if err != nil {
			step("format %2d: %v", f, err)
			continue
		}
		var r []string
		for _, x := range file.Restrictions {
			r = append(r, x.Code)
		}
		step("format %2d asked: got %2d, %s, %d-bit %g kHz, preview only: %v %s", f, file.FormatID, file.MimeType,
			file.BitDepth, file.SamplingRate, file.Sample, strings.Join(r, ","))
		best = file
	}
	if best == nil {
		fail("no format was given")
	}
	dir := filepath.Join("probe-downloads")
	os.MkdirAll(dir, 0o755)
	out := filepath.Join(dir, fmt.Sprintf("%s-%d.flac", t.ID, best.FormatID))
	start := time.Now()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, best.URL, nil)
	resp, err := (&http.Client{Transport: tlsroots.Transport()}).Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) { // without the file's address
			err = ue.Err
		}
		fail("download: %v", err)
	}
	defer resp.Body.Close()
	first := time.Since(start)
	f, err := os.Create(out)
	if err != nil {
		fail("%v", err)
	}
	n, err := io.Copy(f, resp.Body)
	f.Close()
	took := time.Since(start)
	if err != nil {
		fail("download: %v after %d bytes", err, n)
	}
	step("Downloaded %.1f MB in %.1f s (%.1f MB/s; first byte after %d ms; HTTP %d, ranges: %q) to %s",
		float64(n)/1e6, took.Seconds(), float64(n)/1e6/took.Seconds(), first.Milliseconds(), resp.StatusCode,
		resp.Header.Get("Accept-Ranges"), out)
	rf, _ := os.Open(out)
	info, err := qobuz.ReadFLAC(rf)
	rf.Close()
	if err != nil {
		step("Reading it: %v (Content-Type %s)", err, resp.Header.Get("Content-Type"))
		return
	}
	step("FLAC: %d-bit %d Hz, %d channels, %d:%02d long", info.Bits, info.Rate, info.Channels,
		info.Samples/int64(info.Rate)/60, info.Samples/int64(info.Rate)%60)
	var names []string
	for k := range info.Tags {
		names = append(names, k)
	}
	sort.Strings(names)
	step("Tags (%s): %s", info.Vendor, strings.Join(names, ", "))
	for _, k := range []string{"TITLE", "ARTIST", "ALBUM", "ALBUMARTIST", "TRACKNUMBER", "DISCNUMBER", "DATE"} {
		if v, ok := info.Tags[k]; ok {
			step("  %-12s %s", k, v)
		}
	}
	step("Embedded cover: %v (%d KB)", info.Picture, info.PictureBytes/1024)
}

func main() {
	ctx := context.Background()
	if len(os.Args) < 2 {
		fmt.Println("usage: qprobe login | check | get [track-id] | logout")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "login":
		login(ctx)
	case "check":
		check(ctx)
	case "get":
		arg := ""
		if len(os.Args) > 2 {
			arg = os.Args[2]
		}
		get(ctx, arg)
	case "logout":
		os.Remove(filepath.Join(stateDir, "session.json"))
		step("Signed out on this Mac")
	default:
		fail("unknown command %q", os.Args[1])
	}
}
