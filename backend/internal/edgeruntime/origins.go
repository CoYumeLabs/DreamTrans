package edgeruntime

import (
	"net"
	"net/url"
	"slices"
	"strings"

	"golang.org/x/net/publicsuffix"
)

// browserOrigins also admits the www/apex counterpart of a configured HTTPS
// site. Expand at startup so existing node configurations gain the same behavior
// after an upgrade. Grants still bind the exact requesting Origin.
func browserOrigins(configured []string) []string {
	origins := slices.Clone(configured)
	for _, origin := range configured {
		if alias := wwwOrigin(origin); alias != "" && !slices.Contains(origins, alias) {
			origins = append(origins, alias)
		}
	}
	return origins
}

func wwwOrigin(origin string) string {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil ||
		u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return ""
	}
	host := u.Hostname()
	base := strings.TrimPrefix(host, "www.")
	if net.ParseIP(base) != nil {
		return ""
	}
	// Do not turn a configured application subdomain into permission for its
	// parent, siblings, or a public suffix (including shared hosting suffixes).
	registrable, err := publicsuffix.EffectiveTLDPlusOne(base)
	if err != nil || registrable != base {
		return ""
	}
	alias := "www." + base
	if host != base {
		alias = base
	}
	if port := u.Port(); port != "" {
		alias = net.JoinHostPort(alias, port)
	}
	return "https://" + alias
}
