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

// parseDiff walks a unified diff once. Paths are head-side for added and
// shown, base-side for removed; a rename therefore keys the two sides
// differently, which is what the two trees need.
func parseDiff(diff string) diffLines {
	d := diffLines{added: map[string][]int{}, shown: map[string]map[int]bool{}, removed: map[string][]int{}}
	var oldPath, newPath string
	var oldLine, newLine int
	inHunk := false
	for l := range strings.SplitSeq(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "--- "):
			oldPath = stripPrefix(l[4:], "a/")
			inHunk = false
		case strings.HasPrefix(l, "+++ "):
			newPath = stripPrefix(l[4:], "b/")
			inHunk = false
		case strings.HasPrefix(l, "@@"):
			oldLine, newLine = hunkStarts(l)
			inHunk = oldLine > 0 && newLine > 0
		case !inHunk:
		case strings.HasPrefix(l, "+"):
			if newPath != "" {
				d.added[newPath] = append(d.added[newPath], newLine)
				d.show(newPath, newLine)
			}
			newLine++
		case strings.HasPrefix(l, "-"):
			if oldPath != "" {
				d.removed[oldPath] = append(d.removed[oldPath], oldLine)
			}
			oldLine++
		case strings.HasPrefix(l, " "):
			if newPath != "" {
				d.show(newPath, newLine)
			}
			oldLine++
			newLine++
		case strings.HasPrefix(l, "\\"):
		default:
			inHunk = false
		}
	}
	return d
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

// hunkStarts reads "@@ -a,b +c,d @@" and returns a and c.
func hunkStarts(header string) (oldStart, newStart int) {
	fields := strings.Fields(header)
	if len(fields) < 3 {
		return 0, 0
	}
	return startOf(fields[1]), startOf(fields[2])
}

func startOf(field string) int {
	field = strings.TrimLeft(field, "-+")
	if i := strings.IndexByte(field, ','); i >= 0 {
		field = field[:i]
	}
	n, err := strconv.Atoi(field)
	if err != nil {
		return 0
	}
	return n
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
// the region as it now is. Hunks are keyed by their head path.
func Hunks(diff string) []Hunk {
	var out []Hunk
	var path string
	var cur *Hunk
	flush := func() {
		if cur != nil && strings.TrimSpace(cur.Text) != "" {
			out = append(out, *cur)
		}
		cur = nil
	}
	for l := range strings.SplitSeq(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "+++ "):
			flush()
			path = stripPrefix(l[4:], "b/")
		case strings.HasPrefix(l, "--- "), strings.HasPrefix(l, "diff --git "):
			flush()
		case strings.HasPrefix(l, "@@"):
			flush()
			if path != "" {
				cur = &Hunk{Path: path}
			}
		case cur == nil:
		case strings.HasPrefix(l, "+"), strings.HasPrefix(l, " "):
			cur.Text += l[1:] + "\n"
		}
	}
	flush()
	return out
}

// Hunk is one hunk's head-side text.
type Hunk struct {
	Path string
	Text string
}
