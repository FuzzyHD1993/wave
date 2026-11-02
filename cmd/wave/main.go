// Command wave — "Волна": TUI-плеер MPD с бесконечным личным радио,
// библиотекой, историей и статистикой.
//
// Usage:
//
//	wave               TUI
//	wave --selftest    check MPD/DB/library/likes
//	wave sync          resync library + likes
//	wave next -n N     print next N wave picks (headless)
//	wave stats         headless stats
package main

import (
	"flag"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"time"

	"wave/internal/daemon"
	mpdclient "wave/internal/mpd"
	"wave/internal/radio"
	"wave/internal/store"
	"wave/internal/tui"
)

const mpdAddr = "localhost:6600"
const likesPath = "~/projects/yandex-wave/likes.json"

func likesFile() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return strings.TrimPrefix(likesPath, "~")
	}
	return home + "/projects/yandex-wave/likes.json"
}

func main() {
	args := os.Args[1:]

	var selftest bool
	var doSync bool
	ncount := 10

	// scan args: --selftest / --sync flags + first subcommand
	cmd := ""
	rest := args[:0:0]
	for _, a := range args {
		switch a {
		case "--selftest", "-selftest":
			selftest = true
		case "--sync", "-sync":
			doSync = true
		default:
			rest = append(rest, a)
		}
	}
	args = rest
	if len(args) > 0 {
		cmd = args[0]
		args = args[1:]
	}
	if !selftest && !doSync {
		switch cmd {
		case "sync":
			doSync = true
			cmd = ""
		case "next", "stats", "selftest":
			if cmd == "selftest" {
				selftest = true
				cmd = ""
			}
		}
	}

	fs := flag.NewFlagSet("wave", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	fs.IntVar(&ncount, "n", 10, "wave next: number of picks")
	if cmd == "next" {
		if err := fs.Parse(args); err != nil {
			os.Exit(2)
		}
	}

	st, err := store.Open(store.DefaultPath())
	if err != nil {
		fmt.Fprintln(os.Stderr, "db:", err)
		os.Exit(1)
	}
	defer st.Close()

	switch {
	case selftest:
		runSelftest(st)
		return
	case doSync:
		runSync(st)
	case cmd == "next":
		runNext(st, ncount)
	case cmd == "stats":
		runStats(st)
	default:
		// TUI
		st.Close()
		if err := runTUI(); err != nil {
			fmt.Fprintln(os.Stderr, "wave:", err)
			os.Exit(1)
		}
	}
}

// ---------- selftest ----------

func runSelftest(st *store.Store) {
	fail := false
	line := func(ok bool, msg string) {
		if ok {
			fmt.Printf("OK  %s\n", msg)
		} else {
			fmt.Printf("FAIL %s\n", msg)
			fail = true
		}
	}

	// bootstrap: library sync if empty + likes import (once)
	n, err := st.CountTracks()
	if err != nil {
		line(false, "DB: "+err.Error())
		os.Exit(1)
	}
	line(true, "DB "+store.DefaultPath())

	mp, err := mpdclient.Dial(mpdAddr)
	if err != nil {
		line(false, "MPD: "+err.Error())
		os.Exit(1)
	}
	defer mp.Close()
	status, err := mp.Status()
	gauge := "?"
	if err == nil {
		gauge = "state=" + mpdclient.AttrOr(status, "state", "?")
	}
	line(err == nil, "MPD "+mpdAddr+" "+gauge)

	if n == 0 {
		n, err = daemonLibrarySync(mp, st)
		if err != nil {
			line(false, "Library sync: "+err.Error())
			os.Exit(1)
		}
	}
	line(n > 0, fmt.Sprintf("Library: %d треков", n))

	matched, total, _, err := likesImportRun(st)
	line(err == nil && matched > 0, fmt.Sprintf("Likes: %d/%d%s", matched, total, errSuffix(err)))
	if err == nil && matched == 0 {
		line(false, "Likes: 0 совпадений")
	}

	if fail {
		os.Exit(1)
	}
	os.Exit(0)
}

func errSuffix(err error) string {
	if err != nil {
		return " (" + err.Error() + ")"
	}
	return ""
}

// ---------- sync ----------

func runSync(st *store.Store) {
	mp, err := mpdclient.Dial(mpdAddr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "mpd:", err)
		os.Exit(1)
	}
	defer mp.Close()
	n, err := daemonLibrarySync(mp, st)
	if err != nil {
		fmt.Fprintln(os.Stderr, "sync:", err)
		os.Exit(1)
	}
	fmt.Printf("Синхронизировано треков: %d\n", n)
	// likes re-import (guarded): matched/total/importedNow
	matched, total, importedNow, err := likesImportRun(st)
	if err == nil && importedNow {
		fmt.Printf("Импорт лайков выполнен: %d/%d\n", matched, total)
	} else if err == nil {
		fmt.Printf("Лайков в базе: %d\n", matched)
	} else {
		fmt.Printf("Лайки: ошибка: %v\n", err)
	}
}

// ---------- next ----------

func runNext(st *store.Store, n int) {
	r := radio.New(st)
	ctx, err := r.NewContext(nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wave next:", err)
		os.Exit(1)
	}
	wts, err := r.Weights(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "wave next:", err)
		os.Exit(1)
	}
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	for i := 0; i < n; i++ {
		t, err := r.Pick(ctx, wts, rng)
		if err != nil {
			fmt.Fprintln(os.Stderr, "wave next:", err)
			os.Exit(1)
		}
		fmt.Printf("%s — %s\n", t.Artist, t.Title)
	}
}

// ---------- shared import helpers ----------

// daemonLibrarySync is the CLI-side library sync.
func daemonLibrarySync(mp *mpdclient.Client, st *store.Store) (int, error) {
	return daemon.LibrarySync(mp, st)
}

// likesImportRun imports likes from the Yandex export (guarded by meta).
func likesImportRun(st *store.Store) (matched, total int, imported bool, err error) {
	return daemon.LikesImport(st, likesFile())
}

// ---------- stats ----------

func runStats(st *store.Store) {
	totalMS, err := st.TotalListen()
	if err != nil {
		fmt.Fprintln(os.Stderr, "stats:", err)
		os.Exit(1)
	}
	plays, _ := st.CountPlays()
	skips, _ := st.CountSkips()
	likes, _ := st.CountLikes()
	fmt.Printf("Прослушано всего: %s\n", humanMS(totalMS))
	fmt.Printf("Прослушиваний: %d, пропусков: %d, лайков: %d\n", plays, skips, likes)

	arts, err := st.TopArtists(10)
	if err == nil && len(arts) > 0 {
		fmt.Println("\nТОП-10 артистов:")
		for i, a := range arts {
			fmt.Printf(" %2d. %-30s %4d  %s\n", i+1, a.Name, a.Count, humanMS(a.ListenMS))
		}
	}
	tracks, err := st.TopTracks(10)
	if err == nil && len(tracks) > 0 {
		fmt.Println("\nТОП-10 треков:")
		for i, t := range tracks {
			fmt.Printf(" %2d. %s  %d\n", i+1, t.Name, t.Count)
		}
	}
	days, err := st.ListenPerDay(30)
	if err == nil {
		fmt.Printf("\n30 дней: %s\n", sparkline(days))
	}
	playsY, msY, err := st.YearTotals(time.Now().Year())
	if err == nil {
		fmt.Printf("\nИтоги года %d: %d треков, %s\n", time.Now().Year(), playsY, humanMS(msY))
	}
}

func humanMS(ms int64) string {
	if ms <= 0 {
		return "0 мин"
	}
	minutes := ms / 60000
	if minutes < 60 {
		return fmt.Sprintf("%d мин", minutes)
	}
	h := minutes / 60
	m := minutes % 60
	if h < 48 {
		return fmt.Sprintf("%d ч %02d мин", h, m)
	}
	return fmt.Sprintf("%d д %d ч", h/24, h%24)
}

// sparkline renders last-30-days listening as ▁▂▃▄▅▆▇█ bars.
func sparkline(days []int64) string {
	var mx int64
	for _, v := range days {
		if v > mx {
			mx = v
		}
	}
	chars := []rune("▁▂▃▄▅▆▇█")
	out := make([]rune, 0, len(days))
	for _, v := range days {
		if mx == 0 || v == 0 {
			out = append(out, ' ')
			continue
		}
		idx := int(float64(v) / float64(mx) * float64(len(chars)-1))
		out = append(out, chars[idx])
	}
	return string(out)
}

// ---------- TUI hook ----------

func runTUI() error {
	return tui.Run()
}
