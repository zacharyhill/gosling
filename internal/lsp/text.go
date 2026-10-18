package lsp

import (
	"bytes"
	"unicode/utf16"
	"unicode/utf8"
)

// position is an LSP position. Character counts UTF-16 code units, the
// default LSP encoding.
type position struct {
	Line      int `json:"line"`
	Character int `json:"character"`
}

type lspRange struct {
	Start position `json:"start"`
	End   position `json:"end"`
}

// offsetAt returns the byte offset of target in text. A character past the
// end of the line maps to the line end, as the LSP specification requires.
func offsetAt(text []byte, target position) (int, bool) {
	lineStart := 0
	for line := 0; line < target.Line; line++ {
		newline := bytes.IndexByte(text[lineStart:], '\n')
		if newline < 0 {
			return 0, false
		}
		lineStart += newline + 1
	}
	offset := lineStart
	for units := 0; units < target.Character && offset < len(text) && text[offset] != '\n'; {
		codePoint, size := utf8.DecodeRune(text[offset:])
		units += utf16.RuneLen(codePoint)
		if units > target.Character {
			break
		}
		offset += size
	}
	return offset, true
}

func positionAt(text []byte, offset int) position {
	offset = min(max(offset, 0), len(text))
	lineStart := bytes.LastIndexByte(text[:offset], '\n') + 1
	character := 0
	for _, codePoint := range string(text[lineStart:offset]) {
		character += utf16.RuneLen(codePoint)
	}
	return position{Line: bytes.Count(text[:offset], []byte("\n")), Character: character}
}

// maxDiffCells limits the size of the line table in diffText. A bigger
// change maps only the text before and after it.
const maxDiffCells = 4_000_000

// textShift maps offsets between two versions of a text. It holds the runs
// of equal lines, in order.
type textShift struct {
	matches []matchedRun
}

// matchedRun is text that is equal in both versions.
type matchedRun struct {
	oldStart, newStart, length int
}

// diffText matches the equal text of two texts. It matches equal lines,
// then the equal characters at the start and end of each changed region.
func diffText(oldText, newText []byte) textShift {
	return textShift{matches: refineGaps(matchLineRuns(oldText, newText), oldText, newText)}
}

// matchLineRuns matches the equal lines of two texts. It trims the common prefix
// and suffix, then finds the longest common subsequence of the lines
// between them.
func matchLineRuns(oldText, newText []byte) []matchedRun {
	oldLines, newLines := splitLines(oldText), splitLines(newText)
	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && bytes.Equal(oldLines[prefix], newLines[prefix]) {
		prefix++
	}
	suffix := 0
	for suffix < len(oldLines)-prefix && suffix < len(newLines)-prefix &&
		bytes.Equal(oldLines[len(oldLines)-1-suffix], newLines[len(newLines)-1-suffix]) {
		suffix++
	}
	pairs := make([][2]int, 0, prefix+suffix)
	for lineIndex := range prefix {
		pairs = append(pairs, [2]int{lineIndex, lineIndex})
	}
	pairs = append(pairs, matchLines(oldLines[prefix:len(oldLines)-suffix], newLines[prefix:len(newLines)-suffix], prefix)...)
	for lineIndex := suffix; lineIndex > 0; lineIndex-- {
		pairs = append(pairs, [2]int{len(oldLines) - lineIndex, len(newLines) - lineIndex})
	}
	return runsFromPairs(pairs, oldLines, newLines)
}

// splitLines splits text after each newline. The last line has no newline.
func splitLines(text []byte) [][]byte {
	return bytes.SplitAfter(text, []byte("\n"))
}

// refineGaps adds runs for the equal characters at the start and end of
// each changed region, so an edit in a line keeps the rest of the line.
func refineGaps(runs []matchedRun, oldText, newText []byte) []matchedRun {
	var refined []matchedRun
	oldOffset, newOffset := 0, 0
	for _, run := range append(runs, matchedRun{oldStart: len(oldText), newStart: len(newText)}) {
		refined = append(refined, gapRuns(oldText[oldOffset:run.oldStart], newText[newOffset:run.newStart], oldOffset, newOffset)...)
		if run.length > 0 {
			refined = append(refined, run)
		}
		oldOffset, newOffset = run.oldStart+run.length, run.newStart+run.length
	}
	return refined
}

// gapRuns matches the common prefix and suffix of one changed region.
func gapRuns(oldGap, newGap []byte, oldOffset, newOffset int) []matchedRun {
	prefix := 0
	for prefix < len(oldGap) && prefix < len(newGap) && oldGap[prefix] == newGap[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(oldGap)-prefix && suffix < len(newGap)-prefix && oldGap[len(oldGap)-1-suffix] == newGap[len(newGap)-1-suffix] {
		suffix++
	}
	var runs []matchedRun
	if prefix > 0 {
		runs = append(runs, matchedRun{oldStart: oldOffset, newStart: newOffset, length: prefix})
	}
	if suffix > 0 {
		runs = append(runs, matchedRun{oldStart: oldOffset + len(oldGap) - suffix, newStart: newOffset + len(newGap) - suffix, length: suffix})
	}
	return runs
}

// matchLines returns the index pairs of a longest common subsequence of
// oldLines and newLines. It adds base to each index.
func matchLines(oldLines, newLines [][]byte, base int) [][2]int {
	if len(oldLines)*len(newLines) > maxDiffCells || len(oldLines) == 0 || len(newLines) == 0 {
		return nil
	}
	width := len(newLines) + 1
	lengths := make([]int32, (len(oldLines)+1)*width)
	for oldIndex := len(oldLines) - 1; oldIndex >= 0; oldIndex-- {
		for newIndex := len(newLines) - 1; newIndex >= 0; newIndex-- {
			cell := oldIndex*width + newIndex
			if bytes.Equal(oldLines[oldIndex], newLines[newIndex]) {
				lengths[cell] = lengths[cell+width+1] + 1
			} else {
				lengths[cell] = max(lengths[cell+width], lengths[cell+1])
			}
		}
	}
	var pairs [][2]int
	for oldIndex, newIndex := 0, 0; oldIndex < len(oldLines) && newIndex < len(newLines); {
		switch cell := oldIndex*width + newIndex; {
		case bytes.Equal(oldLines[oldIndex], newLines[newIndex]):
			pairs = append(pairs, [2]int{base + oldIndex, base + newIndex})
			oldIndex++
			newIndex++
		case lengths[cell+width] >= lengths[cell+1]:
			oldIndex++
		default:
			newIndex++
		}
	}
	return pairs
}

// runsFromPairs turns matched line pairs into byte runs.
func runsFromPairs(pairs [][2]int, oldLines, newLines [][]byte) []matchedRun {
	oldStarts, newStarts := lineStarts(oldLines), lineStarts(newLines)
	var runs []matchedRun
	for _, pair := range pairs {
		run := matchedRun{oldStart: oldStarts[pair[0]], newStart: newStarts[pair[1]], length: len(oldLines[pair[0]])}
		if last := len(runs) - 1; last >= 0 &&
			runs[last].oldStart+runs[last].length == run.oldStart && runs[last].newStart+runs[last].length == run.newStart {
			runs[last].length += run.length
			continue
		}
		runs = append(runs, run)
	}
	return runs
}

func lineStarts(lines [][]byte) []int {
	starts := make([]int, len(lines))
	offset := 0
	for lineIndex, line := range lines {
		starts[lineIndex] = offset
		offset += len(line)
	}
	return starts
}

// toOld maps an offset in the new text to the old text. Offsets in changed
// lines have no match.
func (shift textShift) toOld(offset int) (int, bool) {
	for _, run := range shift.matches {
		if offset >= run.newStart && offset <= run.newStart+run.length {
			return run.oldStart + offset - run.newStart, true
		}
	}
	return 0, false
}

func (shift textShift) toNew(offset int) (int, bool) {
	for _, run := range shift.matches {
		if offset >= run.oldStart && offset <= run.oldStart+run.length {
			return run.newStart + offset - run.oldStart, true
		}
	}
	return 0, false
}
