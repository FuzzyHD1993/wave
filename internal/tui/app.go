package tui

import (
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"wave/internal/daemon"
	mpdclient "wave/internal/mpd"
	"wave/internal/radio"
	"wave/internal/store"
)

const mpdAddr = "localhost:6600"

type section int

const (
	secWave section = iota
	secLibrary
	secHistory
	secStats
	sectionCount
)

var sectionNames = [...]string{"Волна", "Библиотека", "История", "Статистика"}

type queueItem struct {
	id     int
	pos    int
	uri    string
	artist string
	title  string
	durSec float64
}

type libItem struct {
	kind  int // 0 artist, 1 album, 2 track
	track store.Track
	depth int
	label string
}

// ---------- messages ----------

type tickMsg struct{}

type evMsg string // MPD idle subsystem name

type errMsg string

type statusMsg struct {
	status mpdclient.Attrs
	song   mpdclient.Attrs
	queue  []queueItem
	err    error
}

type bootstrapMsg struct {
	tracks int
	likes  int
	err    error
}

type syncMsg struct {
	tracks int
	likes  int
	err    error
}

type actionMsg struct {
	ok   string
	err  error
	file string
}

type topupMsg struct {
	added int
	err   error
}

// ---------- model ----------

// Model is the bubbletea model.
type Model struct {
	st    *store.Store
	mp    *mpdclient.Client
	obs   *daemon.Observer
	radio *radio.Radio

	sec  section
	w, h int

	library libPane
	wave    wavePane
	history histPane
	stats   statsPane

	// player snapshot
	playing   bool
	volume    int
	elasped   float64
	dur       float64
	songPos   int
	plLen     int
	curURI    string
	curArtist string
	curTitle  string
	curLiked  bool
	likedSet  map[string]bool

	queue []queueItem

	// wave keeping
	rng          *rand.Rand
	syncing      bool
	bootstrapped bool
	ticks        int
	flash        string
	flashUntil   time.Time
	errStr       string
}

// NewModel builds the model (no IO here).
func NewModel(st *store.Store, mp *mpdclient.Client, obs *daemon.Observer) Model {
	in := textinput.New()
	in.Placeholder = "поиск (артист, трек, альбом)"
	in.Prompt = "/ "
	in.CharLimit = 100
	m := Model{
		st:     st,
		mp:     mp,
		obs:    obs,
		radio:  radio.New(st),
		sec:    secWave,
		volume: -1,
		rng:    rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	m.library.input = in
	return m
}

// ---------- Init ----------

// Init implements tea.Model.
func (m Model) Init() tea.Cmd {
	m.library.input.Focus()
	return tea.Batch(
		textinput.Blink,
		tick(),
		m.fetchStatus(),
		m.bootstrap(),
	)
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return tickMsg{} })
}

// ---------- commands ----------

func (m *Model) fetchStatus() tea.Cmd {
	return func() tea.Msg {
		st, err := m.mp.Status()
		if err != nil {
			return statusMsg{err: err}
		}
		song, err := m.mp.CurrentSong()
		if err != nil && !mpdNoSong(err) {
			return statusMsg{err: err}
		}
		q, err := m.mp.Queue()
		if err != nil {
			return statusMsg{err: err}
		}
		_ = m.obs.Observe(st, song) // play signal recording
		var items []queueItem
		for i, a := range q {
			id := mpdclient.Int(a, "id", 0)
			it := queueItem{
				id:     id,
				pos:    mpdclient.Int(a, "pos", i),
				uri:    mpdclient.AttrOr(a, "file", ""),
				artist: mpdclient.AttrOr(a, "artist", ""),
				title:  mpdclient.AttrOr(a, "title", ""),
				durSec: mpdclient.Float(a, "duration", 0),
			}
			items = append(items, it)
		}
		return statusMsg{status: st, song: song, queue: items}
	}
}

func mpdNoSong(err error) bool {
	return err != nil && strings.Contains(err.Error(), "No such song")
}

func (m *Model) bootstrap() tea.Cmd {
	return func() tea.Msg {
		n, err := m.st.CountTracks()
		if err != nil {
			return bootstrapMsg{err: err}
		}
		if n == 0 {
			n, err = daemon.LibrarySync(m.mp, m.st)
			if err != nil {
				return bootstrapMsg{err: err}
			}
		}
		likes := 0
		if imp, err := m.st.LikesImported(); err == nil && !imp {
			_, _, _, err := daemon.LikesImport(m.st, likesFilePath())
			if err != nil {
				return bootstrapMsg{tracks: n, err: err}
			}
		}
		likes, _ = m.st.CountLikes()
		return bootstrapMsg{tracks: n, likes: likes}
	}
}

func (m *Model) resync() tea.Cmd {
	m.syncing = true
	return func() tea.Msg {
		n, err := daemon.LibrarySync(m.mp, m.st)
		likes := 0
		if err == nil {
			_, _, _, lerr := daemon.LikesImport(m.st, likesFilePath())
			err = lerr
		}
		likes, _ = m.st.CountLikes()
		return syncMsg{tracks: n, likes: likes, err: err}
	}
}

func likesFilePath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "likes.json"
	}
	return home + "/projects/yandex-wave/likes.json"
}

// topupCmd tops the queue up to radio.UpcomingTarget upcoming tracks when
// fewer than radio.UpcomingMin remain after the current song. State is
// re-fetched from MPD (not the model snapshot) to avoid startup races.
func (m *Model) topupCmd() tea.Cmd {
	return func() tea.Msg {
		status, err := m.mp.Status()
		if err != nil {
			return topupMsg{err: err}
		}
		q, err := m.mp.Queue()
		if err != nil {
			return topupMsg{err: err}
		}
		queueIDs := map[string]bool{}
		for _, a := range q {
			if uri, ok := mpdclient.Attr(a, "file"); ok && uri != "" {
				queueIDs[uri] = true
			}
		}
		songPos := mpdclient.Int(status, "song", -1)
		upcoming := len(q)
		if songPos >= 0 && songPos < len(q) {
			upcoming = len(q) - (songPos + 1)
		}
		if upcoming >= radio.UpcomingMin {
			return topupMsg{}
		}
		r := radio.New(m.st)
		ctx, err := r.NewContext(queueIDs)
		if err != nil {
			return topupMsg{err: err}
		}
		wts, err := r.Weights(ctx)
		if err != nil {
			return topupMsg{err: err}
		}
		rng := rand.New(rand.NewSource(time.Now().UnixNano()))
		added := 0
		for upcoming+added < radio.UpcomingTarget {
			t, err := r.Pick(ctx, wts, rng)
			if err != nil {
				return topupMsg{added: added, err: err}
			}
			if queueIDs[t.ID] {
				continue
			}
			if err := m.mp.Append(t.ID); err != nil {
				return topupMsg{added: added, err: err}
			}
			queueIDs[t.ID] = true
			added++
			if added >= 40 { // hard safety cap
				break
			}
		}
		return topupMsg{added: added}
	}
}

// ---------- update ----------

// Update implements tea.Model.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
		return m, nil

	case tickMsg:
		m.ticks++
		if m.playing && m.ticks%15 == 0 {
			return m, m.topupCmd()
		}
		if m.playing && m.dur > 0 && m.elasped < m.dur {
			m.elasped++
		}
		cmds := []tea.Cmd{tick()}
		if m.flash != "" && time.Now().After(m.flashUntil) {
			m.flash = ""
		}
		return m, tea.Batch(cmds...)

	case evMsg:
		return m, m.fetchStatus()

	case errMsg:
		m.errStr = string(msg)
		return m, nil

	case statusMsg:
		if msg.err != nil {
			m.errStr = msg.err.Error()
			return m, nil
		}
		m.errStr = ""
		m.applyStatus(msg)
		return m, nil

	case bootstrapMsg:
		m.bootstrapped = true
		m.syncing = false
		if msg.err != nil {
			m.errStr = fmt.Sprintf("bootstrap: %v", msg.err)
			return m, nil
		}
		m.setFlash(fmt.Sprintf("библиотека: %d треков, лайков: %d", msg.tracks, msg.likes))
		return m, m.topupCmd()

	case syncMsg:
		m.syncing = false
		if msg.err != nil {
			m.errStr = fmt.Sprintf("sync: %v", msg.err)
			return m, nil
		}
		m.setFlash(fmt.Sprintf("синхронизировано: %d треков, лайков: %d", msg.tracks, msg.likes))
		return m, m.fetchStatus()

	case topupMsg:
		if msg.err != nil {
			m.errStr = fmt.Sprintf("волна: %v", msg.err)
			return m, nil
		}
		return m, m.fetchStatus()

	case actionMsg:
		if msg.err != nil {
			m.errStr = msg.err.Error()
			return m, m.fetchStatus()
		}
		if msg.ok != "" {
			m.setFlash(msg.ok)
		}
		return m, m.fetchStatus()

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

// flash sets a transient status-line message (2 s).
func (m *Model) setFlash(s string) {
	m.flash = s
	m.flashUntil = time.Now().Add(2 * time.Second)
}

func (m *Model) applyStatus(msg statusMsg) {
	state := mpdclient.AttrOr(msg.status, "state", "stop")
	m.playing = state == "play"
	m.volume = mpdclient.Int(msg.status, "volume", -1)
	rec := msg.song
	m.curURI = mpdclient.AttrOr(rec, "file", "")
	m.curArtist = mpdclient.AttrOr(rec, "artist", "")
	m.curTitle = mpdclient.AttrOr(rec, "title", "")
	m.songPos = mpdclient.Int(msg.status, "song", -1)
	m.plLen = mpdclient.Int(msg.status, "playlistlength", 0)
	m.elasped = mpdclient.Float(msg.status, "elapsed", 0)
	m.dur = mpdclient.Float(msg.status, "duration", 0)
	if m.dur == 0 {
		m.dur = mpdclient.Float(rec, "duration", 0)
	}
	m.queue = msg.queue
	liked, err := m.st.TrackIsLiked(m.curURI)
	if err == nil {
		m.curLiked = liked
	}
	// refresh data driven by the new state
	switch m.sec {
	case secWave:
		m.wave.items = m.queue
		if m.wave.cursor >= len(m.queue) {
			m.wave.cursor = max(0, len(m.queue)-1)
		}
	case secHistory:
		m.reloadHistory()
	case secStats:
		m.stats.text = m.renderStats()
	}
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (m *Model) reloadHistory() {
	plays, err := m.st.RecentPlays(100)
	if err != nil {
		m.errStr = err.Error()
		return
	}
	m.history.rows = m.history.rows[:0]
	for _, p := range plays {
		t := time.UnixMilli(p.TS).Format("02.01 15:04")
		m.history.rows = append(m.history.rows, fmt.Sprintf("%s  %s — %s",
			styleNum.Render(t), p.Artist, p.Title))
	}
}

// ---------- keys ----------

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()

	// while the search input is focused everything except Tab-routing types
	// into the input
	if m.sec == secLibrary && m.library.input.Focused() {
		if k != "tab" && k != "shift+tab" {
			return m.libraryKey(msg, k)
		}
	}

	// global keys
	switch k {
	case "ctrl+c", "q":
		return m, tea.Quit
	case "tab":
		m.sec = (m.sec + 1) % sectionCount
		return m, m.onSectionChange()
	case "shift+tab":
		m.sec = (m.sec + sectionCount - 1) % sectionCount
		return m, m.onSectionChange()
	case " ":
		return m.playPause()
	case "n":
		return m, m.simple(func(c *mpdclient.Client) error { return c.Next() })
	case "p":
		return m, m.simple(func(c *mpdclient.Client) error { return c.Prev() })
	case "+", "=":
		return m, m.setVolume(m.volume + 5)
	case "-":
		return m, m.setVolume(m.volume - 5)
	case "l":
		return m.likeCurrent()
	case "s", "delete":
		return m.skip()
	case "left":
		return m, m.seek(-5 * time.Second)
	case "right":
		return m, m.seek(5 * time.Second)
	case "shift+left":
		return m, m.seek(-30 * time.Second)
	case "shift+right":
		return m, m.seek(30 * time.Second)
	case "r":
		return m, m.resync()
	case "/":
		m.sec = secLibrary
		m.library.input.Focus()
		return m, textinput.Blink
	}

	// section-specific keys
	switch m.sec {
	case secLibrary:
		return m.libraryKey(msg, k)
	case secWave:
		return m.waveKey(k)
	}
	// history/stats: arrow scrolling
	if k == "up" || k == "k" {
		m.history.scrl--
		if m.history.scrl < 0 {
			m.history.scrl = 0
		}
	} else if k == "down" || k == "j" {
		m.history.scrl++
	}
	return m, nil
}

func (m *Model) onSectionChange() tea.Cmd {
	switch m.sec {
	case secWave:
		return m.fetchStatus()
	case secHistory:
		m.reloadHistory()
		return nil
	case secStats:
		m.stats.text = m.renderStats()
		return nil
	}
	return nil
}

func (m *Model) simple(f func(c *mpdclient.Client) error) tea.Cmd {
	return func() tea.Msg {
		err := f(m.mp)
		return actionMsg{err: err}
	}
}

func (m *Model) playPause() (tea.Model, tea.Cmd) {
	return m, func() tea.Msg {
		var err error
		if m.playing {
			err = m.mp.Pause(true)
		} else {
			err = m.mp.Pause(false)
		}
		return actionMsg{err: err}
	}
}

func (m *Model) setVolume(v int) tea.Cmd {
	return func() tea.Msg {
		if v < 0 {
			v = 0
		}
		if v > 100 {
			v = 100
		}
		return actionMsg{err: m.mp.SetVolume(v)}
	}
}

func (m *Model) seek(offset time.Duration) tea.Cmd {
	label := fmt.Sprintf("+%s", offset)
	if offset < 0 {
		label = fmt.Sprintf("−%s", -offset)
	}
	return func() tea.Msg {
		if err := m.mp.Seek(offset); err != nil {
			return actionMsg{err: err}
		}
		return actionMsg{ok: "перемотка " + label}
	}
}

func (m *Model) likeCurrent() (tea.Model, tea.Cmd) {
	if m.curURI == "" {
		m.setFlash("нет текущего трека")
		return m, nil
	}
	uri := m.curURI
	return m, func() tea.Msg {
		err := m.st.UpsertLike(uri, time.Now().UnixMilli())
		if err == nil {
			return actionMsg{ok: "♥ добавлено в лайки", file: uri}
		}
		return actionMsg{err: err}
	}
}

func (m *Model) skip() (tea.Model, tea.Cmd) {
	if m.curURI == "" {
		m.setFlash("нет текущего трека")
		return m, nil
	}
	return m, func() tea.Msg {
		err := m.obs.RecordSkip()
		if err == nil {
			err = m.mp.Next()
		}
		return actionMsg{err: err}
	}
}

func (m *Model) libraryKey(msg tea.KeyMsg, k string) (tea.Model, tea.Cmd) {
	in := &m.library.input
	if in.Focused() {
		switch k {
		case "esc":
			in.Blur() // keep query/results; 'a' and other keys work on them
			return m, nil
		case "enter":
			return m.libraryEnter()
		case "up":
			if m.library.cursor > 0 {
				m.library.cursor--
			}
			return m, nil
		case "down":
			if m.library.cursor < len(m.library.results)-1 {
				m.library.cursor++
			}
			return m, nil
		case "tab", "shift+tab":
			// fall through to global? tab already handled above only when
			// focused — we got here instead; handle section change
			if k == "tab" {
				in.Blur()
				m.sec = (m.sec + 1) % sectionCount
				return m, m.onSectionChange()
			}
			in.Blur()
			m.sec = (m.sec + sectionCount - 1) % sectionCount
			return m, m.onSectionChange()
		}
		var cmd tea.Cmd
		*in, cmd = in.Update(msg)
		q := strings.TrimSpace(in.Value())
		if q != m.library.query {
			m.library.query = q
			m.library.results = m.library.load(m, q)
			m.library.cursor = firstPlayable(m.library.results)
		}
		return m, cmd
	}
	switch k {
	case "enter":
		return m.libraryEnter()
	case "up", "k":
		if m.library.cursor > 0 {
			m.library.cursor--
		}
	case "down", "j":
		if m.library.cursor < len(m.library.results)-1 {
			m.library.cursor++
		}
	case "a":
		return m.appendSelected()
	case "/":
		in.Focus()
		return m, textinput.Blink
	}
	return m, nil
}

func (m *Model) waveKey(k string) (tea.Model, tea.Cmd) {
	switch k {
	case "up", "k":
		if m.wave.cursor > 0 {
			m.wave.cursor--
		}
	case "down", "j":
		if m.wave.cursor < len(m.wave.items)-1 {
			m.wave.cursor++
		}
	case "enter":
		if m.wave.cursor < len(m.wave.items) {
			it := m.wave.items[m.wave.cursor]
			id := it.id
			return m, func() tea.Msg {
				err := m.mp.PlayID(id)
				return actionMsg{err: err}
			}
		}
	}
	return m, nil
}

// firstPlayable возвращает индекс первого трека (пропускает заголовки артиста/альбома).
func firstPlayable(items []libItem) int {
	for i, it := range items {
		if it.kind == libKindTrack {
			return i
		}
	}
	return 0
}

func (m *Model) libraryEnter() (tea.Model, tea.Cmd) {
	items := m.library.results
	if m.library.cursor >= len(items) {
		return m, nil
	}
	it := items[m.library.cursor]
	switch it.kind {
	case libKindArtist:
		artist := it.track.Artist
		return m, func() tea.Msg {
			tracks, err := m.st.ArtistTracks(artist)
			if err != nil {
				return actionMsg{err: err}
			}
			if len(tracks) == 0 {
				return actionMsg{err: errors.New("у артиста нет треков")}
			}
			if err := m.mp.Clear(); err != nil {
				return actionMsg{err: err}
			}
			for _, t := range tracks {
				if err := m.mp.Append(t.ID); err != nil {
					return actionMsg{err: err}
				}
			}
			if err := m.mp.Play(0); err != nil {
				return actionMsg{err: err}
			}
			return actionMsg{ok: fmt.Sprintf("▶ %s — %d треков", artist, len(tracks))}
		}
	case libKindAlbum:
		artist, album := it.track.Artist, it.track.Album
		return m, func() tea.Msg {
			tracks, err := m.st.AlbumTracks(artist, album)
			if err != nil {
				return actionMsg{err: err}
			}
			if len(tracks) == 0 {
				return actionMsg{err: errors.New("альбом пуст")}
			}
			if err := m.mp.Clear(); err != nil {
				return actionMsg{err: err}
			}
			for _, t := range tracks {
				if err := m.mp.Append(t.ID); err != nil {
					return actionMsg{err: err}
				}
			}
			if err := m.mp.Play(0); err != nil {
				return actionMsg{err: err}
			}
			return actionMsg{ok: fmt.Sprintf("▶ %s — %s (%d треков)", artist, album, len(tracks))}
		}
	case libKindTrack:
		uri := it.track.ID
		return m, func() tea.Msg {
			id, err := m.mp.AppendID(uri)
			if err != nil {
				return actionMsg{err: err}
			}
			if err := m.mp.PlayID(id); err != nil {
				return actionMsg{err: err}
			}
			return actionMsg{ok: "▶ " + it.track.Artist + " — " + it.track.Title}
		}
	}
	return m, nil
}

func (m *Model) appendSelected() (tea.Model, tea.Cmd) {
	items := m.library.results
	if m.library.cursor >= len(items) {
		return m, nil
	}
	it := items[m.library.cursor]
	var uri string
	switch it.kind {
	case libKindTrack:
		uri = it.track.ID
	case libKindAlbum:
		if len(m.library.results) > 0 {
			// find first track of same album
			for _, x := range items {
				if x.kind == libKindTrack && x.track.Album == it.track.Album && x.track.Artist == it.track.Artist {
					uri = x.track.ID
					break
				}
			}
		}
	}
	if uri == "" {
		m.setFlash("выберите трек")
		return m, nil
	}
	return m, func() tea.Msg {
		err := m.mp.Append(uri)
		msg := actionMsg{err: err}
		if err == nil {
			msg.ok = "+ в очередь"
		}
		return msg
	}
}

// ---------- view ----------

// View implements tea.Model.
func (m Model) View() string {
	if m.w == 0 {
		return "загрузка…"
	}
	sideW := 14
	bodyW := m.w - sideW
	if bodyW < 20 {
		bodyW = 20
	}
	bodyH := m.h - 3 // sidebar rows + status bar
	if bodyH < 3 {
		bodyH = 3
	}

	side := m.renderSidebar(bodyH)
	var body string
	switch m.sec {
	case secWave:
		body = m.renderWave(bodyW, bodyH)
	case secLibrary:
		body = m.renderLibrary(bodyW, bodyH)
	case secHistory:
		body = m.renderHistory(bodyW, bodyH)
	case secStats:
		body = m.renderStatsPane(bodyW, bodyH)
	}

	joined := lipgloss.JoinHorizontal(lipgloss.Top, side, body)
	bar := m.renderStatusBar(m.w)
	return joined + "\n" + bar
}

func (m *Model) renderSidebar(h int) string {
	var b strings.Builder
	b.WriteString(styleWordmark.Render("wave") + "\n")
	b.WriteString(styleHelp.Render("──────") + "\n")
	for i, name := range sectionNames {
		if section(i) == m.sec {
			b.WriteString(styleSidebarSel.Render(" "+name) + "\n")
		} else {
			b.WriteString(styleSidebar.Render(" "+name) + "\n")
		}
	}
	// pad to h rows
	lines := strings.Count(b.String(), "\n")
	for lines < h {
		b.WriteString("\n")
		lines++
	}
	return b.String()
}

func (m *Model) renderWave(w, h int) string {
	var b strings.Builder
	b.WriteString(styleSectionTitle.Render("Волна — бесконечное радио") + "\n")
	if m.bootstrapped && len(m.queue) == 0 {
		b.WriteString(styleHelp.Render("очередь пуста, наполняем волной…") + "\n")
	}
	cur := m.songPos
	for i, it := range m.wave.items {
		row := m.waveRow(it, cur, i)
		if row == "" {
			continue
		}
		if i == m.wave.cursor && i != cur {
			row = styleSelectedRow.Render(padTo(row, w))
		}
		b.WriteString(row + "\n")
		if strings.Count(b.String(), "\n") >= h-1 {
			break
		}
	}
	return ensureHeight(b.String(), h)
}

func (m *Model) waveRow(it queueItem, curPos, i int) string {
	name := it.label()
	if name == "—" || name == "" {
		return ""
	}
	if i < curPos {
		return styleNum.Render(fmt.Sprintf("%3d  %s  %s", i+1, name, "(уже сыграно)"))
	}
	var marker string
	if i == curPos {
		if m.playing {
			marker = styleCurrentRow.Render(" ▶ ")
		} else {
			marker = styleCurrentRow.Render(" ⏸ ")
		}
	} else {
		marker = styleNum.Render(fmt.Sprintf(" %2d ", i-curPos))
	}
	dur := ""
	if it.durSec > 0 {
		dur = styleNum.Render(" " + fmtDurSec(it.durSec))
	}
	line := marker + " " + name + dur
	if i == curPos && m.curLiked {
		line += " " + styleLikeMark.Render("♥")
	}
	return line
}

func (m *Model) renderLibrary(w, h int) string {
	var b strings.Builder
	b.WriteString(styleSectionTitle.Render("Библиотека") + "  " + styleHelp.Render("/ — поиск, enter — играть альбом/трек, a — добавить в волну") + "\n")
	if m.library.input.Focused() || m.library.query != "" {
		b.WriteString(m.library.input.View() + "\n")
	} else {
		b.WriteString(styleHelp.Render("нажмите / для поиска") + "\n")
	}
	if m.syncing {
		b.WriteString(styleInfo.Render("синхронизация…") + "\n")
	}
	items := m.library.results
	if len(items) == 0 {
		if m.library.query != "" {
			b.WriteString(styleHelp.Render("ничего не найдено") + "\n")
		} else {
			b.WriteString(styleHelp.Render("введите запрос (например: metallica)") + "\n")
		}
		return ensureHeight(b.String(), h)
	}
	for i, it := range items {
		var row string
		switch it.kind {
		case libKindArtist:
			row = styleHeader.Render(it.label)
		case libKindAlbum:
			row = "  ▸ " + styleInfo.Render(it.label)
		default:
			row = fmt.Sprintf("      · %s  %s", it.label, styleNum.Render(fmtDurMS(it.track.DurationMS)))
		}
		if i == m.library.cursor && it.kind != libKindArtist {
			row = styleSelectedRow.Render(padTo(row, w))
		}
		b.WriteString(row + "\n")
		if strings.Count(b.String(), "\n") >= h-1 {
			break
		}
	}
	return ensureHeight(b.String(), h)
}

func (m *Model) renderHistory(w, h int) string {
	var b strings.Builder
	b.WriteString(styleSectionTitle.Render("История") + "\n")
	if len(m.history.rows) == 0 {
		b.WriteString(styleHelp.Render("пока пусто — слушайте музыку") + "\n")
	}
	for i := m.history.scrl; i < len(m.history.rows); i++ {
		b.WriteString(" " + m.history.rows[i] + "\n")
		if strings.Count(b.String(), "\n") >= h-1 {
			break
		}
	}
	return ensureHeight(b.String(), h)
}

func (m *Model) renderStatsPane(w, h int) string {
	if m.stats.text == "" {
		m.stats.text = m.renderStats()
	}
	lines := strings.Split(m.stats.text, "\n")
	var b strings.Builder
	b.WriteString(styleSectionTitle.Render("Статистика") + "\n")
	for i := m.history.scrl; i < len(lines); i++ {
		b.WriteString(lines[i] + "\n")
		if strings.Count(b.String(), "\n") >= h-1 {
			break
		}
	}
	return ensureHeight(b.String(), h)
}

func (m *Model) renderStats() string {
	var b strings.Builder
	totalMS, _ := m.st.TotalListen()
	plays, _ := m.st.CountPlays()
	skips, _ := m.st.CountSkips()
	b.WriteString(fmt.Sprintf("%s %s   %s %d   %s %d\n",
		styleKbd.Render("прослушано"), humanMS(totalMS),
		styleKbd.Render("треков"), plays,
		styleKbd.Render("пропусков"), skips))

	arts, _ := m.st.TopArtists(10)
	b.WriteString("\n" + styleInfo.Render("ТОП артистов") + "\n")
	for i, a := range arts {
		b.WriteString(fmt.Sprintf(" %2d. %-28s %3d  %s\n", i+1, a.Name, a.Count, styleNum.Render(humanMS(a.ListenMS))))
	}

	tracks, _ := m.st.TopTracks(10)
	b.WriteString("\n" + styleInfo.Render("ТОП треков") + "\n")
	for i, t := range tracks {
		b.WriteString(fmt.Sprintf(" %2d. %s  %d\n", i+1, t.Name, t.Count))
	}

	// 30-day listening sparkline (▁▂▃▄▅▆▇█), oldest → newest.
	days, _ := m.st.ListenPerDay(30)
	b.WriteString("\n" + styleInfo.Render("30 дней") + "  " + sparkline(days) + "\n")

	// Итоги года
	playsY, msY, _ := m.st.YearTotals(time.Now().Year())
	b.WriteString(fmt.Sprintf("\n%s %d: %d треков, %s\n",
		styleLike.Render("Итоги года"), time.Now().Year(), playsY, humanMS(msY)))
	return b.String()
}

func (m *Model) renderStatusBar(w int) string {
	var left strings.Builder
	state := "■"
	switch {
	case m.playing:
		state = "▶"
	case m.dur > 0 || m.songPos >= 0:
		state = "⏸"
	}
	liked := ""
	if m.curLiked {
		liked = " ♥"
	}
	name := strings.TrimSpace(m.curArtist + " — " + m.curTitle)
	if name == "—" {
		name = ""
	}
	left.WriteString(state + " " + name + liked)
	if m.flash != "" {
		return lipgloss.NewStyle().Foreground(cAccent).Width(w).Render(" " + state + " " + name + "   " + m.flash)
	}
	// progress line
	bar := progressBar(m.elasped, m.dur, min(w-1, 92))
	cur := fmtDurSec(m.elasped)
	total := fmtDurSec(m.dur)
	first := fmt.Sprintf("%s  %s  %s / %s", left.String(), bar, cur, total)
	if len(first) < w {
		vol := ""
		if m.volume >= 0 {
			vol = fmt.Sprintf(" vol %d%%", m.volume)
		}
		pad := w - len([]rune(first)) - len(vol)
		if pad < 1 {
			pad = 1
		}
		first += strings.Repeat(" ", pad) + styleHelp.Render(vol)
	}
	barLine := first
	errLine := ""
	if m.errStr != "" {
		errLine = "  " + styleErr.Render("⚠ "+m.errStr)
	}
	return styleStatusBar.Render(truncate(barLine+errLine, w+200))
}

// ---------- helpers ----------

func padTo(s string, w int) string {
	wid := lipgloss.Width(s)
	if wid >= w {
		return s
	}
	return s + strings.Repeat(" ", w-wid)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

func ensureHeight(s string, h int) string {
	lines := strings.Count(s, "\n")
	if lines >= h {
		return s
	}
	return s + strings.Repeat("\n", h-lines)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func fmtDurSec(s float64) string {
	if s <= 0 {
		return "--:--"
	}
	total := int(s)
	m := total / 60
	sec := total % 60
	if m >= 60 {
		return fmt.Sprintf("%d:%02d:%02d", m/60, m%60, sec)
	}
	return fmt.Sprintf("%d:%02d", m, sec)
}

func fmtDurMS(ms int64) string { return fmtDurSec(float64(ms) / 1000) }

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

var sparkChars = []rune("▁▂▃▄▅▆▇█")

func sparkline(days []int64) string {
	var mx int64
	for _, v := range days {
		if v > mx {
			mx = v
		}
	}
	out := make([]rune, 0, len(days))
	for _, v := range days {
		if mx == 0 || v == 0 {
			out = append(out, ' ')
			continue
		}
		idx := int(float64(v) / float64(mx) * float64(len(sparkChars)-1))
		out = append(out, sparkChars[idx])
	}
	return string(out)
}
