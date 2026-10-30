package main

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"q2-qobuz/internal/cache"
	"q2-qobuz/internal/dl"
	"q2-qobuz/internal/q2"
	"q2-qobuz/internal/q2spot"
	"q2-qobuz/internal/qobuz"
)

// The Q2's Qobuz page (Q2 Pod's card app page, q2-pod's patch/addon.c). A row's kind says what a
// press does: list opens the level its uri names with its context as the argument, action sends its
// uri with its context and toasts the reply, local plays a downloaded track and its folder on from
// it (context album: in name order, which is the list's order), search opens the keyboard, remote
// the QR code, info nothing. A row's position is what a held Play/Pause sends. A reply whose message
// is "refresh" is asked again every 2 s while on screen.
//
// Levels: menu; albums, artists [letter]; songs [recent|letter] [cursor]; playlists; album <id>;
// artist <id>; playlist <id> [cursor]; search <term>; found <kind>:<term>; downloaded; dctx <folder>;
// downloads; settings; pick stream|download|space (a setting's choices). Actions: play <list>:<track>
// streams the list from the track (stream.go); get, cancel <folder>:<track>; getall, stopctx, rmctx
// (twice) <folder>; set <setting>:<value>; signout (twice); retryall; cancelall; stop. Held Play/Pause, as on Q2 Pod's other streaming services:
// dl:<folder> downloads a list, dl:<folder>:<track> a track, rm:<folder>:<track> (twice) removes a
// downloaded one.
//
// Folders, where downloads go (cache's layout): album-<id>, playlist-<id> and liked. Lists, what a
// press streams through: a folder's, liked-recent and liked-<letter> (Songs' pages) and
// found:<term> (a search's songs).

const (
	pageSize    = 45
	lettersOver = 60 // a longer list opens on its letters
	confirmIn   = 5 * time.Second
)

func info(title, sub string) q2spot.Row { return q2spot.Row{Kind: "info", Title: title, Subtitle: sub} }

func list(op, arg, title, sub string) q2spot.Row {
	return q2spot.Row{Kind: "list", URI: op, Context: arg, Title: title, Subtitle: sub}
}

func act(op, arg, title, sub string) q2spot.Row {
	return q2spot.Row{Kind: "action", URI: op, Context: arg, Title: title, Subtitle: sub}
}

func join(parts ...string) string {
	var out []string
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, " · ")
}

func plural(n int, one string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %ss", n, one)
}

func say(err error) string {
	s := err.Error()
	r, n := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(r)) + s[n:]
}

func size(b int64) string {
	switch {
	case b <= 0:
		return ""
	case b < 1<<20:
		return fmt.Sprintf("%d KB", (b+1023)>>10)
	case b < 1<<30:
		return fmt.Sprintf("%d MB", (b+1<<19)>>20)
	}
	return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
}

func clock(sec int) string {
	if sec <= 0 {
		return ""
	}
	if sec >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", sec/3600, sec/60%60, sec%60)
	}
	return fmt.Sprintf("%d:%02d", sec/60, sec%60)
}

func year(date string) string {
	if len(date) >= 4 {
		return date[:4]
	}
	return ""
}

// qualities are Qobuz's four formats, smallest first: MP3 320 kbit/s is its lowest.
var qualities = []int{qobuz.MP3, qobuz.CD, qobuz.HiRes96, qobuz.HiRes192}

func validQuality(q int) bool {
	for _, x := range qualities {
		if q == x {
			return true
		}
	}
	return false
}

func qualityName(q int) string {
	switch q {
	case qobuz.MP3:
		return "MP3 (320 kbps)"
	case qobuz.CD:
		return "CD (16-bit 44.1 kHz)"
	case qobuz.HiRes192:
		return "Up to 24-bit 192 kHz"
	}
	return "Up to 24-bit 96 kHz"
}

// qualityNote is what a quality costs: a four-minute track's size, and what it is good for.
func qualityNote(q int) string {
	switch q {
	case qobuz.MP3:
		return "About 10 MB a track · starts soonest"
	case qobuz.CD:
		return "About 30 MB a track · lossless"
	case qobuz.HiRes192:
		return "About 200 MB a track · the best there is"
	}
	return "About 100 MB a track · hi-res"
}

// format is what a track downloads as at quality q: its master's, capped (MP3: none, 0).
func format(t qobuz.Track, q int) (bits int, khz float64) {
	bits, khz = t.MaximumBitDepth, t.MaximumRate
	if bits == 0 {
		bits, khz = 16, 44.1
	}
	switch q {
	case qobuz.MP3:
		return 0, 0
	case qobuz.CD:
		return 16, min(khz, 44.1)
	case qobuz.HiRes96:
		return bits, min(khz, 96)
	}
	return bits, khz
}

// estimate is a track's size at quality q: 320 kbit/s as MP3, else its PCM at about 60% as FLAC.
func estimate(t qobuz.Track, q int) int64 {
	if q == qobuz.MP3 {
		return int64(t.Duration) * 320000 / 8
	}
	bits, khz := format(t, q)
	return int64(float64(t.Duration) * khz * 1000 * float64(bits) * 2 / 8 * 0.6)
}

func hiRes(t qobuz.Track, q int) string {
	if bits, khz := format(t, q); bits > 16 || khz > 48 {
		return fmt.Sprintf("%d/%g", bits, khz)
	}
	return ""
}

// sortKey orders names as the Q2's Library does: "The " left off, case aside.
func sortKey(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 4 && strings.EqualFold(s[:4], "the ") {
		s = s[4:]
	}
	return strings.ToLower(s)
}

var folds = map[rune]rune{'À': 'A', 'Á': 'A', 'Â': 'A', 'Ã': 'A', 'Ä': 'A', 'Å': 'A', 'Ç': 'C', 'È': 'E', 'É': 'E',
	'Ê': 'E', 'Ë': 'E', 'Ì': 'I', 'Í': 'I', 'Î': 'I', 'Ï': 'I', 'Ñ': 'N', 'Ò': 'O', 'Ó': 'O', 'Ô': 'O', 'Õ': 'O', 'Ö': 'O',
	'Ø': 'O', 'Ù': 'U', 'Ú': 'U', 'Û': 'U', 'Ü': 'U', 'Ý': 'Y'}

// letter is a name's index letter, A to Z, or # for anything else.
func letter(s string) string {
	r, _ := utf8.DecodeRuneInString(sortKey(s))
	r = unicode.ToUpper(r)
	if f, ok := folds[r]; ok {
		r = f
	}
	if r >= 'A' && r <= 'Z' {
		return string(r)
	}
	return "#"
}

// letterRows is A to Z (then #) with how many names start with each, as list rows for op.
func letterRows(op string, names []string, unit string) []q2spot.Row {
	count := map[string]int{}
	for _, n := range names {
		count[letter(n)]++
	}
	var rows []q2spot.Row
	for _, l := range strings.Split("A B C D E F G H I J K L M N O P Q R S T U V W X Y Z #", " ") {
		if count[l] > 0 {
			rows = append(rows, list(op, l, l, plural(count[l], unit)))
		}
	}
	return rows
}

// cursor is a paged level's offset: its last argument when it has more than skip.
func cursor(req q2spot.Request, skip int) int {
	if len(req.Args) <= skip {
		return 0
	}
	n, _ := strconv.Atoi(req.Args[len(req.Args)-1])
	return max(n, 0)
}

// lettered is a list's letter and cursor: More… on a list too short for letters sends only the cursor.
func lettered(req q2spot.Request) (string, int) {
	if len(req.Args) == 1 {
		if n, err := strconv.Atoi(req.Args[0]); err == nil {
			return "", max(n, 0)
		}
	}
	return req.Arg(0), cursor(req, 1)
}

// paged is rows[off:] up to pageSize, and the cursor for More….
func paged[T any](rows []T, off int) ([]T, string) {
	if off >= len(rows) {
		return nil, ""
	}
	end := min(off+pageSize, len(rows))
	if end < len(rows) {
		return rows[off:end], strconv.Itoa(end)
	}
	return rows[off:], ""
}

func live(rows []q2spot.Row, cur string, on bool) q2spot.Reply {
	r := q2spot.Reply{Rows: rows, Cursor: cur}
	if on {
		r.Message = "refresh"
	}
	return r
}

func loadingReply(what string) q2spot.Reply {
	return live([]q2spot.Row{info("Loading…", what)}, "", true)
}

func (a *app) confirm(what string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if at, ok := a.armed[what]; ok && time.Since(at) < confirmIn {
		delete(a.armed, what)
		return true
	}
	a.armed[what] = time.Now()
	return false
}

func (a *app) isArmed(what string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	at, ok := a.armed[what]
	return ok && time.Since(at) < confirmIn
}

// entry is a track in a folder's list, with its file's name there.
type entry struct {
	T    qobuz.Track
	Name string
}

// tracksOf is a folder's tracks in play order and what it is called; done false while fetching.
// A track listed twice (a playlist's repeat) is kept once.
func (a *app) tracksOf(ctx string) (label string, es []entry, err error, done bool) {
	seen := map[string]bool{}
	add := func(t qobuz.Track, name string) {
		if id := t.ID.String(); !seen[id] {
			seen[id] = true
			es = append(es, entry{t, name})
		}
	}
	switch {
	case strings.HasPrefix(ctx, "liked-"):
		_, all, err, done := a.tracksOf("liked")
		which := strings.TrimPrefix(ctx, "liked-")
		label := "Songs: " + which
		if which == "recent" {
			label = "Recently Liked"
		}
		return label, likedSongs(all, which), err, done
	case strings.HasPrefix(ctx, "found:"): // a search's songs
		term := strings.TrimPrefix(ctx, "found:")
		f, err, done := a.search(term)
		if !done || err != nil {
			return "", nil, err, done
		}
		for _, t := range f.Tracks.Items {
			if t.Album != nil {
				add(t, "")
			}
		}
		return "“" + term + "”", es, nil, true
	case ctx == "liked":
		l, busy, lerr := a.listOf("tracks", false)
		if len(l.Tracks) == 0 && (busy || lerr != nil) {
			return "Liked Songs", nil, lerr, !busy
		}
		for _, t := range l.Tracks {
			add(t, cache.Name(cache.LikedPrefix(t.FavoritedAt), qobuz.FullTitle(t.Title, t.Version), t.ID.String()))
		}
		return "Liked Songs", es, nil, true
	case strings.HasPrefix(ctx, "album-"):
		al, err, done := a.album(strings.TrimPrefix(ctx, "album-"))
		if !done || err != nil {
			return "", nil, err, done
		}
		if al.Tracks != nil {
			lite := *al // each track's album, for its tags, without the album's tracks (queue.json keeps each)
			lite.Tracks = nil
			for _, t := range al.Tracks.Items {
				t.Album = &lite
				add(t, cache.Name(cache.AlbumPrefix(t.MediaNumber, t.TrackNumber, al.MediaCount, al.TracksCount),
					qobuz.FullTitle(t.Title, t.Version), t.ID.String()))
			}
		}
		return qobuz.FullTitle(al.Title, al.Version), es, nil, true
	case strings.HasPrefix(ctx, "playlist-"):
		p, err, done := a.playlist(strings.TrimPrefix(ctx, "playlist-"))
		if !done || err != nil {
			return "", nil, err, done
		}
		if p.Tracks != nil {
			for i, t := range p.Tracks.Items {
				add(t, cache.Name(cache.PlaylistPrefix(i+1), qobuz.FullTitle(t.Title, t.Version), t.ID.String()))
			}
		}
		return p.Name, es, nil, true
	}
	return "", nil, fmt.Errorf("no list %q", ctx), true
}

// likedSongs is a Songs page's tracks, from all the liked ones: the newest 200, or a letter's by title.
func likedSongs(all []entry, which string) []entry {
	if which == "recent" {
		return all[:min(len(all), 200)]
	}
	var es []entry
	for _, e := range all {
		if letter(e.T.Title) == which {
			es = append(es, e)
		}
	}
	sort.SliceStable(es, func(i, j int) bool { return sortKey(es[i].T.Title) < sortKey(es[j].T.Title) })
	return es
}

// await is tracksOf, waiting up to 12 s for a fetch (a held Play/Pause wants an answer).
func (a *app) await(ctx string) (string, []entry, error) {
	for end := time.Now().Add(12 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		label, es, err, done := a.tracksOf(ctx)
		if done || time.Now().After(end) {
			if !done {
				err = fmt.Errorf("Qobuz is slow to answer: try again")
			}
			return label, es, err
		}
	}
}

// trackRow is a track of a list: downloaded, a press plays it from the card at once; otherwise a press
// streams the list from it, and Play/Pause held downloads it into folder.
func (a *app) trackRow(list, folder string, e entry, on map[string]string, artist bool) q2spot.Row {
	t, q := e.T, a.settings().Quality
	id := t.ID.String()
	who := ""
	if artist || (t.Album != nil && t.Performer.Name != t.Album.Artist.Name) {
		who = t.Performer.Name
	}
	title := qobuz.FullTitle(t.Title, t.Version)
	if p, ok := on[id]; ok {
		return q2spot.Row{Kind: "local", URI: p, Context: "album", Title: title,
			Subtitle: join(who, clock(t.Duration), "On the Q2"), Position: "rm:" + folder + ":" + id}
	}
	if !t.Streamable {
		return info(title, join(who, "Not available to stream"))
	}
	sub := join(who, clock(t.Duration), hiRes(t, q))
	if j := a.dl.Job(folder, id); j != nil {
		switch j.State {
		case dl.Active:
			pct := ""
			if j.Size > 0 {
				pct = fmt.Sprintf(" %d%%", j.Done*100/j.Size)
			}
			sub = join(who, "Downloading"+pct)
		case dl.Failed:
			sub = join(who, "Couldn't download: "+j.Err)
		default:
			sub = join(who, "Waiting to download")
		}
	}
	return q2spot.Row{Kind: "action", URI: "play", Context: list + ":" + id, Title: title, Subtitle: sub,
		Position: "dl:" + folder + ":" + id}
}

func (a *app) device(req q2spot.Request) q2spot.Reply {
	a.touch(false)
	ok := func(msg string) q2spot.Reply { return q2spot.Reply{Message: msg} }
	arg := req.Arg(0)
	switch req.Op {
	case "caps":
		var rows []q2spot.Row
		for _, op := range []string{"menu", "albums", "artists", "songs", "playlists", "album", "artist", "playlist",
			"search", "found", "downloaded", "dctx", "downloads", "settings", "pick", "set", "play", "get", "cancel",
			"getall", "stopctx", "rmctx", "signout", "retryall", "cancelall", "stop", "dl", "rm", "goto"} {
			rows = append(rows, q2spot.Row{Kind: "op", URI: op})
		}
		return q2spot.Reply{Message: version, Rows: rows}
	case "menu":
		return a.menu()
	case "stop":
		go func() { time.Sleep(300 * time.Millisecond); a.shutdown() }()
		return ok("Qobuz off")
	case "settings":
		return a.settingsPage()
	case "pick":
		return a.pickPage(arg)
	case "set":
		what, v, _ := strings.Cut(arg, ":")
		n, _ := strconv.Atoi(v)
		return a.choose(what, n)
	case "downloaded":
		return a.downloadedPage(cursor(req, 0))
	case "dctx":
		return a.folderPage(arg, cursor(req, 1))
	case "downloads":
		return a.downloadsPage()
	case "cancel":
		ctx, id, _ := strings.Cut(arg, ":")
		a.dl.Cancel(ctx, id)
		return ok("Download cancelled")
	case "stopctx":
		return ok("Cancelled " + plural(a.dl.CancelContext(arg), "download"))
	case "retryall":
		return ok("Trying " + plural(a.dl.RetryFailed(), "download") + " again")
	case "cancelall":
		return ok("Cancelled " + plural(a.dl.CancelContext(""), "download"))
	case "rmctx":
		if !a.confirm("rmctx:" + arg) {
			return ok("Press again to remove them from the Q2")
		}
		a.dl.CancelContext(arg)
		return ok("Removed " + plural(a.cache.RemoveContext(arg), "track"))
	case "signout":
		if !a.confirm("signout") {
			return ok("Press again to sign out")
		}
		a.dl.CancelContext("")
		a.saveSession(nil)
		return ok("Signed out. Downloads stay on the Q2")
	}
	if strings.HasPrefix(req.Op, "rm:") { // Play/Pause held on a downloaded track
		what := strings.TrimPrefix(req.Op, "rm:")
		i := strings.LastIndexByte(what, ':')
		if i < 0 {
			return q2spot.Fail(q2spot.Bad, "Bad request")
		}
		if !a.confirm("rm:" + what) {
			return ok("Hold again to remove it from the Q2")
		}
		if err := a.cache.Remove(what[:i], what[i+1:]); err != nil {
			return q2spot.Fail(q2spot.Error, "It isn't on the Q2")
		}
		return ok("Removed from the Q2")
	}
	if !a.signedIn() {
		return q2spot.Fail(q2spot.Error, "Sign in to Qobuz from your phone first")
	}
	if strings.HasPrefix(req.Op, "dl:") { // Play/Pause held: a list, or one of its tracks
		what := strings.TrimPrefix(req.Op, "dl:")
		if folder, id, ok := strings.Cut(what, ":"); ok {
			return a.get(folder, id)
		}
		return a.getAll(what)
	}
	switch req.Op {
	case "albums":
		return a.albumsPage(lettered(req))
	case "artists":
		return a.artistsPage(lettered(req))
	case "songs":
		return a.songsPage(arg, cursor(req, 1))
	case "playlists":
		return a.playlistsPage(cursor(req, 0))
	case "album":
		return a.folderList("album-"+arg, cursor(req, 1))
	case "playlist":
		return a.folderList("playlist-"+arg, cursor(req, 1))
	case "artist":
		return a.artistPage(arg, cursor(req, 1))
	case "goto": // Q2 Pod's Go to Album or Go to Artist on a song of ours: "album" or "artist", its path
		return a.gotoPage(arg, req.Arg(1), cursor(req, 2))
	case "search":
		return a.searchPage(strings.TrimSpace(arg))
	case "found":
		kind, term, _ := strings.Cut(arg, ":")
		return a.foundPage(kind, term, cursor(req, 1))
	case "get":
		ctx, id, _ := strings.Cut(arg, ":")
		return a.get(ctx, id)
	case "play":
		i := strings.LastIndexByte(arg, ':')
		if i < 0 {
			return q2spot.Fail(q2spot.Bad, "Bad request")
		}
		return a.play(arg[:i], arg[i+1:])
	case "getall":
		return a.getAll(arg)
	}
	return q2spot.Fail(q2spot.Unsupported, "Update Qobuz on the card")
}

func spaced(pin string) string {
	if len(pin) == 6 {
		return pin[:3] + " " + pin[3:]
	}
	return pin
}

func (a *app) phoneRow(title string) q2spot.Row {
	r := q2spot.Row{Kind: "remote", Title: title, Subtitle: "http://" + a.addr() + " · PIN " + spaced(a.settings().PIN)}
	if a.addr() == "" {
		r.Subtitle = "Connect the Q2 to Wi-Fi first"
	}
	return r
}

func (a *app) menu() q2spot.Reply {
	if !a.signedIn() {
		return q2spot.Reply{Rows: []q2spot.Row{
			a.phoneRow("Sign In from Your Phone"),
			info("Scan the code with your phone", "Then press Sign In with Qobuz on its page"),
			info("A paid Qobuz plan is needed", "Your password stays with Qobuz"),
			act("stop", "", "Turn Off Qobuz", ""),
		}}
	}
	busy := false
	count := func(kind, unit string) string {
		l, b, err := a.listOf(kind, false)
		busy = busy || b
		n := len(l.Albums) + len(l.Tracks) + len(l.Artists) + len(l.Playlists)
		switch {
		case n == 0 && b:
			return "Loading…"
		case n == 0 && err != nil:
			return "Couldn't load: " + err.Error()
		}
		return plural(n, unit)
	}
	rows := []q2spot.Row{
		list("albums", "", "Albums", count("albums", "album")),
		list("artists", "", "Artists", count("artists", "artist")),
		list("songs", "", "Songs", count("tracks", "liked song")),
		list("playlists", "", "Playlists", count("playlists", "playlist")),
		{Kind: "search", Title: "Search Qobuz", Subtitle: "Albums, songs, artists and playlists"},
	}
	if t, sub, ok := a.streamRow(); ok {
		rows = append([]q2spot.Row{info(t, sub)}, rows...)
		busy = true
	}
	used, files := a.cache.Usage()
	limit := int64(a.settings().LimitMB) << 20
	space := "Nothing yet · " + size(limit) + " of space"
	if files > 0 {
		space = join(plural(files, "track"), size(used)+" of "+size(limit))
	}
	rows = append(rows, list("downloaded", "", "Downloaded", space))
	if jobs := a.dl.Jobs(); len(jobs) > 0 {
		rows = append(rows, list("downloads", "", "Downloads", jobsSummary(jobs)))
		busy = true
	}
	rows = append(rows, list("settings", "", "Settings", "Quality, download space, account"),
		act("stop", "", "Turn Off Qobuz", "Downloads carry on next time"))
	return live(rows, "", busy)
}

func jobsSummary(jobs []dl.Job) string {
	waiting, failed := 0, 0
	active := ""
	for _, j := range jobs {
		switch j.State {
		case dl.Active:
			active = "Downloading"
			if j.Size > 0 {
				active += fmt.Sprintf(" %d%%", j.Done*100/j.Size)
			}
		case dl.Waiting:
			waiting++
		case dl.Failed:
			failed++
		}
	}
	w, f := "", ""
	if waiting > 0 {
		w = fmt.Sprintf("%d waiting", waiting)
	}
	if failed > 0 {
		f = fmt.Sprintf("%d failed", failed)
	}
	return join(active, w, f)
}

// listReply is a favourites list's level: loading, failed, or rows.
func listReply(rows []q2spot.Row, busy bool, err error, empty string, off int) q2spot.Reply {
	if len(rows) == 0 {
		switch {
		case busy:
			return loadingReply("Your Qobuz library")
		case err != nil:
			return q2spot.Fail(q2spot.Offline, say(err))
		}
		return q2spot.Reply{Rows: []q2spot.Row{info(empty, "")}}
	}
	page, next := paged(rows, off)
	return live(page, next, busy)
}

func albumRow(al qobuz.Album, withArtist bool) q2spot.Row {
	hr := ""
	if al.MaximumBitDepth > 16 {
		hr = "Hi-Res"
	}
	who := ""
	if withArtist {
		who = al.Artist.Name
	}
	r := list("album", al.ID.String(), qobuz.FullTitle(al.Title, al.Version), join(who, year(al.ReleaseDate), hr))
	r.Position = "dl:album-" + al.ID.String()
	return r
}

func (a *app) albumsPage(l string, off int) q2spot.Reply {
	lst, busy, err := a.listOf("albums", false)
	albums := append([]qobuz.Album(nil), lst.Albums...)
	sort.SliceStable(albums, func(i, j int) bool { return sortKey(albums[i].Title) < sortKey(albums[j].Title) })
	var rows []q2spot.Row
	if l == "" && len(albums) > lettersOver {
		var names []string
		for _, al := range albums {
			names = append(names, al.Title)
		}
		return live(letterRows("albums", names, "album"), "", busy)
	}
	for _, al := range albums {
		if l == "" || letter(al.Title) == l {
			rows = append(rows, albumRow(al, true))
		}
	}
	return listReply(rows, busy, err, "No favourite albums", off)
}

func (a *app) artistsPage(l string, off int) q2spot.Reply {
	lst, busy, err := a.listOf("artists", false)
	artists := append([]qobuz.Artist(nil), lst.Artists...)
	sort.SliceStable(artists, func(i, j int) bool { return sortKey(artists[i].Name) < sortKey(artists[j].Name) })
	if l == "" && len(artists) > lettersOver {
		var names []string
		for _, ar := range artists {
			names = append(names, ar.Name)
		}
		return live(letterRows("artists", names, "artist"), "", busy)
	}
	var rows []q2spot.Row
	for _, ar := range artists {
		if l == "" || letter(ar.Name) == l {
			rows = append(rows, list("artist", ar.ID.String(), ar.Name, plural(ar.AlbumsCount, "album")))
		}
	}
	return listReply(rows, busy, err, "No favourite artists", off)
}

func (a *app) songsPage(l string, off int) q2spot.Reply {
	_, es, err, done := a.tracksOf("liked")
	_, busy, _ := a.listOf("tracks", false)
	if !done && len(es) == 0 {
		return loadingReply("Your liked songs")
	}
	if err != nil && len(es) == 0 {
		return q2spot.Fail(q2spot.Offline, say(err))
	}
	if l == "" {
		var names []string
		for _, e := range es {
			names = append(names, e.T.Title)
		}
		rows := []q2spot.Row{list("songs", "recent", "Recently Liked", "Newest first")}
		return live(append(rows, letterRows("songs", names, "song")...), "", busy)
	}
	page, next := paged(likedSongs(es, l), off)
	on := a.cache.Files("liked")
	var rows []q2spot.Row
	for _, e := range page {
		rows = append(rows, a.trackRow("liked-"+l, "liked", e, on, true))
	}
	return live(rows, next, busy || a.moving("liked"))
}

func (a *app) playlistsPage(off int) q2spot.Reply {
	lst, busy, err := a.listOf("playlists", false)
	var rows []q2spot.Row
	for _, p := range lst.Playlists {
		owner := ""
		if p.Owner.ID.String() != fmt.Sprint(a.account().UserID) {
			owner = p.Owner.Name
		}
		r := list("playlist", p.ID.String(), p.Name, join(plural(p.TracksCount, "track"), owner))
		r.Position = "dl:playlist-" + p.ID.String()
		rows = append(rows, r)
	}
	return listReply(rows, busy, err, "No playlists", off)
}

// moving is whether a folder has downloads waiting or running.
func (a *app) moving(ctx string) bool {
	for _, j := range a.dl.Jobs() {
		if j.Ctx == ctx && j.State != dl.Failed {
			return true
		}
	}
	return false
}

// folderList is an album's or a playlist's level: play, download or remove it, then its tracks.
func (a *app) folderList(ctx string, off int) q2spot.Reply {
	_, es, err, done := a.tracksOf(ctx)
	if !done {
		return loadingReply("")
	}
	if err != nil {
		return q2spot.Fail(q2spot.Error, say(err))
	}
	on := a.cache.Files(ctx)
	// Files follow the list's order: a playlist reordered, an album named by an older version.
	want := map[string]string{}
	for _, e := range es {
		want[e.T.ID.String()] = e.Name
	}
	if a.cache.Renumber(ctx, want) > 0 {
		on = a.cache.Files(ctx)
	}
	what := "Album"
	if strings.HasPrefix(ctx, "playlist-") {
		what = "Playlist"
	}
	q := a.settings().Quality
	var rows []q2spot.Row
	if off == 0 {
		have, can, queued := 0, 0, 0 // can: on the Q2, or Qobuz streams it here
		var need, onBytes int64
		for _, e := range es {
			if _, ok := on[e.T.ID.String()]; ok {
				have++
				can++
			} else if e.T.Streamable {
				can++
				need += estimate(e.T, q)
			}
		}
		missing := ""
		if n := len(es) - can; n > 0 {
			missing = plural(n, "track") + " Qobuz doesn't stream here"
		}
		for _, j := range a.dl.Jobs() {
			if j.Ctx == ctx && j.State != dl.Failed {
				queued++
			}
		}
		for _, x := range a.cache.Contexts() {
			if x.Name == ctx {
				onBytes = x.Bytes
			}
		}
		switch {
		case queued > 0:
			rows = append(rows, act("stopctx", ctx, fmt.Sprintf("Downloading: %d of %d on the Q2", have, can),
				plural(queued, "track")+" to go · press to cancel"))
		case have < can:
			title := "Download " + what
			if have > 0 {
				title = fmt.Sprintf("Download the Other %d", can-have)
			}
			rows = append(rows, act("getall", ctx, title, join(plural(can-have, "track"), "about "+size(need),
				"or hold Play/Pause on a track")))
		case have == 0:
			rows = append(rows, info("Not Available to Download", missing))
		default:
			rows = append(rows, info("All on the Q2", join(plural(have, "track"), size(onBytes), missing)))
		}
		if have > 0 {
			t := "Remove from the Q2"
			if a.isArmed("rmctx:" + ctx) {
				t = "Press Again to Remove"
			}
			rows = append(rows, act("rmctx", ctx, t, join(plural(have, "track"), size(onBytes))))
		}
	}
	var tracks []q2spot.Row
	for _, e := range es {
		tracks = append(tracks, a.trackRow(ctx, ctx, e, on, strings.HasPrefix(ctx, "playlist-")))
	}
	page, next := paged(tracks, off)
	return live(append(rows, page...), next, a.moving(ctx))
}

// gotoPage is the album or the artist (the song's performer, else its album's artist) of the song at
// path, one of ours by the track id in its name: their pages, whether or not the library has them.
func (a *app) gotoPage(kind, path string, off int) q2spot.Reply {
	id := cache.TrackID(filepath.Base(path))
	if id == "" || (kind != "album" && kind != "artist") {
		return q2spot.Fail(q2spot.Bad, "That song isn't one of Qobuz's")
	}
	t, err, done := a.track(id)
	if !done {
		return loadingReply("")
	}
	if err != nil {
		return q2spot.Fail(q2spot.Error, say(err))
	}
	if kind == "album" {
		if t.Album == nil || t.Album.ID.String() == "" {
			return q2spot.Fail(q2spot.Error, "Qobuz doesn't say its album")
		}
		return a.folderList("album-"+t.Album.ID.String(), off)
	}
	artist := t.Performer.ID.String()
	if (artist == "" || artist == "0") && t.Album != nil {
		artist = t.Album.Artist.ID.String()
	}
	if artist == "" || artist == "0" {
		return q2spot.Fail(q2spot.Error, "Qobuz doesn't say its artist")
	}
	return a.artistPage(artist, off)
}

func (a *app) artistPage(id string, off int) q2spot.Reply {
	p, err, done := a.artist(id)
	if !done {
		return loadingReply("")
	}
	if err != nil {
		return q2spot.Fail(q2spot.Error, say(err))
	}
	albums := append([]qobuz.Album(nil), p.Albums...)
	sort.SliceStable(albums, func(i, j int) bool { return albums[i].ReleaseDate > albums[j].ReleaseDate })
	var rows []q2spot.Row
	for _, al := range albums {
		rows = append(rows, albumRow(al, al.Artist.Name != p.Artist.Name))
	}
	if len(rows) == 0 {
		rows = append(rows, info("No albums", ""))
	}
	page, next := paged(rows, off)
	return q2spot.Reply{Rows: page, Cursor: next}
}

func (a *app) searchPage(term string) q2spot.Reply {
	if term == "" {
		return q2spot.Fail(q2spot.Bad, "Type something to search for")
	}
	f, err, done := a.search(term)
	if !done {
		return loadingReply("Searching")
	}
	if err != nil {
		return q2spot.Fail(q2spot.Offline, say(err))
	}
	var rows []q2spot.Row
	for _, k := range []struct {
		kind, title string
		n           int
	}{{"albums", "Albums", f.Albums.Total}, {"tracks", "Songs", f.Tracks.Total}, {"artists", "Artists", f.Artists.Total},
		{"playlists", "Playlists", f.Playlists.Total}} {
		if k.n > 0 {
			rows = append(rows, list("found", k.kind+":"+term, k.title, fmt.Sprintf("%d found", k.n)))
		}
	}
	if len(rows) == 0 {
		rows = append(rows, info("Nothing found", "Try other words"))
	}
	return q2spot.Reply{Rows: rows}
}

func (a *app) foundPage(kind, term string, off int) q2spot.Reply {
	f, err, done := a.search(term)
	if !done {
		return loadingReply("Searching")
	}
	if err != nil {
		return q2spot.Fail(q2spot.Offline, say(err))
	}
	var rows []q2spot.Row
	switch kind {
	case "albums":
		for _, al := range f.Albums.Items {
			rows = append(rows, albumRow(al, true))
		}
	case "artists":
		for _, ar := range f.Artists.Items {
			rows = append(rows, list("artist", ar.ID.String(), ar.Name, plural(ar.AlbumsCount, "album")))
		}
	case "playlists":
		for _, p := range f.Playlists.Items {
			r := list("playlist", p.ID.String(), p.Name, join(plural(p.TracksCount, "track"), p.Owner.Name))
			r.Position = "dl:playlist-" + p.ID.String()
			rows = append(rows, r)
		}
	case "tracks": // streamed through the results; held, one downloads into its album's folder
		for _, t := range f.Tracks.Items {
			if t.Album == nil {
				continue
			}
			folder := "album-" + t.Album.ID.String()
			r := a.trackRow("found:"+term, folder, entry{T: t}, a.cache.Files(folder), true)
			if t.Album.Title != "" && r.Kind != "local" && a.dl.Job(folder, t.ID.String()) == nil {
				r.Subtitle = join(t.Performer.Name, t.Album.Title, clock(t.Duration))
			}
			rows = append(rows, r)
		}
	}
	page, next := paged(rows, off)
	return live(page, next, kind == "tracks" && len(a.dl.Jobs()) > 0)
}

// play streams a list from one of its tracks.
func (a *app) play(list, id string) q2spot.Reply {
	label, es, err := a.await(list)
	if err != nil {
		return q2spot.Fail(q2spot.Error, say(err))
	}
	for i, e := range es {
		if e.T.ID.String() == id {
			if _, cached := a.cache.Anywhere(id); q2.LocalIP() == "" && !cached {
				return q2spot.Fail(q2spot.Offline, "Connect the Q2 to Wi-Fi to stream")
			}
			a.startStream(label, es, i)
			return q2spot.Reply{Message: startToast(qobuz.FullTitle(e.T.Title, e.T.Version))}
		}
	}
	return q2spot.Fail(q2spot.Unavailable, "That track isn't in the list any more")
}

// startToast says a stream will start, not that it failed when the toast goes: the title cut so
// the message fits a toast's 120 bytes.
func startToast(title string) string {
	const tail = " as soon as it is ready"
	if room := q2spot.MaxMessage - len("Starting ") - len(tail); len(title) > room {
		title = q2spot.Cut(title, room-len("…")) + "…"
	}
	return "Starting " + title + tail
}

// get queues one track of a folder.
func (a *app) get(ctx, id string) q2spot.Reply {
	label, es, err := a.await(ctx)
	if err != nil {
		return q2spot.Fail(q2spot.Error, say(err))
	}
	for _, e := range es {
		if e.T.ID.String() == id {
			a.cache.Label(ctx, label)
			if a.dl.Queue(dl.Job{Ctx: ctx, Name: e.Name, Track: e.T}) == 0 {
				return q2spot.Reply{Message: "It's already on the Q2 or on its way"}
			}
			if n := len(a.dl.Jobs()); n > 1 {
				return q2spot.Reply{Message: fmt.Sprintf("Downloading after %d more", n-1)}
			}
			return q2spot.Reply{Message: "Downloading"}
		}
	}
	return q2spot.Fail(q2spot.Unavailable, "That track isn't in the list any more")
}

// getAll queues a folder's tracks not on the card, in order.
func (a *app) getAll(ctx string) q2spot.Reply {
	if ctx == "liked" {
		return q2spot.Fail(q2spot.Bad, "Download liked songs one at a time")
	}
	label, es, err := a.await(ctx)
	if err != nil {
		return q2spot.Fail(q2spot.Error, say(err))
	}
	a.cache.Label(ctx, label)
	var jobs []dl.Job
	for _, e := range es {
		if e.T.Streamable {
			jobs = append(jobs, dl.Job{Ctx: ctx, Name: e.Name, Track: e.T})
		}
	}
	n := a.dl.Queue(jobs...)
	if n == 0 {
		return q2spot.Reply{Message: "All of it is on the Q2 or on its way"}
	}
	return q2spot.Reply{Message: "Downloading " + plural(n, "track") + " of " + label}
}

func (a *app) downloadedPage(off int) q2spot.Reply {
	ctxs := a.cache.Contexts()
	if len(ctxs) == 0 {
		return q2spot.Reply{Rows: []q2spot.Row{info("Nothing downloaded", "Open an album and press Download Album")}}
	}
	var rows []q2spot.Row
	for _, x := range ctxs {
		rows = append(rows, list("dctx", x.Name, x.Label, join(plural(x.Files, "track"), size(x.Bytes))))
	}
	page, next := paged(rows, off)
	return q2spot.Reply{Rows: page, Cursor: next}
}

// folderPage is a folder's downloads as the card has them, without Wi-Fi.
func (a *app) folderPage(ctx string, off int) q2spot.Reply {
	files := a.cache.Files(ctx)
	if len(files) == 0 {
		return q2spot.Reply{Rows: []q2spot.Row{info("Nothing here now", "")}}
	}
	paths := make([]string, 0, len(files))
	for _, p := range files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	var rows, tracks []q2spot.Row
	if off == 0 {
		t := "Remove from the Q2"
		if a.isArmed("rmctx:" + ctx) {
			t = "Press Again to Remove"
		}
		rows = append(rows, act("rmctx", ctx, t, plural(len(paths), "track")))
	}
	for _, p := range paths {
		name := cache.Stem(filepath.Base(p))
		if i := strings.LastIndex(name, " ["); i > 0 {
			name = name[:i]
		}
		if _, rest, ok := strings.Cut(name, " "); ok {
			name = rest
		}
		tracks = append(tracks, q2spot.Row{Kind: "local", URI: p, Context: "album", Title: name,
			Position: "rm:" + ctx + ":" + cache.TrackID(filepath.Base(p))})
	}
	page, next := paged(tracks, off)
	return q2spot.Reply{Rows: append(rows, page...), Cursor: next}
}

func (a *app) downloadsPage() q2spot.Reply {
	jobs := a.dl.Jobs()
	if len(jobs) == 0 {
		return q2spot.Reply{Rows: []q2spot.Row{info("Nothing downloading", "")}}
	}
	var rows []q2spot.Row
	failed := 0
	for _, j := range jobs {
		if len(rows) == q2spot.MaxRows-2 {
			break
		}
		id := j.Ctx + ":" + j.Track.ID.String()
		title := qobuz.FullTitle(j.Track.Title, j.Track.Version)
		switch j.State {
		case dl.Active:
			pct := ""
			if j.Size > 0 {
				pct = fmt.Sprintf("%d%% of %s", j.Done*100/j.Size, size(j.Size))
			}
			rows = append(rows, act("cancel", id, title, join(j.Track.Performer.Name, "Downloading "+pct)))
		case dl.Failed:
			failed++
			rows = append(rows, act("get", id, title, "Couldn't download: "+j.Err))
		default:
			why := "Waiting"
			if j.Err != "" {
				why = "Trying again soon: " + j.Err
			}
			rows = append(rows, act("cancel", id, title, join(j.Track.Performer.Name, why)))
		}
	}
	if failed > 0 {
		rows = append(rows, act("retryall", "", "Try Failed Again", plural(failed, "track")))
	}
	rows = append(rows, act("cancelall", "", "Cancel All", plural(len(jobs), "download")))
	return live(rows, "", true)
}

var spaces = []int{1024, 2048, 4096, 8192, 16384, 32768} // Download Space's choices, in MB

func (a *app) settingsPage() q2spot.Reply {
	s := a.settings()
	used, _ := a.cache.Usage()
	acct := a.account()
	rows := []q2spot.Row{
		list("pick", "stream", "Streaming Quality", qualityName(s.StreamQuality)),
		list("pick", "download", "Download Quality", qualityName(s.Quality)),
		list("pick", "space", "Download Space", join(fmt.Sprintf("%d GB", s.LimitMB/1024), size(used)+" used")),
	}
	if q2.HasCap("goto") { // Q2 Pod sends Go to on our songs only when it has it
		rows = append(rows, gotoRow(s.GotoLibrary))
	}
	rows = append(rows, a.phoneRow("Phone Page"))
	if acct.Token != "" {
		plan := acct.Plan
		if acct.HiRes {
			plan = join(plan, "Hi-Res")
		}
		t := "Sign Out"
		if a.isArmed("signout") {
			t = "Press Again to Sign Out"
		}
		who := "Signed in"
		if acct.Name != "" {
			who += " as " + acct.Name
		}
		rows = append(rows, info(who, join(plan, "since "+acct.SignedIn.Format("2 Jan 2006"))),
			act("signout", "", t, "Downloads stay on the Q2"))
	}
	return q2spot.Reply{Rows: rows}
}

// gotoRow switches what Q2 Pod's Go to Album and Go to Artist open on a Qobuz song.
func gotoRow(library bool) q2spot.Row {
	if library {
		return act("set", "goto:1", "Go to Qobuz Pages: Off", "Go to Album and Artist open your Library's pages")
	}
	return act("set", "goto:0", "Go to Qobuz Pages: On", "Go to Album and Artist open Qobuz's full pages")
}

// pickPage is a setting's choices: a press picks one (set), which then reads Selected; Return goes
// back to Settings.
func (a *app) pickPage(what string) q2spot.Reply {
	s := a.settings()
	var rows []q2spot.Row
	switch what {
	case "stream", "download":
		cur := s.StreamQuality
		if what == "download" {
			cur = s.Quality
		}
		for _, q := range qualities {
			sub := qualityNote(q)
			if q == cur {
				sub = "Selected"
			}
			rows = append(rows, act("set", fmt.Sprintf("%s:%d", what, q), qualityName(q), sub))
		}
	case "space":
		used, _ := a.cache.Usage()
		for _, mb := range spaces {
			sub := ""
			if mb == s.LimitMB {
				sub = "Selected · " + size(used) + " used"
			} else if used > int64(mb)<<20 {
				sub = "The oldest downloads would go"
			}
			rows = append(rows, act("set", fmt.Sprintf("space:%d", mb), fmt.Sprintf("%d GB", mb/1024), sub))
		}
	default:
		return q2spot.Fail(q2spot.Bad, "No such setting")
	}
	return q2spot.Reply{Rows: rows}
}

// choose picks a setting's value.
func (a *app) choose(what string, v int) q2spot.Reply {
	switch what {
	case "stream", "download":
		if !validQuality(v) {
			return q2spot.Fail(q2spot.Bad, "No such quality")
		}
		if err := a.update(func(s *settings) {
			if what == "stream" {
				s.StreamQuality = v
			} else {
				s.Quality = v
			}
		}); err != nil {
			return q2spot.Fail(q2spot.Error, "Couldn't save the setting")
		}
		if what == "stream" {
			return q2spot.Reply{Message: "Streaming in " + qualityName(v)}
		}
		return q2spot.Reply{Message: "Downloads in " + qualityName(v)}
	case "space":
		ok := false
		for _, mb := range spaces {
			ok = ok || mb == v
		}
		if !ok {
			return q2spot.Fail(q2spot.Bad, "No such size")
		}
		if err := a.update(func(s *settings) { s.LimitMB = v }); err != nil {
			return q2spot.Fail(q2spot.Error, "Couldn't save the setting")
		}
		if used, _ := a.cache.Usage(); used > int64(v)<<20 {
			go a.cache.Trim(int64(v)<<20, "")
			return q2spot.Reply{Message: fmt.Sprintf("%d GB: the oldest downloads go", v/1024)}
		}
		return q2spot.Reply{Message: fmt.Sprintf("Downloads keep to %d GB", v/1024)}
	case "goto":
		if v != 0 && v != 1 {
			return q2spot.Fail(q2spot.Bad, "No such choice")
		}
		if err := a.update(func(s *settings) { s.GotoLibrary = v == 0 }); err != nil {
			return q2spot.Fail(q2spot.Error, "Couldn't save the setting")
		}
		a.manifest()
		if v == 1 {
			return q2spot.Reply{Message: "Go to opens Qobuz's pages"}
		}
		return q2spot.Reply{Message: "Go to opens your Library's pages"}
	}
	return q2spot.Fail(q2spot.Bad, "No such setting")
}
