package browser

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"

	"github.com/capitanfindusfi/scout-kit/fetch"
)

const (
	fallbackNavigationTimeout = 45 * time.Second
	fallbackIdleTimeout       = 5 * time.Second
)

// Fallback returns a fetch.Fallback that loads the URL in this browser, with
// the stealth evasions, and hands back the browser's cookies for the URL's
// site so that the HTTP client can continue with the same session.
func (b *Browser) Fallback() fetch.Fallback {
	return func(ctx context.Context, rawURL string) (*fetch.Response, []fetch.Cookie, error) {
		page, err := b.NewPage(true, nil, "")
		if err != nil {
			return nil, nil, err
		}
		defer func() { _ = page.Close() }()

		// Wait on Chrome's own load event: rod's WaitLoad runs a script in the
		// page, which fails on pages that are still loading.
		p := page.Context(ctx).Timeout(fallbackNavigationTimeout)
		loaded := p.WaitNavigation(proto.PageLifecycleEventNameLoad)
		if err := p.Navigate(rawURL); err != nil {
			return nil, nil, fmt.Errorf("failed to open %s: %w", rawURL, err)
		}
		loaded()
		if err := p.GetContext().Err(); err != nil {
			return nil, nil, fmt.Errorf("failed to load %s: %w", rawURL, err)
		}
		// Best effort: a busy page must not fail the fetch.
		if err := page.Context(ctx).Timeout(fallbackIdleTimeout).WaitIdle(fallbackIdleTimeout); err != nil {
			log.Printf("page %s did not go idle: %v", rawURL, err)
		}

		html, err := page.HTML()
		if err != nil {
			return nil, nil, err
		}
		res := &fetch.Response{Status: navigationStatus(page), URL: rawURL, Body: []byte(html), Via: "browser"}
		if info, err := page.Info(); err == nil {
			res.URL = info.URL
		}
		if res.Status == 0 {
			// Unknown: the page loaded, which is the best the browser can say.
			res.Status = 200
		}

		return res, siteCookies(b, rawURL), nil
	}
}

func siteCookies(b *Browser, rawURL string) []fetch.Cookie {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil
	}
	b.mu.Lock()
	rodBrowser := b.rod
	b.mu.Unlock()
	if rodBrowser == nil {
		return nil
	}
	cookies, err := rodBrowser.GetCookies()
	if err != nil {
		log.Printf("failed to read the browser's cookies: %v", err)
		return nil
	}

	var out []fetch.Cookie
	for _, c := range cookies {
		if !cookieAppliesTo(c.Domain, u.Hostname()) {
			continue
		}
		out = append(out, fetch.Cookie{Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path, Secure: c.Secure})
	}
	return out
}

// cookieAppliesTo tells whether a cookie of the given domain ("www.x.it" or
// ".x.it") is sent to host.
func cookieAppliesTo(domain, host string) bool {
	bare := strings.TrimPrefix(domain, ".")
	if domain == host {
		return true
	}
	return strings.HasPrefix(domain, ".") && (host == bare || strings.HasSuffix(host, domain))
}

// navigationStatus reads the HTTP status of the page load from the browser's
// navigation timing (Chrome 109+); 0 when unavailable, which means "unknown".
func navigationStatus(p *rod.Page) int {
	res, err := p.Eval(`() => {
		const nav = performance.getEntriesByType("navigation")[0];
		return nav && nav.responseStatus ? nav.responseStatus : 0;
	}`)
	if err != nil {
		return 0
	}
	return res.Value.Int()
}
