package sshx

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"
)

// This file is the remote command-execution layer behind the single
// ssh_shell MCP tool. A Shell wraps one remote login shell (bash -i -l
// when it exists) on a PTY; commands run inside it so cd/env persist
// between calls. Completion is detected by a marker printed after each
// command that also carries the exit code and cwd.

const (
	// markerBody is the payload each post-command marker carries: exit
	// code and cwd.  delimiters are never typed by a user.
	markerBody = "__RSM_<__<e:%d><c:%s>__>__RSM__"
	markerSep  = "\x1f\x01__RSM_<__<e:" // pre-marker: noise before it is discarded
	markerEnd  = "__>__RSM__"
)

var markerRe = regexp.MustCompile("\x1f\x01__RSM_<__<e:([0-9]+)><c:(.*)>__>__RSM__")

// promptState is the bash line that arms completion detection. PROMPT_COMMAND
// prints the marker after every command; PS1 carries a leading newline so
// stray prompt redraws can't corrupt a marker's line.
const promptState = "PS1=''; PROMPT_COMMAND='printf \"\\x1f\\x01__RSM_<__<e:%d><c:%s>__>__RSM__\\n\" \"$?\" \"$PWD\"'"

// bootstrap is typed into bash after rc files load. It overrides prompt
// hooks and PS1 so user rc can't interfere with the marker, disables
// readline echo and bracketed paste, then prints a sentinel — everything
// before the sentinel is rc/banner noise.
const bootstrap = "unset PS2 PS3 PS4 PROMPT_COMMAND; bind 'set enable-bracketed-paste off' 2>/dev/null; stty -echo 2>/dev/null; " +
	promptState + "; printf '\\x1f\\x01__RSM_<__<e:0><c:%s>__>__RSM__\\n' \"$PWD\"\n"

// bootstrapFallback is the bootstrap for bash --norc / sh (no `bind`).
const bootstrapFallback = "unset PS2 PS3 PS4 PROMPT_COMMAND; stty -echo 2>/dev/null; " +
	promptState + "; printf '\\x1f\\x01__RSM_<__<e:0><c:%s>__>__RSM__\\n' \"$PWD\"\n"

// initScript runs in the channel's initial sh: exec interactive login bash
// so rc files load (nvm/conda/pyenv land on PATH).
const initScript = "exec bash -i -l\n"

// degradedScript skips rc entirely: minimal bash, or sh when bash is absent.
const degradedScript = "if command -v bash >/dev/null 2>&1; then exec bash --norc --noprofile; else exec sh; fi\n"

// shellKind records which remote shell a Shell ended up with.
const (
	kindBashRC   = iota // bash -i -l: rc loaded, then overridden by bootstrap
	kindBashBare        // bash --norc --noprofile: rc too slow/broken
	kindSh              // no bash on the host
)

const maxMarkerWait = 10 * time.Second

var errDegradedRetry = errors.New("remote shell init produced no marker within 10s")

// ErrShellBusy means a command is still running on this shell.
var ErrShellBusy = errors.New("shell busy: a command is still running — use poll for output, input to feed it, or interrupt to stop it")

// Shell is a per-host named PTY running a login shell.
type Shell struct {
	Name     string
	Owner    string // MCP clientInfo.name / transport session id
	Kind     int    // shellKind — which remote shell the bootstrap landed on
	Degraded bool   // true when kind != kindBashRC
	OpenedAt time.Time

	parent  *Session
	session *ssh.Session
	stdin   io.Writer
	writeMu sync.Mutex

	mu     sync.Mutex
	out    *shellReader
	closed bool
	doneCh chan struct{}
}

// openShellOn opens channel+PTY on the session's current transport and
// feeds `script` to the initial sh. Shared by OpenShell and openDegraded.
func (sess *Session) openShellOn(name, owner, script string) (*Shell, error) {
	client, err := sess.getClient()
	if err != nil {
		return nil, err
	}
	chSess, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("new ssh channel: %w", err)
	}
	modes := ssh.TerminalModes{
		ssh.ECHO:          1, // stty -echo in the bootstrap turns it off once rc has run
		ssh.TTY_OP_ISPEED: 14400,
		ssh.TTY_OP_OSPEED: 14400,
	}
	if err := chSess.RequestPty("xterm-256color", 24, 200, modes); err != nil {
		_ = chSess.Close()
		return nil, fmt.Errorf("request pty: %w", err)
	}
	stdin, err := chSess.StdinPipe()
	if err != nil {
		_ = chSess.Close()
		return nil, err
	}
	stdout, err := chSess.StdoutPipe()
	if err != nil {
		_ = chSess.Close()
		return nil, err
	}
	stderr, err := chSess.StderrPipe()
	if err != nil {
		_ = chSess.Close()
		return nil, err
	}
	if err := chSess.Start("sh"); err != nil {
		_ = chSess.Close()
		return nil, fmt.Errorf("start shell: %w", err)
	}

	sh := &Shell{
		Name: name, Owner: owner,
		parent: sess, session: chSess, stdin: stdin,
		out:      newShellReader(),
		doneCh:   make(chan struct{}),
		OpenedAt: time.Now(),
	}
	go sh.pump(stdout)
	go sh.pump(stderr)
	go func() {
		_ = chSess.Wait()
		sh.mu.Lock()
		sh.closed = true
		sh.mu.Unlock()
		select {
		case <-sh.doneCh:
		default:
			close(sh.doneCh)
		}
	}()

	if _, err := sh.writeBytes([]byte(script)); err != nil {
		_ = sh.Close()
		return nil, err
	}
	return sh, nil
}

// OpenShell opens the named shell: login bash, bootstrap after rc.
// errDegradedRetry when the rc-loaded bash produced no marker in time.
func (sess *Session) OpenShell(name, owner string) (*Shell, error) {
	sh, err := sess.openShellOn(name, owner, initScript)
	if err != nil {
		return nil, err
	}
	if _, err := sh.writeBytes([]byte(bootstrap)); err != nil {
		_ = sh.Close()
		return nil, err
	}
	sh.Kind = kindBashRC
	if err := sh.out.waitFirstMarker(maxMarkerWait); err != nil {
		_ = sh.Close()
		return nil, errDegradedRetry
	}
	return sh, nil
}

// openDegraded opens a shell that skips rc files entirely. Degraded is
// sticky — a later respawn keeps reporting the missing rc environment.
func (sess *Session) openDegraded(name, owner string) (*Shell, error) {
	sh, err := sess.openShellOn(name, owner, degradedScript)
	if err != nil {
		return nil, err
	}
	if _, err := sh.writeBytes([]byte(bootstrapFallback)); err != nil {
		_ = sh.Close()
		return nil, err
	}
	if err := sh.out.waitFirstMarker(maxMarkerWait); err != nil {
		_ = sh.Close()
		return nil, fmt.Errorf("minimal shell also produced no marker: %w", err)
	}
	sh.Degraded = true
	sh.Kind = kindBashBare
	return sh, nil
}

// OpenOrRespawn returns the named shell owned by owner, opening it if it
// doesn't exist or its PTY died. Second return = respawn (shell_restarted).
func (sess *Session) OpenOrRespawn(name, owner string) (*Shell, bool, error) {
	sess.shellsMu.Lock()
	if sess.closed.Load() {
		sess.shellsMu.Unlock()
		return nil, false, fmt.Errorf("session %q is closed", sess.ID)
	}
	if sh, ok := sess.shells[name]; ok {
		dead := sh.isClosed()
		if !dead {
			if sh.Owner != owner {
				sess.shellsMu.Unlock()
				return nil, false, fmt.Errorf("shell %q not found on session %q", name, sess.ID)
			}
			sess.shellsMu.Unlock()
			return sh, false, nil
		}
		delete(sess.shells, name)
		_ = sh.Close()
	}
	sess.shellsMu.Unlock()

	sh, err := sess.OpenShell(name, owner)
	if err != nil {
		if errors.Is(err, errDegradedRetry) {
			sh, derr := sess.openDegraded(name, owner)
			if derr != nil {
				return nil, false, derr
			}
			sess.registerShell(name, sh)
			return sh, true, nil
		}
		return nil, false, err
	}
	sess.registerShell(name, sh)
	return sh, true, nil
}

// DropShell removes and closes the named shell if the caller owns it —
// used by ssh_shell reset. A foreign owner's shell is untouched.
func (sess *Session) DropShell(name, owner string) {
	sess.shellsMu.Lock()
	defer sess.shellsMu.Unlock()
	if sh, ok := sess.shells[name]; ok && sh.Owner == owner {
		delete(sess.shells, name)
		_ = sh.Close()
	}
}

func (sess *Session) registerShell(name string, sh *Shell) {
	sess.shellsMu.Lock()
	defer sess.shellsMu.Unlock()
	if sess.closed.Load() {
		_ = sh.Close()
		return
	}
	sess.shells[name] = sh
}

func (sh *Shell) writeBytes(b []byte) (int, error) {
	sh.writeMu.Lock()
	defer sh.writeMu.Unlock()
	return sh.stdin.Write(b)
}

func (sh *Shell) pump(r io.Reader) {
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := make([]byte, n)
			copy(chunk, buf[:n])
			sh.out.feed(chunk)
		}
		if err != nil {
			sh.out.close()
			return
		}
	}
}

// Close terminates the remote shell and its channel.
func (sh *Shell) Close() error {
	sh.mu.Lock()
	if sh.closed {
		sh.mu.Unlock()
		return nil
	}
	sh.closed = true
	sh.mu.Unlock()
	_ = sh.session.Close()
	select {
	case <-sh.doneCh:
	default:
		close(sh.doneCh)
	}
	return nil
}

func (sh *Shell) isClosed() bool {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return sh.closed
}

// RunResult is what one ssh_shell call produces.
type RunResult struct {
	Output    string `json:"output"`
	Status    string `json:"status"` // "done" or "running"
	ExitCode  *int   `json:"exit_code,omitempty"`
	CWD       string `json:"cwd,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// Run writes cmd to the shell and collects output until the marker
// arrives or timeout hits. On timeout the command keeps running.
func (sh *Shell) Run(cmd string, timeout time.Duration) (*RunResult, error) {
	if sh.isClosed() {
		return nil, errors.New("shell is closed")
	}
	if !sh.out.beginRun() {
		return nil, ErrShellBusy
	}
	if _, err := sh.writeBytes([]byte(cmd + "\n")); err != nil {
		sh.out.endRun()
		return nil, fmt.Errorf("write command: %w", err)
	}
	res, done := sh.out.collect(timeout)
	if !done {
		return &RunResult{Output: res.Output, Status: "running", Truncated: res.Truncated}, nil
	}
	res.Status = "done"
	return res, nil
}

// Poll returns output accumulated since the last call plus done-ness.
func (sh *Shell) Poll(timeout time.Duration) (*RunResult, error) {
	if sh.isClosed() {
		return nil, errors.New("shell is closed")
	}
	res, done := sh.out.collect(timeout)
	res.Status = "done"
	if !done {
		res.Status = "running"
	}
	return res, nil
}

// Input writes raw bytes to the shell (answers prompts, feeds vim/less).
func (sh *Shell) Input(data string) error {
	if sh.isClosed() {
		return errors.New("shell is closed")
	}
	_, err := sh.writeBytes([]byte(data))
	return err
}

// Interrupt sends Ctrl-C.
func (sh *Shell) Interrupt() error {
	return sh.Input("\x03")
}

// ---------------------------------------------------------------------------
// Output layer: byte stream → cleaned output + marker detection
// ---------------------------------------------------------------------------

type shellReader struct {
	mu       sync.Mutex
	cond     *sync.Cond
	buf      []byte // raw undigested bytes (possible partial marker tail)
	running  bool
	fullOut  []byte // raw bytes of the in-flight command's output
	lastRes  *RunResult
	gotFirst bool
}

func newShellReader() *shellReader {
	r := &shellReader{}
	r.cond = sync.NewCond(&r.mu)
	return r
}

func (r *shellReader) feed(b []byte) {
	r.mu.Lock()
	r.buf = append(r.buf, b...)
	r.digestLocked()
	r.cond.Broadcast()
	r.mu.Unlock()
}

func (r *shellReader) close() {
	r.mu.Lock()
	r.cond.Broadcast()
	r.mu.Unlock()
}

// digestLocked splits buf at every complete marker. Text before a marker is
// the previous command's output (or pre-bootstrap noise, discarded). A tail
// that could grow into a marker stays buffered so a marker split across two
// reads is still detected.
func (r *shellReader) digestLocked() {
	for {
		loc := markerRe.FindSubmatchIndex(r.buf)
		if loc == nil {
			keep := trailingMarkerPrefix(r.buf)
			if r.running {
				r.fullOut = append(r.fullOut, r.buf[:len(r.buf)-keep]...)
			}
			r.buf = append([]byte{}, r.buf[len(r.buf)-keep:]...)
			return
		}
		before := r.buf[:loc[0]]
		// Anything before a markerSep inside `before` is prompt/redraw noise.
		if i := bytes.LastIndex(before, []byte(markerSep)); i >= 0 {
			before = before[i+len(markerSep):]
		}
		exitStr := string(r.buf[loc[2]:loc[3]])
		cwd := string(r.buf[loc[4]:loc[5]])
		r.buf = r.buf[loc[1]:]
		if !r.gotFirst {
			r.gotFirst = true
			r.fullOut = r.fullOut[:0]
			continue // bootstrap sentinel — not a command result
		}
		if r.running {
			r.fullOut = append(r.fullOut, before...)
			code, _ := strconv.Atoi(exitStr)
			out := cleanOutput(r.fullOut)
			res := &RunResult{ExitCode: &code, CWD: cwd}
			if len(out) > maxOutputChars {
				res.Output = headTail(out, maxOutputChars)
				res.Truncated = true
			} else {
				res.Output = out
			}
			r.lastRes = res
			r.fullOut = r.fullOut[:0]
			r.running = false
		}
	}
}

// trailingMarkerPrefix returns the length of a suffix of b that could be
// the start of a marker; those bytes stay buffered.
func trailingMarkerPrefix(b []byte) int {
	prefix := []byte(markerSep)
	max := len(prefix)
	if len(b) < max {
		max = len(b)
	}
	for n := max; n > 0; n-- {
		if bytes.Equal(b[len(b)-n:], prefix[:n]) {
			return n
		}
	}
	return 0
}

func (r *shellReader) waitFirstMarker(d time.Duration) error {
	deadline := time.Now().Add(d)
	r.mu.Lock()
	defer r.mu.Unlock()
	for !r.gotFirst {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return errors.New("no marker within timeout")
		}
		r.waitLocked(remaining)
	}
	return nil
}

func (r *shellReader) waitLocked(d time.Duration) {
	t := time.AfterFunc(d, func() {
		r.mu.Lock()
		r.cond.Broadcast()
		r.mu.Unlock()
	})
	r.cond.Wait()
	t.Stop()
}

func (r *shellReader) beginRun() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running {
		return false
	}
	r.running = true
	r.fullOut = r.fullOut[:0]
	return true
}

func (r *shellReader) endRun() {
	r.mu.Lock()
	r.running = false
	r.mu.Unlock()
}

// collect waits up to d for the in-flight command's marker. done=false
// means the timeout fired; the command keeps running and fullOut keeps
// accumulating for the next call.
func (r *shellReader) collect(d time.Duration) (*RunResult, bool) {
	deadline := time.Now().Add(d)
	r.mu.Lock()
	defer r.mu.Unlock()
	for r.running || len(r.buf) > 0 {
		// A pending marker can still be in buf while running was already
		// cleared by an earlier digest pass — keep waiting until the buffer
		// drains or the deadline hits.
		if !r.running && len(r.buf) == 0 {
			break
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return r.partialLocked(), false
		}
		r.waitLocked(remaining)
	}
	if r.lastRes != nil {
		res := r.lastRes
		r.lastRes = nil
		return res, true
	}
	return r.partialLocked(), true
}

func (r *shellReader) partialLocked() *RunResult {
	out := cleanOutput(r.fullOut)
	res := &RunResult{}
	if len(out) > maxOutputChars {
		res.Output = headTail(out, maxOutputChars)
		res.Truncated = true
	} else {
		res.Output = out
	}
	return res
}

// ---------------------------------------------------------------------------
// Output post-processing (pure helpers — unit-tested)
// ---------------------------------------------------------------------------

const maxOutputChars = 10_000

// ansiRe covers CSI, OSC (…BEL or …ST), DCS/SOS/PM/APC (…ST), and simple
// Fe/Fp escapes — the full set a PTY emits.
var ansiRe = regexp.MustCompile("\x1b(?:\\[[0-9;?]*[ -/]*[@-~]|\\][^\x07]*(?:\x07|\x1b\\\\)|[PX^_][^\x1b]*(?:\x1b\\\\|$)|[@-_])")

// stripANSI removes escape sequences and residual terminal litter
// (CRs, backspace overstrike).
func stripANSI(s string) string {
	s = ansiRe.ReplaceAllString(s, "")
	s = strings.ReplaceAll(s, "\r", "")
	for strings.Contains(s, "\b") {
		i := strings.IndexByte(s, '\b')
		if i == 0 {
			s = s[1:]
			continue
		}
		s = s[:i-1] + s[i+1:]
	}
	return s
}

// cleanOutput strips ANSI junk from a command's raw captured bytes.
func cleanOutput(b []byte) string {
	return strings.TrimLeft(stripANSI(string(b)), "\n")
}

// headTail keeps the first and last limit/2 bytes of s with a truncation
// notice in between. Cuts respect UTF-8 rune boundaries.
func headTail(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	half := limit / 2
	head := s[:utf8Boundary(s, half)]
	tail := s[utf8BoundaryStart(s, len(s)-half):]
	return head + "\n\n… output truncated (" + strconv.Itoa(len(s)) + " bytes; full output available via poll) …\n\n" + tail
}

// utf8Boundary backs n off to the last complete-rune boundary ≤ n.
func utf8Boundary(s string, n int) int {
	if n > len(s) {
		n = len(s)
	}
	for n > 0 && n < len(s) && !utf8.RuneStart(s[n]) {
		n--
	}
	return n
}

// utf8BoundaryStart advances i to the first rune boundary ≥ i.
func utf8BoundaryStart(s string, i int) int {
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return i
}
