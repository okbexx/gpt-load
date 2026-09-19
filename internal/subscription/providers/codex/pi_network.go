package codex

import (
	"net/url"
	"strconv"
	"strings"
)

// The bridge receives a frozen per-attempt choice, never a request to consult
// the sidecar's process environment. Keep this grammar aligned with Node's
// validateProxyURL; unsupported proxy families fail before credentials dispatch.
func validPiProxy(value string) bool {
	if value == "" || value == "direct" {
		return true
	}
	if len(value) > 4096 || (!strings.HasPrefix(value, "http://") && !strings.HasPrefix(value, "https://")) || strings.ContainsAny(value, "\\?#") {
		return false
	}
	for _, c := range value {
		if c <= 32 || c == 127 {
			return false
		}
	}
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" || u.Opaque != "" || u.Port() == "0" {
		return false
	}
	if port := u.Port(); port != "" {
		number, err := strconv.Atoi(port)
		if err != nil || number < 1 || number > 65535 {
			return false
		}
	}
	if u.User != nil {
		password, _ := u.User.Password()
		for _, credential := range []string{u.User.Username(), password} {
			if len(credential) > 1024 {
				return false
			}
			for _, c := range credential {
				if c < 32 || c == 127 {
					return false
				}
			}
		}
	}
	return true
}
