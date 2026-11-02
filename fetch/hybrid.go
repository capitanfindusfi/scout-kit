package fetch

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strings"
	"sync"
	"time"
)

// ErrBlocked means the site refused the request and there was no fallback (or
// it failed too).
var ErrBlocked = errors.New("request blocked")

// challengeMarkers appear in the anti-bot pages that replace the real one.
var challengeMarkers = []string{
	"captcha-delivery.com",        // DataDome
	"cf-chl-", "Just a moment...", // Cloudflare
	"px-captcha",          // PerimeterX
	"_Incapsula_Resource", // Imperva
}

// maxChallengeBody is the size under which a 2xx page is checked for challenge
// markers: real pages can mention them in a script, a challenge page is small.
const maxChallengeBody = 16 * 1024

// Blocked reports whether the site answered with an anti-bot refusal or
// challenge instead of the page.
func Blocked(r *Response) bool {
	switch {
	case r.Status == 403 || r.Status == 429:
		return true
	case r.Status >= 500:
		return hasMarker(r.Body)
	case len(r.Body) < maxChallengeBody:
		return hasMarker(r.Body)
	}
	return false
}

func hasMarker(body []byte) bool {
	text := string(body)
	for _, m := range challengeMarkers {
		if strings.Contains(text, m) {
			return true
		}
	}
	return false
}

// Fallback fetches a URL some other way, normally in a real browser. It also
// returns the browser's cookies for the site, which Hybrid hands to the HTTP
// client so that it can try again with the browser's session.
type Fallback func(ctx context.Context, rawURL string) (*Response, []Cookie, error)

// Hybrid tries plain HTTP first and the Fallback when the site blocks it. After
// a block the host goes straight to the fallback for Cooldown, rather than
// hammering a site that has already flagged the client, then HTTP is tried
// again (with the cookies the browser collected meanwhile).
type Hybrid struct {
	HTTP     *Client
	Fallback Fallback
	// Cooldown is how long a host stays on the fallback after a block; the
	// default is 10 minutes.
	Cooldown time.Duration

	mu    sync.Mutex
	until map[string]time.Time
	now   func() time.Time
}

const defaultCooldown = 10 * time.Minute

func NewHybrid(client *Client, fallback Fallback) *Hybrid {
	return &Hybrid{HTTP: client, Fallback: fallback, until: map[string]time.Time{}, now: time.Now}
}

func (h *Hybrid) clock() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

func hostOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func (h *Hybrid) coolingDown(host string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.clock().Before(h.until[host])
}

func (h *Hybrid) startCooldown(host string) {
	cooldown := h.Cooldown
	if cooldown == 0 {
		cooldown = defaultCooldown
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.until == nil {
		h.until = map[string]time.Time{}
	}
	h.until[host] = h.clock().Add(cooldown)
}

// Get fetches rawURL, over HTTP if the site lets it, otherwise through the
// fallback.
func (h *Hybrid) Get(ctx context.Context, rawURL string, extra map[string]string) (*Response, error) {
	host := hostOf(rawURL)
	if h.Fallback != nil && h.coolingDown(host) {
		return h.viaFallback(ctx, rawURL)
	}

	res, err := h.HTTP.Get(ctx, rawURL, extra)
	switch {
	case err != nil && ctx.Err() != nil:
		return nil, err
	case err == nil && !Blocked(res):
		return res, nil
	case err != nil:
		log.Printf("http fetch failed, using the fallback: %v", err)
	default:
		log.Printf("%s blocked over http (status %d), using the fallback", rawURL, res.Status)
	}

	if h.Fallback == nil {
		if err != nil {
			return nil, err
		}
		return res, fmt.Errorf("%s (status %d): %w", rawURL, res.Status, ErrBlocked)
	}
	h.startCooldown(host)
	return h.viaFallback(ctx, rawURL)
}

func (h *Hybrid) viaFallback(ctx context.Context, rawURL string) (*Response, error) {
	res, cookies, err := h.Fallback(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	res.Via = "browser"
	h.HTTP.SetCookies(cookies)
	if Blocked(res) {
		return res, fmt.Errorf("%s (status %d, browser): %w", rawURL, res.Status, ErrBlocked)
	}
	return res, nil
}
