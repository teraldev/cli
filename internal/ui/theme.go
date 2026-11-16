package ui

import (
	"os"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
)

// Theme holds the CLI's styles for one output stream.
type Theme struct {
	Accent  lipgloss.Style
	Success lipgloss.Style
	Warning lipgloss.Style
	Danger  lipgloss.Style
	Muted   lipgloss.Style
	Bold    lipgloss.Style
	Code    lipgloss.Style
	Link    lipgloss.Style
	Header  lipgloss.Style
}

var (
	accent  = lipgloss.AdaptiveColor{Light: "#5B3DF5", Dark: "#9D8CFF"}
	success = lipgloss.AdaptiveColor{Light: "#15803D", Dark: "#4ADE80"}
	warning = lipgloss.AdaptiveColor{Light: "#B45309", Dark: "#FBBF24"}
	danger  = lipgloss.AdaptiveColor{Light: "#B91C1C", Dark: "#F87171"}
	muted   = lipgloss.AdaptiveColor{Light: "#6B7280", Dark: "#9CA3AF"}
)

func newTheme(r *lipgloss.Renderer, tty bool) Theme {
	if !tty || os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		r.SetColorProfile(termenv.Ascii)
	}
	return Theme{
		Accent:  r.NewStyle().Foreground(accent),
		Success: r.NewStyle().Foreground(success),
		Warning: r.NewStyle().Foreground(warning),
		Danger:  r.NewStyle().Foreground(danger),
		Muted:   r.NewStyle().Foreground(muted),
		Bold:    r.NewStyle().Bold(true),
		Code:    r.NewStyle().Foreground(accent),
		Link:    r.NewStyle().Foreground(accent).Underline(true),
		Header:  r.NewStyle().Foreground(muted).Bold(true),
	}
}

// Status colors a deployment status.
func (t Theme) Status(status string) string {
	switch status {
	case "ready":
		return t.Success.Render("● " + status)
	case "failed", "cancelled":
		return t.Danger.Render("● " + status)
	case "stopped", "superseded", "skipped":
		return t.Muted.Render("○ " + status)
	case "awaiting_approval":
		return t.Warning.Render("◐ awaiting approval")
	default:
		return t.Accent.Render("◐ " + status)
	}
}
