// Package tui renders the wave UI (bubbletea + lipgloss).
package tui

import "github.com/charmbracelet/lipgloss"

// Yandex-Music-inspired palette on a dark base: acid-green + violet accents,
// all ANSI256-safe for a plain 256-color terminal.
var (
	cBG     = lipgloss.Color("235")
	cText   = lipgloss.Color("252")
	cDim    = lipgloss.Color("245")
	cAccent = lipgloss.Color("154") // acid green
	cViolet = lipgloss.Color("141")
	cSelBg  = lipgloss.Color("237")
	cSelFg  = lipgloss.Color("157")
	cPink   = lipgloss.Color("212")
	cRed    = lipgloss.Color("203")
)

var (
	styleBase = lipgloss.NewStyle().Foreground(cText)

	styleWordmark = lipgloss.NewStyle().
			Foreground(cAccent).
			Bold(true)

	styleSidebar = lipgloss.NewStyle().
			Foreground(cDim).
			Padding(0, 1)

	styleSidebarSel = lipgloss.NewStyle().
			Foreground(lipgloss.Color("16")).
			Background(cViolet).
			Bold(true).
			Padding(0, 1)

	styleHeader = lipgloss.NewStyle().
			Foreground(cViolet).
			Bold(true)

	styleSelectedRow = lipgloss.NewStyle().
				Foreground(cSelFg).
				Background(cSelBg)

	styleCurrentRow = lipgloss.NewStyle().
			Foreground(lipgloss.Color("16")).
			Background(cAccent).
			Bold(true)

	styleLikeMark = lipgloss.NewStyle().Foreground(cPink).Bold(true)

	styleStatusBar = lipgloss.NewStyle().
			Border(lipgloss.Border{Top: "─"}, false, false, false, false).
			BorderForeground(cViolet).
			Padding(0, 1)

	styleHelp = lipgloss.NewStyle().Foreground(cDim)

	styleErr  = lipgloss.NewStyle().Foreground(cRed)
	styleInfo = lipgloss.NewStyle().Foreground(cViolet)

	styleNum = lipgloss.NewStyle().Foreground(cDim)
	styleKbd = lipgloss.NewStyle().Foreground(cAccent)

	styleLike = lipgloss.NewStyle().
			Foreground(cAccent).
			Bold(true)

	styleSectionTitle = lipgloss.NewStyle().
				Foreground(cAccent).
				Bold(true).
				MarginBottom(0)
)
