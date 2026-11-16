package ui

import (
	"errors"
	"fmt"

	"github.com/charmbracelet/huh"
)

// ErrCanceled is returned when the user aborts a prompt.
var ErrCanceled = errors.New("canceled")

// NonInteractiveError explains which flag replaces a prompt that cannot be
// shown.
type NonInteractiveError struct {
	What string
	Flag string
}

func (e *NonInteractiveError) Error() string {
	return fmt.Sprintf("%s is required: pass %s (prompts are disabled because this is not an interactive terminal)", e.What, e.Flag)
}

// Option is one choice in a [IO.Select] prompt.
type Option struct {
	Label string
	Value string
}

// Select asks the user to choose one option. flag names the flag that
// answers the question non-interactively.
func (o *IO) Select(title, flag string, options []Option) (string, error) {
	if !o.Interactive() {
		return "", &NonInteractiveError{What: title, Flag: flag}
	}
	if len(options) == 1 {
		return options[0].Value, nil
	}
	opts := make([]huh.Option[string], len(options))
	for i, opt := range options {
		opts[i] = huh.NewOption(opt.Label, opt.Value)
	}
	var value string
	err := huh.NewSelect[string]().Title(title).Options(opts...).Value(&value).Run()
	if errors.Is(err, huh.ErrUserAborted) {
		return "", ErrCanceled
	}
	return value, err
}

// Input asks for a line of text.
func (o *IO) Input(title, flag, placeholder string, validate func(string) error) (string, error) {
	if !o.Interactive() {
		return "", &NonInteractiveError{What: title, Flag: flag}
	}
	var value string
	field := huh.NewInput().Title(title).Placeholder(placeholder).Value(&value)
	if validate != nil {
		field = field.Validate(validate)
	}
	err := field.Run()
	if errors.Is(err, huh.ErrUserAborted) {
		return "", ErrCanceled
	}
	return value, err
}

// Secret asks for a value without echoing it.
func (o *IO) Secret(title, flag string) (string, error) {
	if !o.Interactive() {
		return "", &NonInteractiveError{What: title, Flag: flag}
	}
	var value string
	err := huh.NewInput().Title(title).EchoMode(huh.EchoModePassword).Value(&value).Run()
	if errors.Is(err, huh.ErrUserAborted) {
		return "", ErrCanceled
	}
	return value, err
}

// Confirm asks a yes/no question. Without a terminal it fails unless yes
// (from --yes) is set.
func (o *IO) Confirm(title string, yes bool) error {
	if yes {
		return nil
	}
	if !o.Interactive() {
		return &NonInteractiveError{What: "confirmation", Flag: "--yes"}
	}
	var ok bool
	err := huh.NewConfirm().Title(title).Affirmative("Yes").Negative("No").Value(&ok).Run()
	if errors.Is(err, huh.ErrUserAborted) || (err == nil && !ok) {
		return ErrCanceled
	}
	return err
}
