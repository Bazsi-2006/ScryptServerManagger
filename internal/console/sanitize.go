package console

import (
	"strings"
	"sync"
)

// Sanitizer turns a raw byte stream (possibly terminal output with ANSI
// escape sequences, carriage returns and backspaces) into clean text lines.
type Sanitizer struct {
	mu      sync.Mutex
	maxLine int
	emit    func(string)
	line    []byte
	state   int
	sawCR   bool
}

const (
	stText = iota
	stEsc
	stCSI
	stOSC
	stOSCEsc
	stSkipOne
)

// NewSanitizer emits each completed line through emit. Lines longer than
// maxLine bytes are split.
func NewSanitizer(maxLine int, emit func(string)) *Sanitizer {
	if maxLine < 16 {
		maxLine = 16
	}
	return &Sanitizer{maxLine: maxLine, emit: emit}
}

func (s *Sanitizer) flushLocked() {
	text := strings.ToValidUTF8(string(s.line), "�")
	s.line = s.line[:0]
	s.emit(text)
}

// Write feeds raw output. It never fails.
func (s *Sanitizer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range p {
		switch s.state {
		case stEsc:
			switch c {
			case '[':
				s.state = stCSI
			case ']', 'P', '^', '_', 'X':
				s.state = stOSC // OSC / DCS / PM / APC / SOS: skip to terminator
			case '(', ')', '*', '+', '#', '%', ' ':
				s.state = stSkipOne
			default:
				s.state = stText
			}
			continue
		case stCSI:
			if c >= 0x40 && c <= 0x7e {
				s.state = stText
			}
			continue
		case stOSC:
			if c == 0x07 {
				s.state = stText
			} else if c == 0x1b {
				s.state = stOSCEsc
			}
			continue
		case stOSCEsc:
			if c == '\\' {
				s.state = stText
			} else if c != 0x1b {
				s.state = stOSC
			}
			continue
		case stSkipOne:
			s.state = stText
			continue
		}
		if s.sawCR {
			s.sawCR = false
			if c != '\n' {
				// A bare carriage return rewinds to the start of the line;
				// what was there is about to be overwritten.
				s.line = s.line[:0]
			}
		}
		switch {
		case c == '\n':
			s.flushLocked()
		case c == '\r':
			s.sawCR = true
		case c == 0x1b:
			s.state = stEsc
		case c == '\b':
			if n := len(s.line); n > 0 {
				s.line = s.line[:n-1]
			}
		case c == '\t':
			s.line = append(s.line, c)
		case c < 0x20 || c == 0x7f:
			// drop other control characters
		default:
			s.line = append(s.line, c)
			if len(s.line) >= s.maxLine {
				s.flushLocked()
			}
		}
	}
	return len(p), nil
}

// Flush emits a pending partial line (e.g. an interactive prompt that is not
// newline-terminated). It reports whether anything was emitted.
func (s *Sanitizer) Flush() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.line) == 0 || s.state != stText {
		return false
	}
	s.sawCR = false
	s.flushLocked()
	return true
}

// Pending reports whether a partial line is buffered.
func (s *Sanitizer) Pending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.line) > 0
}

// CleanLines sanitizes a complete chunk of output and returns its lines.
func CleanLines(data []byte, maxLine int) []string {
	var out []string
	s := NewSanitizer(maxLine, func(l string) { out = append(out, l) })
	_, _ = s.Write(data)
	s.Flush()
	return out
}
