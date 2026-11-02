package tui

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	mpdgompd "github.com/fhs/gompd/v2/mpd"

	"wave/internal/daemon"
	mpdclient "wave/internal/mpd"
	"wave/internal/store"
)

// Run starts the wave TUI and blocks until the user quits.
func Run() error {
	st, err := store.Open(store.DefaultPath())
	if err != nil {
		return fmt.Errorf("db: %w", err)
	}
	defer st.Close()

	mp, err := mpdclient.Dial(mpdAddr)
	if err != nil {
		return fmt.Errorf("mpd: %w", err)
	}
	defer mp.Close()

	obs := daemon.NewObserver(st)
	model := NewModel(st, mp, obs)
	prog := tea.NewProgram(model, tea.WithAltScreen())

	watcher, err := mpdgompd.NewWatcher("tcp", mpdAddr, "", "player", "mixer", "playlist")
	if err != nil {
		return fmt.Errorf("mpd watcher: %w", err)
	}
	go func() {
		defer watcher.Close()
		for {
			select {
			case ev, ok := <-watcher.Event:
				if !ok {
					return
				}
				prog.Send(evMsg(ev))
			case e, ok := <-watcher.Error:
				if !ok {
					return
				}
				prog.Send(errMsg("mpd: " + e.Error()))
			}
		}
	}()

	_, err = prog.Run()
	return err
}

