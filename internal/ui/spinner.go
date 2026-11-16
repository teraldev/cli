package ui

import (
	"fmt"
	"sync"
	"time"
)

var frames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Spinner shows progress on stderr. Off a terminal it prints the message
// once and stays silent, so CI logs get one line instead of noise.
type Spinner struct {
	io      *IO
	mu      sync.Mutex
	message string
	done    chan struct{}
	stopped chan struct{}
	active  bool
}

// Spin starts a spinner with a message.
func (o *IO) Spin(message string) *Spinner {
	s := &Spinner{io: o, message: message}
	if o.Quiet || o.JSON {
		return s
	}
	if !o.ErrIsTTY() {
		fmt.Fprintf(o.Err, "%s %s\n", o.ErrTheme.Accent.Render("→"), message)
		return s
	}
	s.active = true
	s.done = make(chan struct{})
	s.stopped = make(chan struct{})
	go s.run()
	return s
}

func (s *Spinner) run() {
	defer close(s.stopped)
	ticker := time.NewTicker(80 * time.Millisecond)
	defer ticker.Stop()
	for i := 0; ; i++ {
		s.mu.Lock()
		fmt.Fprintf(s.io.Err, "\r\033[K%s %s", s.io.ErrTheme.Accent.Render(frames[i%len(frames)]), s.message)
		s.mu.Unlock()
		select {
		case <-s.done:
			return
		case <-ticker.C:
		}
	}
}

// Update changes the message.
func (s *Spinner) Update(message string) {
	s.mu.Lock()
	changed := s.message != message
	s.message = message
	s.mu.Unlock()
	if changed && !s.active && !s.io.Quiet && !s.io.JSON {
		fmt.Fprintf(s.io.Err, "%s %s\n", s.io.ErrTheme.Accent.Render("→"), message)
	}
}

// Interrupt clears the spinner line so other output can be printed, then
// resumes on the next tick.
func (s *Spinner) Interrupt(print func()) {
	if !s.active {
		print()
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fmt.Fprint(s.io.Err, "\r\033[K")
	print()
}

// Stop removes the spinner.
func (s *Spinner) Stop() {
	if !s.active {
		return
	}
	s.active = false
	close(s.done)
	<-s.stopped
	fmt.Fprint(s.io.Err, "\r\033[K")
}
