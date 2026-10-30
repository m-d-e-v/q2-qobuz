# q2-qobuz

Qobuz on the **Shanling Q2** running [Q2 Pod](https://github.com/DiamondBond/q2-pod). Sign in from your phone, browse your favourite albums, artists and songs, your playlists and Qobuz's catalogue on the Q2, and download tracks, albums and playlists to the card as FLAC up to 24-bit 192 kHz. Each file is tagged and carries its cover, and Q2 Pod's own player plays it. It needs a paid Qobuz plan that streams lossless (hi-res needs one that streams hi-res), and Q2 Pod 1.4.2 or later, which runs card apps.

> [!IMPORTANT]
> Qobuz publishes no API for this. q2-qobuz uses the one Qobuz's own web player uses, as every third-party Qobuz player does, and reads its app id from the web player. Qobuz can change or stop that at any time, and its terms may not allow it. Your password only ever goes to Qobuz's own sign-in page.

## Install

1. Download `q2-qobuz-*.zip` from the latest release.
2. Power the Q2 off and put its microSD card in your computer.
3. Unzip the file to the card's root, so the card has `.qobuz/q2-qobuz`, `.qobuz/run`, `.qobuz/q2app` and the icons.
4. Put the card back and power the Q2 on. **Streaming** now has a **Qobuz** row.

## Sign in

1. On the Q2, open **Streaming → Qobuz** and press **Sign In from Your Phone**. A QR code shows.
2. Scan it with your phone, on the same Wi-Fi as the Q2. The Q2's page opens; press **Sign In with Qobuz**.
3. Sign in on Qobuz's page. It sends you back to the Q2, which now has your library.

The sign-in is kept in the Q2's own storage (`/mnt/data/q2-qobuz`), not on the card. **Settings → Sign Out** removes it.

## Use

- **Albums**, **Artists**, **Songs** (liked) and **Playlists**: your Qobuz library. Long lists open on their letters. **Songs** has **Recently Liked**, newest first.
- **Search Qobuz**: albums, songs, artists and playlists from the whole catalogue.
- **Press a track to stream it**, and the rest of its list after it, as soon as its first track has arrived (Q2 Pod 1.4.3's stream mode for card apps): the toast says "Starting … as soon as it is ready", and Now Playing opens by itself when it is. Streaming is CD quality by default; MP3 starts soonest; the app stays two tracks ahead and clears tracks well behind, so streaming doesn't fill the card. A track already downloaded plays from the Q2 at once, even one Qobuz no longer streams; one it doesn't stream here is left out.
- **Hold Play/Pause to download**, as on Q2 Pod's other streaming services: on a track, an album or a playlist, in any list. An album or playlist also has **Download Album** (or **Download Playlist**) at the top of its page, and **Remove from the Q2** once any of it is there. Hold Play/Pause twice on a downloaded track to remove it.
- **Downloaded**: everything you downloaded, by album or playlist. It works without Wi-Fi.
- **Downloads**: what is downloading or waiting. Downloads pause while a stream fetches a track and carry on after.
- **Settings**: **Streaming Quality** (CD by default) and **Download Quality** (up to 24-bit 96 kHz by default), each a list to pick from: MP3 (320 kbps, Qobuz's lowest), CD, up to 24-bit 96 kHz or up to 24-bit 192 kHz; **Download Space** (1 to 32 GB; the oldest downloads go first when it is full); **Go to Qobuz Pages** (on by default: Go to Album and Go to Artist on a Qobuz song open Qobuz's full pages; off: your Library's), on Q2 Pod builds that have it (they list `goto` in `/tmp/q2-addon.caps`); the phone page; your account.

A four-minute track is about 10 MB as MP3, 30 MB at CD quality, 100 MB at 24-bit 96 kHz and 200 MB at 24-bit 192 kHz, so the first track of a stream takes that long to arrive on the Q2's Wi-Fi. Downloads carry on from where they stopped after Wi-Fi drops, standby or USB storage, and the Q2 stays awake while they or a stream run (the screen still turns off). Downloads are kept in `.qobuz/cache` and streams in `.qobuz/stream`, played only from the Qobuz page, not added to your Library.

**Now Playing's titles** come from each file's tags. Q2 Pod up to 1.4.3 shows a card app's tracks by their file names, with an unknown artist; newer Q2 Pod reads their tags.

## Files

- `.qobuz/config.json`: the phone page's key and PIN, `idle_minutes`, `quality` (downloads'), `stream_quality` and `limit_mb`.
- `.qobuz/stream/<session>/`: the tracks of the stream playing, deleted as it moves on and when the next starts.
- `.qobuz/cache/<album-…|playlist-…|liked>/`: downloads, named in play order with the track's id, `.flac` (tagged with Vorbis comments) or `.mp3` (ID3v2.4).
- `.qobuz/lists/`, `.qobuz/covers/`, `.qobuz/creds.json`, `.qobuz/queue.json`, `.qobuz/partial/`: your library as last read, covers, the web player's app id, the downloads waiting and unfinished ones.
- `/mnt/data/q2-qobuz/session.json`: the sign-in.

## Build

    contrib/shanlingq2/build.sh

needs Go 1.22 or later and python3, and no dependencies outside Go's standard library. Tests: `go test ./...` and `python3 contrib/shanlingq2/test_launcher.py`. How it fits Q2 Pod: [contrib/shanlingq2/README](contrib/shanlingq2/README).

`cmd/qprobe` tries Qobuz from a computer: `go build -o bin/qprobe ./cmd/qprobe`, then `bin/qprobe login` (sign in from a phone through this computer, as a Q2 would), `check` (the account and every page of the library) and `get [track-id]` (each format's answer for a track, and the best one downloaded and read).

## License

[MIT](LICENSE). The Streaming icon (`contrib/shanlingq2/icon*.png`) is a plain Q; a local build uses `contrib/shanlingq2/personal/icon*.png` instead when that folder exists (git ignores it). `internal/q2`, `internal/q2spot`, `internal/qr` and the launcher and packaging follow [q2-filebrowser](https://github.com/group-a-study/q2-filebrowser) (MIT). `internal/tlsroots/cacert.pem` is Mozilla's CA bundle as curl publishes it (MPL-2.0). Qobuz is a trademark of Xandrie SA; this project is not affiliated with Qobuz or Shanling.
