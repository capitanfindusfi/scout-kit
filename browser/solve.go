package browser

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/go-rod/rod"

	"github.com/capitanfindusfi/scout-kit/fetch"
)

const (
	// solvePollInterval is how often the page is checked for the challenge.
	solvePollInterval = time.Second
	// solveGrace is how long a page that shows no challenge is watched before it
	// is taken as clean: DataDome often swaps in the challenge a moment after the
	// load.
	solveGrace = 5 * time.Second
	// DefaultSolveTimeout is how long SolveChallenge waits for the challenge to
	// be solved if the context has no deadline of its own.
	DefaultSolveTimeout = 5 * time.Minute
)

// ErrChallengeUnsolved means the challenge was still on the page when the time
// ran out.
var ErrChallengeUnsolved = errors.New("challenge not solved in time")

// SolveChallenge opens rawURL in the shared Chrome, which has a window, and
// waits for the anti-bot challenge (DataDome's captcha, or any other page
// fetch.Blocked recognises) to be solved there by whoever is looking at it; it
// does not solve it by itself. Once the site has redirected to the real page it
// returns the site's cookies, the new DataDome ones included.
//
// The cookies are already in the browser, so they are in the profile too: a
// remote browser's are saved to the cookie snapshot right away, a local
// Chrome writes them to its profile as it does for any cookie, at the latest
// when Close shuts it down.
//
// A page with no challenge returns after a few seconds with the cookies it has.
// Without a deadline in ctx, DefaultSolveTimeout applies. Options can run code
// of yours on the page while it waits, see WithPageHook; without one, the
// resolver registered for the site's domain is used, see RegisterResolver.
func (b *Browser) SolveChallenge(ctx context.Context, rawURL string, opts ...SolveOption) ([]fetch.Cookie, error) {
	var cfg solveConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.hook == nil {
		cfg.hook = resolverFor(rawURL)
	}

	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, DefaultSolveTimeout)
		defer cancel()
	}

	page, err := b.NewPage(true, nil, "")
	if err != nil {
		return nil, err
	}
	defer func() { _ = page.Close() }()

	if err := page.Context(ctx).Navigate(rawURL); err != nil {
		return nil, fmt.Errorf("failed to open %s: %w", rawURL, err)
	}

	stopHook := cfg.startHook(ctx, page)
	err = waitChallengeGone(ctx, page)
	stopHook()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", rawURL, err)
	}
	// Best effort: let the real page settle so that the cookies it sets after
	// the redirect are included.
	if err := page.Context(ctx).WaitIdle(fallbackIdleTimeout); err != nil {
		log.Printf("page %s did not go idle: %v", rawURL, err)
	}

	cookies := siteCookies(b, rawURL)
	if err := b.persistCookies(); err != nil {
		// The cookies are still in the running browser and in the result
		log.Printf("failed to save the cookies of the solved challenge: %v", err)
	}
	return cookies, nil
}

// PageHook is code run on the page while SolveChallenge waits, typically to
// drive it with page.Mouse and page.Keyboard, or to dig into the challenge's
// iframe. It starts right after the navigation is issued, so the page may still
// be loading: wait for what it needs (page.Element waits for the element). The
// page is bound to a context that is cancelled when the challenge is gone, the
// time runs out or the hook returns, so its calls stop by themselves; honour
// ctx in anything long. A hook that returns early is fine: the wait goes on, in
// case somebody solves the challenge by hand.
type PageHook func(ctx context.Context, page *rod.Page) error

// SolveOption tunes SolveChallenge.
type SolveOption func(*solveConfig)

type solveConfig struct {
	hook PageHook
}

// WithPageHook runs hook on the page during the wait, see PageHook.
func WithPageHook(hook PageHook) SolveOption {
	return func(c *solveConfig) { c.hook = hook }
}

// startHook runs the hook, if any, in the background. The returned function
// cancels it and waits for it to finish, so that the page is not closed under
// it.
func (c *solveConfig) startHook(ctx context.Context, page *rod.Page) (stop func()) {
	if c.hook == nil {
		return func() {}
	}

	hookCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				log.Printf("page hook panicked: %v", r)
			}
		}()
		if err := c.hook(hookCtx, page.Context(hookCtx)); err != nil && hookCtx.Err() == nil {
			log.Printf("page hook failed: %v", err)
		}
	}()
	return func() {
		cancel()
		<-done
	}
}

// waitChallengeGone polls the page until it shows no challenge. A page that
// showed one must have lost it (the redirect); one that never did is accepted
// after solveGrace.
func waitChallengeGone(ctx context.Context, page *rod.Page) error {
	start := time.Now()
	seen := false
	ticker := time.NewTicker(solvePollInterval)
	defer ticker.Stop()

	for {
		// An error is expected while the page navigates: look again next tick.
		if html, err := page.HTML(); err == nil {
			if pageBlocked(html) {
				if !seen {
					log.Printf("challenge on the page, waiting for it to be solved")
				}
				seen = true
			} else if seen || time.Since(start) >= solveGrace {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			if seen {
				return ErrChallengeUnsolved
			}
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func pageBlocked(html string) bool {
	return fetch.Blocked(&fetch.Response{Status: 200, Body: []byte(html)})
}

// persistCookies makes sure the browser's current cookies survive it. A local
// Chrome flushes its own to the profile; only a remote one, which is killed
// before it does, needs the snapshot.
func (b *Browser) persistCookies() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.rod == nil || b.remoteURL == "" {
		return nil
	}
	return b.saveCookieSnapshot()
}
