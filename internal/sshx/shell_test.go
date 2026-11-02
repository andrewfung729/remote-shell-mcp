package sshx

// Shell ownership scoping lives in OpenOrRespawn/DropShell — exercised
// end-to-end in test/e2e. This file holds the Seam-2 unit tests for the
// pure helpers of the command-execution layer (issue #14).

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

const bs = "\x1f\x01__RSM_<__<e:0><c:/>__>__RSM__\n" // bootstrap marker

func TestStripANSI(t *testing.T) {
	in := "plain \x1b[32mgreen\x1b[0m \x1b[1;31mbold-red\x1b[m done"
	if got := stripANSI(in); got != "plain green bold-red done" {
		t.Fatalf("got %q", got)
	}
	// OSC title sequence ending in BEL
	if got := stripANSI("\x1b]0;my title\x07body"); got != "body" {
		t.Fatalf("osc: %q", got)
	}
	// OSC ending in ST
	if got := stripANSI("\x1b]0;t\x1b\\body"); got != "body" {
		t.Fatalf("osc st: %q", got)
	}
	// CR removal + backspace overstrike
	if got := stripANSI("ab\r\nc\bd"); got != "ab\nd" {
		t.Fatalf("cr/bs: %q", got)
	}
}

func TestHeadTailTruncation(t *testing.T) {
	s := strings.Repeat("x", 5000) + strings.Repeat("y", 10000) + strings.Repeat("z", 5000)
	got := headTail(s, maxOutputChars)
	if !strings.HasPrefix(got, strings.Repeat("x", 5000)) {
		t.Fatal("head missing")
	}
	if !strings.HasSuffix(got, strings.Repeat("z", 5000)) {
		t.Fatal("tail missing")
	}
	if !strings.Contains(got, "output truncated") {
		t.Fatal("no truncation notice")
	}
}

func TestHeadTailMultibyte(t *testing.T) {
	head := strings.Repeat("世", 2000) // 6000 bytes, rune-boundary hazard
	s := head + strings.Repeat("a", 9000) + "tail-end"
	got := headTail(s, maxOutputChars)
	if !utf8.ValidString(got) {
		t.Fatal("truncated output is not valid UTF-8")
	}
	if !strings.HasSuffix(got, "tail-end") {
		t.Fatal("tail lost")
	}
	if !strings.Contains(got, "output truncated") {
		t.Fatal("no notice")
	}
	if len(got) > maxOutputChars+200 {
		t.Fatalf("too long: %d", len(got))
	}
}

func TestMarkerParsing(t *testing.T) {
	r := newShellReader()
	r.feed([]byte("\x1f\x01__RSM_<__<e:0><c:/root>__>__RSM__\n")) // bootstrap
	if !r.gotFirst {
		t.Fatal("first marker not detected")
	}
	if !r.beginRun() {
		t.Fatal("beginRun failed")
	}
	r.feed([]byte("hello output\nmore\n"))
	r.feed([]byte("\x1f\x01__RSM_<__<e:3><c:/tmp/x>__>__RSM__\n"))
	res, done := r.collect(200 * time.Millisecond)
	if !done {
		t.Fatal("marker not collected")
	}
	if res.ExitCode == nil || *res.ExitCode != 3 {
		t.Fatalf("exit code %v", res.ExitCode)
	}
	if res.CWD != "/tmp/x" {
		t.Fatalf("cwd %q", res.CWD)
	}
	if !strings.Contains(res.Output, "hello output") {
		t.Fatalf("output %q", res.Output)
	}
}

func TestMarkerSplitAcrossFeeds(t *testing.T) {
	r := newShellReader()
	r.feed([]byte(bs))
	r.beginRun()
	r.feed([]byte("out\n\x1f\x01__RSM_<__<e:")) // partial marker
	r.feed([]byte("7><c:/home>__>__RSM__\n"))
	res, done := r.collect(200 * time.Millisecond)
	if !done || res.ExitCode == nil || *res.ExitCode != 7 || res.CWD != "/home" {
		t.Fatalf("split marker lost: %+v done=%v", res, done)
	}
}

func TestBusyDuringRun(t *testing.T) {
	r := newShellReader()
	r.feed([]byte(bs))
	r.beginRun()
	if r.beginRun() {
		t.Fatal("second beginRun should fail while running")
	}
	r.feed([]byte(bs))
	if !r.beginRun() {
		t.Fatal("beginRun after completion should work")
	}
}

func TestCollectTimeoutKeepsPartial(t *testing.T) {
	r := newShellReader()
	r.feed([]byte(bs))
	r.beginRun()
	r.feed([]byte("partial data\n"))
	res, done := r.collect(50 * time.Millisecond)
	if done {
		t.Fatal("expected timeout")
	}
	if !strings.Contains(res.Output, "partial data") {
		t.Fatalf("partial lost: %q", res.Output)
	}
	r.feed([]byte(bs))
	res2, done2 := r.collect(200 * time.Millisecond)
	if !done2 || res2.ExitCode == nil {
		t.Fatalf("final result lost: %+v", res2)
	}
}

func TestBannerNoiseBeforeMarkerDiscarded(t *testing.T) {
	r := newShellReader()
	r.feed([]byte("BIG BANNER\nmotd text\n")) // pre-bootstrap junk
	r.feed([]byte(bs))
	if !r.gotFirst {
		t.Fatal("bootstrap marker not detected")
	}
	r.beginRun()
	r.feed([]byte("clean\n"))
	r.feed([]byte("\x1f\x01__RSM_<__<e:0><c:/root>__>__RSM__\n"))
	res, done := r.collect(200 * time.Millisecond)
	if !done {
		t.Fatal("marker not collected")
	}
	if strings.Contains(res.Output, "BANNER") || strings.Contains(res.Output, "motd") {
		t.Fatalf("banner noise leaked into output: %q", res.Output)
	}
	if !strings.Contains(res.Output, "clean") {
		t.Fatalf("output lost: %q", res.Output)
	}
}
