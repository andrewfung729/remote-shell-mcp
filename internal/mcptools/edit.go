package mcptools

import (
	"bytes"
	"errors"
)

// This file is the exactly-once matcher behind ssh_file_edit. Matching is
// byte-exact — no line-ending, whitespace, or Unicode normalization — so text
// that came out of ssh_file_read matches as it was received, CRLF included.

// errEditNeedsRead refuses an edit the daemon has no content record for.
var errEditNeedsRead = errors.New("no record of this file's content: call ssh_file_read first")

// checkEditTexts rejects edits that cannot be a change at all.
func checkEditTexts(oldText, newText string) error {
	if oldText == "" {
		return errors.New("old_text must not be empty")
	}
	if oldText == newText {
		return errors.New("old_text and new_text are identical: nothing to change")
	}
	return nil
}

// countMatches returns how many byte offsets in hay start needle — overlapping
// starts included, so "aa" in "aaa" counts 2 — and the first such offset, or
// -1 when there is none. A count of 0 or more than 1 is an error the caller
// turns into a tool error; a count of exactly 1 is the edit.
func countMatches(hay, needle []byte) (count, first int) {
	first = -1
	if len(needle) == 0 || len(hay) < len(needle) {
		return 0, -1
	}
	for i := 0; i+len(needle) <= len(hay); i++ {
		if bytes.Equal(hay[i:i+len(needle)], needle) {
			if count == 0 {
				first = i
			}
			count++
		}
	}
	return count, first
}

// applyEdit splices newText over the one match of old at idx. An empty
// newText deletes.
func applyEdit(hay []byte, idx int, old, newText []byte) []byte {
	out := make([]byte, 0, len(hay)-len(old)+len(newText))
	out = append(out, hay[:idx]...)
	out = append(out, newText...)
	out = append(out, hay[idx+len(old):]...)
	return out
}
