package tui

import (
	"fmt"
	"sort"
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/lipgloss"
	"github.com/lucasb-eyer/go-colorful"
)

// library item kinds.
const (
	libKindArtist = 0
	libKindAlbum  = 1
	libKindTrack  = 2
)

type libPane struct {
	input   textinput.Model
	query   string
	results []libItem
	cursor  int
}

type wavePane struct {
	items  []queueItem
	cursor int
}

type histPane struct {
	rows []string
	scrl int
}

type statsPane struct {
	text string
}

// label is the display text of a queue item.
func (it queueItem) label() string {
	name := strings.TrimSpace(it.artist)
	if name == "" {
		name = it.title
	} else {
		name += " — " + it.title
	}
	return name
}

// load performs the search and flattens it into grouped items.
func (p *libPane) load(m *Model, q string) []libItem {
	tracks, err := m.st.SearchTracks(q, 500)
	if err != nil {
		m.errStr = err.Error()
		return nil
	}
	var out []libItem
	lastArtist, lastAlbum := "", ""
	for _, t := range tracks {
		if t.Artist != lastArtist {
			out = append(out, libItem{kind: libKindArtist, track: t, label: artistLabel(t.Artist), depth: 0})
			lastArtist = t.Artist
			lastAlbum = ""
		}
		if t.Album != lastAlbum {
			out = append(out, libItem{kind: libKindAlbum, track: t, label: t.Album, depth: 1})
			lastAlbum = t.Album
		}
		out = append(out, libItem{kind: libKindTrack, track: t, label: t.Title, depth: 2})
	}
	return out
}

func artistLabel(a string) string { return "◈ " + a }

// progressBar renders elapsed/dur as a gradient bar of the given width.
func progressBar(elapsed, dur float64, width int) string {
	if width < 4 {
		return ""
	}
	if dur <= 0 {
		return strings.Repeat(styleHelp.Render("─"), width)
	}
	frac := elapsed / dur
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	filled := int(frac * float64(width))
	from, _ := colorful.Hex("#8E7CD8")
	to, _ := colorful.Hex("#D7FF87")
	var b strings.Builder
	for i := 0; i < width; i++ {
		t := float64(i) / float64(width-1)
		c := from.BlendLab(to, t)
		if i < filled {
			b.WriteString(lipgloss.NewStyle().Foreground(lipgloss.Color(c.Hex())).Render("━"))
		} else {
			b.WriteString(styleHelp.Render("╌"))
		}
	}
	return b.String()
}

// fmtShares is kept for debug lines in the wave pane title.
func fmtShares(artist string) string { return artist }

var _ = fmt.Sprintf
var _ = sort.Strings
