package console

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func texts(lines []Line) []string {
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l.Text
	}
	return out
}

func TestBufferRingAndTail(t *testing.T) {
	b := NewBuffer(3)
	for _, s := range []string{"a", "b", "c", "d", "e"} {
		b.Append(s)
	}
	lines, _ := b.Tail(0)
	if !reflect.DeepEqual(texts(lines), []string{"c", "d", "e"}) {
		t.Fatalf("ring content %v", texts(lines))
	}
	if lines[0].Seq != 3 || lines[2].Seq != 5 || b.LastSeq() != 5 {
		t.Fatalf("sequence numbers %+v", lines)
	}
	last, _ := b.Tail(2)
	if !reflect.DeepEqual(texts(last), []string{"d", "e"}) {
		t.Fatalf("tail(2) %v", texts(last))
	}
}

func TestSubscribeResumeWithoutGapOrDuplicate(t *testing.T) {
	b := NewBuffer(100)
	for i := 0; i < 10; i++ {
		b.Append("line")
	}
	epoch := b.Epoch()
	// Reconnect having seen up to seq 7: exactly 8, 9, 10 are replayed.
	backlog, reset, _, sub := b.Subscribe(epoch, 7, 50, 16)
	defer b.Unsubscribe(sub)
	if reset || len(backlog) != 3 || backlog[0].Seq != 8 {
		t.Fatalf("reset=%v backlog=%+v", reset, backlog)
	}
	b.Append("live")
	ev := <-sub.C
	if ev.Line.Seq != 11 || ev.Line.Text != "live" {
		t.Fatalf("live event %+v", ev)
	}
	// Fully caught up: nothing to replay.
	backlog, reset, _, sub2 := b.Subscribe(epoch, 11, 50, 16)
	b.Unsubscribe(sub2)
	if reset || len(backlog) != 0 {
		t.Fatalf("caught-up subscriber got reset=%v backlog=%d", reset, len(backlog))
	}
}

func TestSubscribeResetsWhenGapCannotBeReplayed(t *testing.T) {
	b := NewBuffer(5)
	for i := 0; i < 20; i++ {
		b.Append("x")
	}
	// seq 3 has long been evicted.
	backlog, reset, _, sub := b.Subscribe(b.Epoch(), 3, 4, 16)
	b.Unsubscribe(sub)
	if !reset || len(backlog) != 4 || backlog[3].Seq != 20 {
		t.Fatalf("reset=%v backlog=%+v", reset, backlog)
	}
	// Unknown epoch (buffer was reset or the server restarted).
	_, reset, _, sub = b.Subscribe("stale-epoch", 20, 4, 16)
	b.Unsubscribe(sub)
	if !reset {
		t.Fatal("a stale epoch must force a reset")
	}
	// A sequence number from the future is not trusted either.
	_, reset, _, sub = b.Subscribe(b.Epoch(), 999, 4, 16)
	b.Unsubscribe(sub)
	if !reset {
		t.Fatal("future sequence must force a reset")
	}
}

func TestResetStartsNewEpochAndNotifies(t *testing.T) {
	b := NewBuffer(10)
	b.Append("old")
	old := b.Epoch()
	_, _, _, sub := b.Subscribe(old, 1, 10, 16)
	defer b.Unsubscribe(sub)
	b.Reset()
	ev := <-sub.C
	if !ev.Reset || ev.Epoch == old || b.LastSeq() != 0 {
		t.Fatalf("reset event %+v", ev)
	}
	if lines, _ := b.Tail(0); len(lines) != 0 {
		t.Fatal("buffer should be empty after reset")
	}
}

func TestSlowSubscriberIsDroppedNotBuffered(t *testing.T) {
	b := NewBuffer(1000)
	_, _, _, slow := b.Subscribe("", 0, 0, 16)
	_, _, _, fast := b.Subscribe("", 0, 0, 16)
	var wg sync.WaitGroup
	wg.Add(1)
	got := 0
	go func() {
		defer wg.Done()
		for range fast.C {
			got++
		}
	}()
	for i := 0; i < 500; i++ {
		b.Append("x")
		time.Sleep(50 * time.Microsecond)
	}
	if !slow.Lagged() {
		t.Fatal("the stalled subscriber should have been dropped")
	}
	if _, open := <-slow.C; open {
		// drain what was queued; the channel must end up closed
		for range slow.C {
		}
	}
	if b.Subscribers() != 1 {
		t.Fatalf("subscriber count %d, want 1", b.Subscribers())
	}
	b.Unsubscribe(fast)
	b.Unsubscribe(fast) // idempotent
	wg.Wait()
	if got == 0 {
		t.Fatal("the healthy subscriber must keep receiving")
	}
}

func TestSanitizer(t *testing.T) {
	cases := []struct {
		name, in string
		want     []string
	}{
		{"plain", "hello\nworld\n", []string{"hello", "world"}},
		{"crlf", "a\r\nb\r\n", []string{"a", "b"}},
		{"colours", "\x1b[32mgreen\x1b[0m ok\n", []string{"green ok"}},
		{"cursor and erase", "\x1b[2K\x1b[1Gprompt\n", []string{"prompt"}},
		{"osc title", "\x1b]0;title\x07text\n", []string{"text"}},
		{"osc st", "\x1b]8;;http://x\x1b\\link\n", []string{"link"}},
		{"carriage return overwrite", "10%\r50%\r100%\n", []string{"100%"}},
		{"backspace", "abx\bc\n", []string{"abc"}},
		{"control chars", "a\x00b\x07c\n", []string{"abc"}},
		{"tab kept", "a\tb\n", []string{"a\tb"}},
		{"charset select", "\x1b(Bok\n", []string{"ok"}},
		{"invalid utf8", "bad\xff\xfe\n", []string{"bad�"}},
		{"partial flushed", "no newline", []string{"no newline"}},
	}
	for _, c := range cases {
		got := CleanLines([]byte(c.in), 4096)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestSanitizerSplitsAcrossWritesAndLongLines(t *testing.T) {
	var out []string
	s := NewSanitizer(16, func(l string) { out = append(out, l) })
	// An escape sequence split over two writes must still be removed.
	s.Write([]byte("ab\x1b[3"))
	s.Write([]byte("1mcd\n"))
	if !reflect.DeepEqual(out, []string{"abcd"}) {
		t.Fatalf("split escape: %q", out)
	}
	out = nil
	s.Write([]byte(strings.Repeat("x", 40) + "\n"))
	if len(out) != 3 || len(out[0]) != 16 || len(out[2]) != 8 {
		t.Fatalf("long line split: %q", out)
	}
}

func TestTailerFollowsTruncationAndRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "out.log")
	var mu sync.Mutex
	var got []string
	san := NewSanitizer(4096, func(l string) { mu.Lock(); got = append(got, l); mu.Unlock() })
	tl := NewTailer(path, san)
	snapshot := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), got...) }

	tl.Poll() // file does not exist yet: must not fail
	write := func(s string, flag int) {
		f, err := os.OpenFile(path, flag|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(s)
		f.Close()
	}
	write("one\ntwo\n", os.O_APPEND)
	tl.Poll()
	write("three\n", os.O_APPEND)
	tl.Poll()
	if !reflect.DeepEqual(snapshot(), []string{"one", "two", "three"}) {
		t.Fatalf("append: %q", snapshot())
	}
	// Truncation: the file restarts from zero.
	write("trunc\n", os.O_TRUNC)
	tl.Poll()
	// Rotation: rename and recreate (new inode).
	os.Rename(path, path+".1")
	write("after-rotate\n", os.O_APPEND)
	tl.Poll()
	want := []string{"one", "two", "three", "trunc", "after-rotate"}
	if !reflect.DeepEqual(snapshot(), want) {
		t.Fatalf("got %q, want %q", snapshot(), want)
	}
	// No duplicates on idle polls.
	tl.Poll()
	tl.Poll()
	if len(snapshot()) != len(want) {
		t.Fatal("idle polls must not re-deliver lines")
	}
}

func TestTailerSkipToEndAndBoundedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cap.log")
	os.WriteFile(path, []byte("old1\nold2\nold3\n"), 0o600)
	var got []string
	tl := NewTailer(path, NewSanitizer(4096, func(l string) { got = append(got, l) }))
	pre := tl.SkipToEnd(1 << 20)
	if string(pre) != "old1\nold2\nold3\n" {
		t.Fatalf("preload %q", pre)
	}
	tl.Poll()
	if len(got) != 0 {
		t.Fatal("history must not be replayed as new output")
	}
	// The capture file is truncated once it has been consumed past MaxBytes.
	tl.MaxBytes = 64
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	defer f.Close()
	f.WriteString(strings.Repeat("y", 100) + "\n")
	tl.Poll()
	if st, _ := os.Stat(path); st.Size() != 0 {
		t.Fatalf("file should have been truncated, size %d", st.Size())
	}
	// The writer's O_APPEND descriptor continues at the new end.
	f.WriteString("fresh\n")
	tl.Poll()
	if got[len(got)-1] != "fresh" {
		t.Fatalf("output after truncation lost: %q", got)
	}
}
