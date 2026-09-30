package agent

import "strings"

// ghValueFlags are the gh flags whose value is the next argument, so it is
// not taken for the path or the target a call reads.
var ghValueFlags = map[string]bool{
	"-X": true, "--method": true, "-H": true, "--header": true, "-f": true, "--raw-field": true,
	"-F": true, "--field": true, "-q": true, "--jq": true, "-t": true, "--template": true,
	"--input": true, "-p": true, "--preview": true, "--hostname": true, "--cache": true,
	"-L": true, "--limit": true, "--json": true,
}

// ghSource is what a gh call with args reads, as a URL: gh api's
// resource, or the release, pull request, issue or repository a view
// names; "" for any other call.
func ghSource(args []string) string {
	repo, pos := ghArgs(args)
	if len(pos) < 2 {
		return ""
	}
	if pos[0] == "api" {
		path := strings.TrimPrefix(pos[1], "/")
		if path == "graphql" || strings.Contains(path, "{") {
			return ""
		}
		return "https://api.github.com/" + path
	}
	var target string
	if len(pos) > 2 {
		target = pos[2]
	}
	call := pos[0] + " " + pos[1]
	if call == "repo view" && ownerRepo(target) {
		return "https://github.com/" + target
	}
	if !ownerRepo(repo) {
		return ""
	}
	page := "https://github.com/" + repo
	switch {
	case call == "release view" && target == "":
		return page + "/releases/latest"
	case call == "release view":
		return page + "/releases/tag/" + target
	case call == "release list":
		return page + "/releases"
	case (call == "pr view" || call == "pr diff") && target != "":
		return page + "/pull/" + target
	case call == "issue view" && target != "":
		return page + "/issues/" + target
	}
	return ""
}

// ghArgs is the repository a gh call names with -R or --repo, and its
// positional arguments, flags and their values left out.
func ghArgs(args []string) (repo string, pos []string) {
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-R" || a == "--repo":
			if i+1 < len(args) {
				repo = args[i+1]
			}
			i++
		case strings.HasPrefix(a, "--repo="):
			repo = strings.TrimPrefix(a, "--repo=")
		case ghValueFlags[a]:
			i++
		case !strings.HasPrefix(a, "-"):
			pos = append(pos, a)
		}
	}
	return repo, pos
}

// ownerRepo reports whether s is an owner/repo pair.
func ownerRepo(s string) bool {
	owner, repo, ok := strings.Cut(s, "/")
	return ok && owner != "" && repo != "" && !strings.ContainsAny(repo, "/ ")
}
