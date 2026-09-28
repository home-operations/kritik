package contextpack

import (
	"strconv"
	"strings"
)

// diffLines is what a unified diff says per path: the head-side lines it
// added, the head-side lines it shows at all (added plus context), and the
// base-side lines it removed.
type diffLines struct {
	added   map[string][]int
	shown   map[string]map[int]bool
	removed map[string][]int
}

// diffLine is one line of a hunk: its kind ('+', '-' or ' '), its text
// without the marker, the paths of the file's two sides ("" for a side
// that does not exist) and the line's number on each side.
type diffLine struct {
	kind             byte
	text             string
	oldPath, newPath string
	oldLine, newLine int
	// hunk numbers the diff's hunks from 0.
	hunk int
}

// walkDiff calls fn for each line of each hunk of a unified diff. A hunk's
// lines are counted from its header, so a removed "-- x" or an added
// "++ x", which read like file headers, stay lines of the hunk.
func walkDiff(diff string, fn func(diffLine)) {
	cur := diffLine{hunk: -1}
	var oldLeft, newLeft int
	for l := range strings.SplitSeq(diff, "\n") {
		if oldLeft > 0 || newLeft > 0 {
			if l == "" {
				// A context line whose leading space was stripped.
				l = " "
			}
			cur.kind, cur.text = l[0], l[1:]
			switch cur.kind {
			case '+':
				fn(cur)
				cur.newLine++
				newLeft--
				continue
			case '-':
				fn(cur)
				cur.oldLine++
				oldLeft--
				continue
			case ' ':
				fn(cur)
				cur.oldLine++
				cur.newLine++
				oldLeft--
				newLeft--
				continue
			case '\\':
				// "\ No newline at end of file"
				continue
			}
			// Not a hunk line: the hunk ended short of its counts, and the
			// line is read as a header.
			oldLeft, newLeft = 0, 0
		}
		switch {
		case strings.HasPrefix(l, "diff --git "):
			cur.oldPath, cur.newPath = "", ""
		case strings.HasPrefix(l, "--- "):
			cur.oldPath = stripPrefix(l[4:], "a/")
		case strings.HasPrefix(l, "+++ "):
			cur.newPath = stripPrefix(l[4:], "b/")
		case strings.HasPrefix(l, "@@"):
			if h, ok := parseHunkHeader(l); ok {
				cur.oldLine, oldLeft, cur.newLine, newLeft = h.oldStart, h.oldCount, h.newStart, h.newCount
				cur.hunk++
			}
		}
	}
}

// parseDiff walks a unified diff once. Paths are head-side for added and
// shown, base-side for removed; a rename therefore keys the two sides
// differently, which is what the two trees need.
func parseDiff(diff string) diffLines {
	d := diffLines{added: map[string][]int{}, shown: map[string]map[int]bool{}, removed: map[string][]int{}}
	walkDiff(diff, func(l diffLine) {
		switch {
		case l.kind == '-' && l.oldPath != "":
			d.removed[l.oldPath] = append(d.removed[l.oldPath], l.oldLine)
		case l.kind == '+' && l.newPath != "":
			d.added[l.newPath] = append(d.added[l.newPath], l.newLine)
			d.show(l.newPath, l.newLine)
		case l.kind == ' ' && l.newPath != "":
			d.show(l.newPath, l.newLine)
		}
	})
	return d
}

// ShownLines returns, per head-side path, the head-side lines a unified
// diff shows: its added and context lines.
func ShownLines(diff string) map[string]map[int]bool {
	return parseDiff(diff).shown
}

func (d *diffLines) show(path string, line int) {
	if d.shown[path] == nil {
		d.shown[path] = map[int]bool{}
	}
	d.shown[path][line] = true
}

func stripPrefix(p, prefix string) string {
	p = strings.TrimSpace(p)
	if p == "/dev/null" {
		return ""
	}
	return strings.TrimPrefix(p, prefix)
}

// hunkHeader is a hunk header's "@@ -oldStart,oldCount +newStart,newCount @@".
type hunkHeader struct {
	oldStart, oldCount, newStart, newCount int
}

// parseHunkHeader reads a hunk header; a count left out is 1.
func parseHunkHeader(l string) (hunkHeader, bool) {
	fields := strings.Fields(l)
	if len(fields) < 3 || !strings.HasPrefix(fields[1], "-") || !strings.HasPrefix(fields[2], "+") {
		return hunkHeader{}, false
	}
	oldStart, oldCount, ok1 := hunkRange(fields[1][1:])
	newStart, newCount, ok2 := hunkRange(fields[2][1:])
	return hunkHeader{oldStart, oldCount, newStart, newCount}, ok1 && ok2
}

func hunkRange(s string) (start, count int, ok bool) {
	first, rest, hasCount := strings.Cut(s, ",")
	start, err := strconv.Atoi(first)
	if err != nil {
		return 0, 0, false
	}
	count = 1
	if hasCount {
		if count, err = strconv.Atoi(rest); err != nil {
			return 0, 0, false
		}
	}
	return start, count, true
}

// runs groups sorted line numbers into ranges, merging neighbours closer
// than gap lines.
func runs(lines []int, gap int) [][2]int {
	var out [][2]int
	for _, l := range lines {
		if n := len(out); n > 0 && l-out[n-1][1] <= gap {
			out[n-1][1] = l
			continue
		}
		out = append(out, [2]int{l, l})
	}
	return out
}

// Hunks returns the head-side text of each hunk in a unified diff: added
// and context lines, without the removed ones, so each string reads like
// the region as it now is. Hunks are keyed by their head path; a deleted
// file's have none and are left out.
func Hunks(diff string) []Hunk {
	var out []Hunk
	last := -1
	var cur Hunk
	var text strings.Builder
	flush := func() {
		if cur.Path != "" && strings.TrimSpace(text.String()) != "" {
			cur.Text = text.String()
			out = append(out, cur)
		}
		text.Reset()
	}
	walkDiff(diff, func(l diffLine) {
		if l.hunk != last {
			flush()
			last, cur = l.hunk, Hunk{Path: l.newPath}
		}
		if l.kind != '-' {
			text.WriteString(l.text)
			text.WriteByte('\n')
		}
	})
	flush()
	return out
}

// Hunk is one hunk's head-side text.
type Hunk struct {
	Path string
	Text string
}
