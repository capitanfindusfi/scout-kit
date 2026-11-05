package browser

import (
	"net/url"
	"strings"
	"sync"
)

var (
	resolversMu sync.RWMutex
	resolvers   = map[string]PageHook{}
)

// RegisterResolver sets the PageHook that SolveChallenge runs for pages of
// domain, and of its subdomains, unless the caller passes WithPageHook. It is
// meant to be called from init functions, typically those of the git-ignored
// browser/challenges package, which holds per-site resolvers. The most specific
// registered domain wins. Registering a domain again replaces its hook.
func RegisterResolver(domain string, hook PageHook) {
	domain = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(domain), "."))
	if domain == "" || hook == nil {
		return
	}
	resolversMu.Lock()
	defer resolversMu.Unlock()
	resolvers[domain] = hook
}

// resolverFor returns the hook registered for rawURL's host, walking up from
// the host to its parent domains, or nil.
func resolverFor(rawURL string) PageHook {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil
	}
	host := strings.ToLower(u.Hostname())

	resolversMu.RLock()
	defer resolversMu.RUnlock()
	for host != "" {
		if hook, ok := resolvers[host]; ok {
			return hook
		}
		_, rest, found := strings.Cut(host, ".")
		if !found {
			return nil
		}
		host = rest
	}
	return nil
}
