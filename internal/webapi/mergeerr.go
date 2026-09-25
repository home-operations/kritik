package webapi

import (
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/home-operations/kritik/internal/configfile"
)

var (
	// otherTenantRe matches the ways a configfile error names a tenant, so
	// a message about the one being written never names another.
	otherTenantRe = regexp.MustCompile(`dashboard\[([^\]]*)\]|tenants\[\d+\]|tenant "([^"]*)"`)
	quotedRe      = regexp.MustCompile(`"([^"]*)"`)
	installNameRe = regexp.MustCompile(`^installations\[\d+\]\.name$`)
	yamlLineRe    = regexp.MustCompile(`^line \d+: `)
	yamlFieldRe   = regexp.MustCompile(`field (\S+) not found`)
	yamlTypeRe    = regexp.MustCompile(` in type [\w.*\[\]]+`)
)

// validateWithout merges the running dashboard tenants minus slug: whether
// the configuration is valid before the write being judged.
func validateWithout(current *configfile.File, slug string, open configfile.Opener) error {
	dash := slices.DeleteFunc(current.Dashboard(), func(d configfile.DashboardTenant) bool { return d.Slug == slug })
	_, err := configfile.Merge(current, dash, open)
	return err
}

// mergeFailure turns a failed configfile.ValidateDashboard of the tenant
// slug, decoded as candidate, into the API's answer. Merge reports a clash
// against whichever tenant sorts later, so an error naming another tenant
// is still the candidate's fault when the configuration validates without
// it (baseline): both are a 422. Only a configuration already invalid
// without the write is a 409, as no write can be judged until an operator
// fixes it.
func mergeFailure(slug string, candidate *configfile.Tenant, err error, baseline func() error) error {
	me, ok := errors.AsType[*configfile.MergeError](err)
	if ok && me.Slug == slug {
		path, msg := splitPath(trimConfigfile(me.Err.Error()), slug)
		return errStatus(http.StatusUnprocessableEntity, CodeInvalidSpec, scrubTenants(msg, slug), pathDetails{Path: path})
	}
	if baseline() != nil {
		return errStatus(http.StatusConflict, CodeConfigBlocked,
			"the configuration is invalid elsewhere, so this change cannot be checked; an operator must fix it first", nil)
	}
	var other, rest string
	if ok {
		other = me.Slug
		rest = trimConfigfile(me.Err.Error())
	} else {
		rest = trimConfigfile(err.Error())
	}
	theirPath, detail := splitPath(rest, other)
	path := candidatePath(candidate, theirPath, detail)
	msg := "conflicts with another tenant: " + scrubTenants(strings.TrimLeft(strings.TrimPrefix(detail, theirPath), ": "), slug)
	if path != "" {
		msg = path + ": " + msg
	}
	return errStatus(http.StatusUnprocessableEntity, CodeInvalidSpec, msg, pathDetails{Path: path})
}

func trimConfigfile(msg string) string {
	for strings.HasPrefix(msg, "configfile: ") {
		msg = strings.TrimPrefix(msg, "configfile: ")
	}
	return msg
}

// splitPath strips slug's "dashboard[<slug>]." prefix from msg and returns
// the path it names, up to the first colon or space, and the message.
func splitPath(msg, slug string) (path, rest string) {
	prefix := "dashboard[" + slug + "]"
	after, ok := strings.CutPrefix(msg, prefix+".")
	if !ok {
		return "", strings.TrimSpace(strings.TrimPrefix(msg, prefix))
	}
	path = after
	if i := strings.IndexAny(path, ": "); i >= 0 {
		path = path[:i]
	}
	return path, after
}

// candidatePath maps the path of another tenant's clash onto the
// candidate's spec: the slug, or an installation by the name it shares.
func candidatePath(candidate *configfile.Tenant, theirPath, detail string) string {
	switch {
	case theirPath == slugPath.Path:
		return slugPath.Path
	case installNameRe.MatchString(theirPath):
		m := quotedRe.FindStringSubmatch(detail)
		if m == nil {
			return ""
		}
		for i := range candidate.Installations {
			if candidate.Installations[i].Name == m[1] {
				return "installations[" + strconv.Itoa(i) + "].name"
			}
		}
	}
	return ""
}

// scrubTenants rewords every mention of a tenant other than slug.
func scrubTenants(msg, slug string) string {
	return otherTenantRe.ReplaceAllStringFunc(msg, func(m string) string {
		sub := otherTenantRe.FindStringSubmatch(m)
		if (sub[1] != "" && sub[1] == slug) || (sub[2] != "" && sub[2] == slug) {
			return m
		}
		return "another tenant"
	})
}

// decodeFailure is a spec that does not decode as a tenant: the decoder's
// message without the line numbers of a document the client never wrote,
// and the offending field's name as the path when it has one.
func decodeFailure(err error) error {
	msg := trimConfigfile(err.Error())
	msg = strings.Replace(msg, "yaml: unmarshal errors:", "", 1)
	var parts []string
	for line := range strings.SplitSeq(msg, "\n") {
		line = strings.TrimSpace(line)
		line = yamlLineRe.ReplaceAllString(line, "")
		if line != "" {
			parts = append(parts, yamlTypeRe.ReplaceAllString(line, ""))
		}
	}
	msg = strings.ReplaceAll(strings.Join(parts, "; "), ":; ", ": ")
	var path string
	switch m := yamlFieldRe.FindStringSubmatch(msg); {
	case m != nil:
		path = m[1]
	case strings.Contains(msg, "tenant spec slug"):
		path = slugPath.Path
	}
	return errStatus(http.StatusUnprocessableEntity, CodeInvalidSpec, msg, pathDetails{Path: path})
}
