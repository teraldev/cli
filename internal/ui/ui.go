// Package ui renders the CLI's output: styled status lines on stderr, data
// (tables, values, JSON) on stdout, spinners, and interactive prompts.
//
// Output adapts to where it goes. Colors and spinners appear only on a
// terminal and honor NO_COLOR; prompts appear only when stdin and stdout are
// terminals and no CI environment is detected, otherwise commands fail with
// a hint naming the flag to pass instead.
package ui

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-isatty"
)

// IO bundles the streams and output mode of one CLI invocation.
type IO struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer

	// JSON switches data output to JSON and silences decoration.
	JSON bool
	// Quiet silences status lines.
	Quiet bool

	outTTY, errTTY, inTTY bool
	ci                    bool

	// Theme styles stdout; ErrTheme styles stderr.
	Theme    Theme
	ErrTheme Theme
}

// System returns an IO bound to the process's standard streams.
func System() *IO {
	io := &IO{
		In:     os.Stdin,
		Out:    os.Stdout,
		Err:    os.Stderr,
		outTTY: isTerminal(os.Stdout),
		errTTY: isTerminal(os.Stderr),
		inTTY:  isTerminal(os.Stdin),
		ci:     isCI(),
	}
	io.Theme = newTheme(lipgloss.NewRenderer(os.Stdout), io.outTTY)
	io.ErrTheme = newTheme(lipgloss.NewRenderer(os.Stderr), io.errTTY)
	return io
}

// Interactive reports whether the user can answer prompts.
func (o *IO) Interactive() bool {
	return o.inTTY && o.outTTY && !o.ci && !o.JSON
}

// IsTTY reports whether stdout is a terminal.
func (o *IO) IsTTY() bool { return o.outTTY }

// ErrIsTTY reports whether stderr is a terminal (spinners, progress bars).
func (o *IO) ErrIsTTY() bool { return o.errTTY && !o.JSON }

// CI reports whether a CI environment was detected.
func (o *IO) CI() bool { return o.ci }

func (o *IO) status(symbol string, style lipgloss.Style, format string, args ...any) {
	if o.Quiet || o.JSON {
		return
	}
	fmt.Fprintf(o.Err, "%s %s\n", style.Render(symbol), fmt.Sprintf(format, args...))
}

// Success prints "✓ message".
func (o *IO) Success(format string, args ...any) {
	o.status("✓", o.ErrTheme.Success, format, args...)
}

// Info prints "• message".
func (o *IO) Info(format string, args ...any) {
	o.status("•", o.ErrTheme.Muted, format, args...)
}

// Step prints "→ message", for progress through a multi-step command.
func (o *IO) Step(format string, args ...any) {
	o.status("→", o.ErrTheme.Accent, format, args...)
}

// Warn prints "! message". Warnings show even in quiet mode.
func (o *IO) Warn(format string, args ...any) {
	if o.JSON {
		fmt.Fprintf(o.Err, "warning: %s\n", fmt.Sprintf(format, args...))
		return
	}
	fmt.Fprintf(o.Err, "%s %s\n", o.ErrTheme.Warning.Render("!"), fmt.Sprintf(format, args...))
}

// Hint prints a dimmed follow-up suggestion.
func (o *IO) Hint(format string, args ...any) {
	if o.Quiet || o.JSON {
		return
	}
	fmt.Fprintf(o.Err, "  %s\n", o.ErrTheme.Muted.Render(fmt.Sprintf(format, args...)))
}

// Blank prints an empty status line.
func (o *IO) Blank() {
	if o.Quiet || o.JSON {
		return
	}
	fmt.Fprintln(o.Err)
}

// Println writes a data line to stdout.
func (o *IO) Println(a ...any) { fmt.Fprintln(o.Out, a...) }

// Printf writes formatted data to stdout.
func (o *IO) Printf(format string, a ...any) { fmt.Fprintf(o.Out, format, a...) }

// PrintJSON writes v as indented JSON to stdout.
func (o *IO) PrintJSON(v any) error {
	enc := json.NewEncoder(o.Out)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

// Code styles inline code for stderr, e.g. a command to run next.
func (o *IO) Code(s string) string { return o.ErrTheme.Code.Render(s) }

// Link renders a URL for stderr.
func (o *IO) Link(url string) string { return o.ErrTheme.Link.Render(url) }

// Heading prints a bold title line to stderr.
func (o *IO) Heading(title string) {
	if o.Quiet || o.JSON {
		return
	}
	fmt.Fprintln(o.Err, o.ErrTheme.Bold.Render(title))
}

func isTerminal(f *os.File) bool {
	return isatty.IsTerminal(f.Fd()) || isatty.IsCygwinTerminal(f.Fd())
}

// isCI detects common CI environments, where prompts can never be answered.
func isCI() bool {
	for _, name := range []string{"CI", "GITHUB_ACTIONS", "GITLAB_CI", "BUILDKITE", "CIRCLECI", "JENKINS_URL", "TEAMCITY_VERSION", "TF_BUILD"} {
		if v := os.Getenv(name); v != "" && !strings.EqualFold(v, "false") && v != "0" {
			return true
		}
	}
	return false
}

// New returns an IO on arbitrary streams, without colors or prompts. Tests
// and embedding use it.
func New(in io.Reader, out, err io.Writer) *IO {
	o := &IO{In: in, Out: out, Err: err}
	o.Theme = newTheme(lipgloss.NewRenderer(out), false)
	o.ErrTheme = newTheme(lipgloss.NewRenderer(err), false)
	return o
}
