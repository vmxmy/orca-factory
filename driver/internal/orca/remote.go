package orca

import (
	"errors"
	"net/url"
	"strings"
)

// ErrRemoteUnrecognized means a remote URL could not be reduced to a
// comparable host/owner/repo value; the raw input is never included, since
// it may carry embedded credentials.
var ErrRemoteUnrecognized = errors.New("git 远端 URL 无法规范化")

// NormalizeRemote reduces a git remote URL to the same host/owner/repo form
// as CanonicalKey (OF-56), so a live `git remote get-url origin` can be
// compared against Orca's cached identity. It handles https URLs, the
// git@host:owner/repo scp-style shorthand, ssh:// URLs (with or without a
// port), a trailing ".git", a trailing slash, and embedded credentials.
// Credentials are discarded before the host and path are read and never
// appear in the result or in the returned error.
func NormalizeRemote(remote string) (string, error) {
	host, path, ok := splitRemote(strings.TrimSpace(remote))
	if !ok {
		return "", ErrRemoteUnrecognized
	}
	host = strings.ToLower(host)
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	var segments []string
	for _, s := range strings.Split(path, "/") {
		if s != "" {
			segments = append(segments, s)
		}
	}
	if host == "" || len(segments) < 2 {
		return "", ErrRemoteUnrecognized
	}
	return host + "/" + strings.Join(segments, "/"), nil
}

// splitRemote separates a remote URL into its host and path, discarding any
// userinfo credentials. It recognizes URL-scheme forms (https://, ssh://,
// git://) and the git@host:path scp-like shorthand used by SSH remotes.
func splitRemote(remote string) (host, path string, ok bool) {
	if strings.Contains(remote, "://") {
		u, err := url.Parse(remote)
		if err != nil || u.Hostname() == "" {
			return "", "", false
		}
		return u.Hostname(), u.Path, true
	}
	rest := remote
	if at := strings.LastIndex(remote, "@"); at >= 0 {
		rest = remote[at+1:]
	}
	host, path, ok = strings.Cut(rest, ":")
	return host, path, ok && host != ""
}
