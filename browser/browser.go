package browser

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/launcher/flags"
	"github.com/go-rod/rod/lib/proto"
	stealthlib "github.com/go-rod/stealth"
)

// Browser is a Chrome instance shared by the browser-based scrapers. If Chrome
// dies, the next page it opens relaunches it. Chrome runs either locally or in
// a remote browser service (browserless), see NewRemote.
type Browser struct {
	mu       sync.Mutex
	launcher *launcher.Launcher
	rod      *rod.Browser
	// profileDir is the persistent Chrome profile, kept across restarts. Empty
	// means a temporary profile, removed when Chrome is shut down. With a
	// remote browser it is a path inside the browser service.
	profileDir string
	// remoteURL is the browser service's CDP endpoint, launch options included;
	// empty for a local Chrome.
	remoteURL string
	// stateDir is where the fingerprints of the applied cookie seeds are kept.
	// Empty means they are not kept, so a changed seed never replaces the
	// browser's cookies.
	stateDir string
}

// chromeFlag is a command line switch Chrome is started with, locally or by
// the remote browser service.
type chromeFlag struct {
	name  string
	value string
}

var chromeFlags = []chromeFlag{
	{"disable-blink-features", "AutomationControlled"},
	{"lang", "it-IT"},
	{"accept-lang", "it-IT,it"},
}

// New launches a local Chrome, using the one installed on the system or
// downloading it on first run if none is found. With a profileDir, Chrome keeps
// its profile there, cookies included, so they survive a restart; without one
// it uses a temporary profile.
func New(profileDir string) (*Browser, error) {
	b := &Browser{profileDir: profileDir, stateDir: profileDir}
	if err := b.launch(); err != nil {
		return nil, err
	}
	return b, nil
}

// NewRemote connects to a browserless service at wsURL (e.g.
// ws://chromium:3000), which starts a Chrome for this connection with the same
// flags as a local one. profileDir is the persistent profile inside the
// service; stateDir is a local directory for the cookie seed fingerprints.
func NewRemote(wsURL, profileDir, stateDir string) (*Browser, error) {
	remoteURL, err := remoteLaunchURL(wsURL, profileDir)
	if err != nil {
		return nil, err
	}

	if stateDir != "" {
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			return nil, fmt.Errorf("failed to create browser state dir %s: %w", stateDir, err)
		}
	}

	b := &Browser{profileDir: profileDir, remoteURL: remoteURL, stateDir: stateDir}
	if err := b.launch(); err != nil {
		return nil, err
	}
	return b, nil
}

// remoteLaunchURL adds browserless's launch options to wsURL: a window (see
// launch), chromeFlags and, if given, the profile. browserless recommends
// base64 for the options' JSON.
func remoteLaunchURL(wsURL, profileDir string) (string, error) {
	u, err := url.Parse(wsURL)
	if err != nil {
		return "", fmt.Errorf("invalid browser URL %q: %w", wsURL, err)
	}
	if u.Scheme != "ws" && u.Scheme != "wss" {
		return "", fmt.Errorf("invalid browser URL %q: want ws:// or wss://", wsURL)
	}

	var args []string
	for _, f := range chromeFlags {
		args = append(args, "--"+f.name+"="+f.value)
	}
	if profileDir != "" {
		args = append(args, "--user-data-dir="+profileDir)
	}

	options, err := json.Marshal(map[string]any{"headless": false, "args": args})
	if err != nil {
		return "", err
	}

	query := u.Query()
	query.Set("launch", base64.StdEncoding.EncodeToString(options))
	u.RawQuery = query.Encode()
	return u.String(), nil
}

// launch starts Chrome with a window: anti-bot checks (DataDome on
// immobiliare.it) block headless Chrome however it is disguised. On a server
// without a screen the window goes to a virtual display (Xvfb, see Dockerfile).
func (b *Browser) launch() error {
	if b.remoteURL != "" {
		return b.connectRemote()
	}

	l := launcher.New().Headless(false)
	for _, f := range chromeFlags {
		l = l.Set(flags.Flag(f.name), f.value)
	}

	// Prefer the installed Chrome, which is kept current, over the old
	// Chromium build rod downloads by default.
	if bin, found := launcher.LookPath(); found {
		l = l.Bin(bin)
	}

	if b.profileDir != "" {
		if err := os.MkdirAll(b.profileDir, 0o700); err != nil {
			return fmt.Errorf("failed to create browser profile %s: %w", b.profileDir, err)
		}
		clearProfileLocks(b.profileDir)
		l = l.UserDataDir(b.profileDir)
	}

	controlURL, err := l.Launch()
	if err != nil {
		return fmt.Errorf("failed to launch browser: %w", err)
	}

	// No device emulation: its user agent claims an older Chrome than the one
	// running, and that mismatch alone triggers the captcha.
	rodBrowser := rod.New().ControlURL(controlURL).NoDefaultDevice()
	if err := rodBrowser.Connect(); err != nil {
		l.Kill()
		b.cleanup(l)
		return fmt.Errorf("failed to connect to browser: %w", err)
	}

	b.launcher = l
	b.rod = rodBrowser
	log.Printf("browser launched")
	return nil
}

// connectRemote has the browser service start a Chrome for this connection.
// The service owns the process and the profile, so there is no launcher, no
// lock clearing and no cleanup here.
func (b *Browser) connectRemote() error {
	rodBrowser := rod.New().ControlURL(b.remoteURL).NoDefaultDevice()
	if err := rodBrowser.Connect(); err != nil {
		return fmt.Errorf("failed to connect to remote browser: %w", err)
	}

	b.rod = rodBrowser
	if err := b.restoreCookieSnapshot(); err != nil {
		// The profile still has the cookies Chrome wrote to it before
		log.Printf("failed to restore the browser's cookies: %v", err)
	}
	log.Printf("remote browser connected")
	return nil
}

// cookieSnapshotFile, in the state dir, carries a remote Chrome's cookies over
// to the next connection. browserless kills Chrome as soon as the connection
// closes, before Chrome writes its latest cookies to the profile (it does so
// about every 30 seconds), and losing a cookie the site just refreshed brings
// back the captcha.
const cookieSnapshotFile = "cookies.json"

// saveCookieSnapshot writes the browser's cookies to the state dir, if any.
func (b *Browser) saveCookieSnapshot() error {
	if b.stateDir == "" {
		return nil
	}
	cookies, err := b.rod.GetCookies()
	if err != nil {
		return fmt.Errorf("failed to read browser cookies: %w", err)
	}
	data, err := json.Marshal(cookies)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(b.stateDir, cookieSnapshotFile), data, 0o600)
}

// restoreCookieSnapshot sets the cookies saved when the last connection
// closed, then removes the snapshot: if a connection ends without saving one
// (a crash), the profile's own copy, which is newer, is used instead.
func (b *Browser) restoreCookieSnapshot() error {
	if b.stateDir == "" {
		return nil
	}
	path := filepath.Join(b.stateDir, cookieSnapshotFile)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}

	var cookies []*proto.NetworkCookie
	if err := json.Unmarshal(data, &cookies); err != nil {
		return fmt.Errorf("invalid cookie snapshot %s: %w", path, err)
	}
	if len(cookies) > 0 {
		if err := b.rod.SetCookies(proto.CookiesToParams(cookies)); err != nil {
			return fmt.Errorf("failed to set browser cookies: %w", err)
		}
	}
	log.Printf("restored %d browser cookies", len(cookies))
	return os.Remove(path)
}

// gracefulExitTimeout is how long Chrome gets to exit by itself, writing its
// cookies to the profile, before it is killed.
const gracefulExitTimeout = 10 * time.Second

func (b *Browser) shutdown() {
	if b.rod != nil {
		if b.remoteURL != "" {
			if err := b.saveCookieSnapshot(); err != nil {
				log.Printf("failed to save the browser's cookies: %v", err)
			}
		}
		_ = b.rod.Close()
		b.rod = nil
		if b.launcher != nil && b.profileDir != "" {
			waitForExit(b.launcher.PID(), gracefulExitTimeout)
		}
	}
	if b.launcher != nil {
		b.launcher.Kill()
		b.cleanup(b.launcher)
		b.launcher = nil
	}
}

// waitForExit waits until the process is gone, or the timeout passes.
func waitForExit(pid int, timeout time.Duration) {
	if pid == 0 {
		return
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		// Signal 0 only checks that the process exists
		if process.Signal(syscall.Signal(0)) != nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	log.Printf("browser did not exit within %s, killing it", timeout)
}

// cleanup removes the launcher's temporary profile. A persistent profile is
// left alone: the launcher would delete whatever directory it was given.
func (b *Browser) cleanup(l *launcher.Launcher) {
	if b.profileDir == "" {
		l.Cleanup()
	}
}

// profileLockFiles are the files Chrome uses to claim a profile. A crash or a
// new container hostname leaves them behind, and Chrome then refuses the
// profile as in use by another computer.
var profileLockFiles = []string{"SingletonLock", "SingletonCookie", "SingletonSocket"}

// clearProfileLocks removes the stale claims on a profile. It is only safe
// because this process is the one user of the profile.
func clearProfileLocks(profileDir string) {
	for _, name := range profileLockFiles {
		if err := os.Remove(filepath.Join(profileDir, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			log.Printf("failed to remove stale profile lock %s: %v", name, err)
		}
	}
}

// Close shuts Chrome down, which flushes its cookies to the profile, and
// removes the profile if it is temporary.
func (b *Browser) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.shutdown()
	return nil
}

// ParseCookies turns a Cookie header style string ("name=value; other=value")
// into cookies for domain. An empty string yields no cookies. Cookies Go
// rejects (e.g. Google's g_state, whose value is raw JSON) are skipped, so a
// header copied from the browser can be pasted as is.
func ParseCookies(raw, domain string) ([]*proto.NetworkCookieParam, error) {
	if raw == "" {
		return nil, nil
	}

	var params []*proto.NetworkCookieParam
	for pair := range strings.SplitSeq(raw, ";") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}

		cookies, err := http.ParseCookie(pair)
		if err != nil {
			name, _, _ := strings.Cut(pair, "=")
			log.Printf("skipping cookie %q for %s: %v", name, domain, err)
			continue
		}

		for _, c := range cookies {
			params = append(params, &proto.NetworkCookieParam{
				Name:   c.Name,
				Value:  c.Value,
				Domain: domain,
				Path:   "/",
				Secure: true,
			})
		}
	}

	if len(params) == 0 {
		return nil, fmt.Errorf("no valid cookies for %s", domain)
	}
	return params, nil
}

// CookieHeader formats cookies as a Cookie request header value
// ("name=value; other=value"), the format IMMOBILIARE_COOKIES takes.
func CookieHeader(cookies []*proto.NetworkCookie) string {
	pairs := make([]string, 0, len(cookies))
	for _, cookie := range cookies {
		pairs = append(pairs, cookie.Name+"="+cookie.Value)
	}
	return strings.Join(pairs, "; ")
}

// seedFingerprint identifies a seed: the raw cookie string given for a domain.
func seedFingerprint(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// seedFingerprintPath is where the fingerprint of the last seed applied for a
// domain is kept, inside the state dir (the profile, for a local Chrome).
func seedFingerprintPath(stateDir, domain string) string {
	sum := sha256.Sum256([]byte(domain))
	return filepath.Join(stateDir, ".cookie-seed-"+hex.EncodeToString(sum[:8]))
}

// seedChanged tells whether raw differs from the seed last applied for the
// domain, which is also the case when none was applied yet.
func seedChanged(stateDir, domain, raw string) bool {
	stored, err := os.ReadFile(seedFingerprintPath(stateDir, domain))
	return err != nil || strings.TrimSpace(string(stored)) != seedFingerprint(raw)
}

func rememberSeed(stateDir, domain, raw string) error {
	return os.WriteFile(seedFingerprintPath(stateDir, domain), []byte(seedFingerprint(raw)), 0o600)
}

// cookiesToSet picks which of the given cookies to set. The given cookies are
// only a starting value: once the site has set or refreshed one (e.g.
// DataDome's), the browser's copy is kept. The exception is a seed that changed
// since it was last applied, which is a deliberate new value: all its cookies
// are set, and the browser's copies they replace are returned to be deleted.
func cookiesToSet(
	existing []*proto.NetworkCookie,
	cookies []*proto.NetworkCookieParam,
	replace bool,
) (toSet []*proto.NetworkCookieParam, toDelete []*proto.NetworkCookie) {
	for _, cookie := range cookies {
		if !replace {
			if !hasCookie(existing, cookie) {
				toSet = append(toSet, cookie)
			}
			continue
		}

		toSet = append(toSet, cookie)
		toDelete = append(toDelete, matchingCookies(existing, cookie)...)
	}
	return toSet, toDelete
}

// applySeed sets the seed's cookies on the page's browser as described in
// cookiesToSet. With a state dir it tells a changed seed from an already
// applied one by its fingerprint, and records the fingerprint once the cookies
// are set.
func (b *Browser) applySeed(page *rod.Page, cookies []*proto.NetworkCookieParam, raw string) error {
	if len(cookies) == 0 {
		return nil
	}
	domain := cookies[0].Domain

	existing, err := b.rod.GetCookies()
	if err != nil {
		return fmt.Errorf("failed to read browser cookies: %w", err)
	}

	changed := b.stateDir != "" && seedChanged(b.stateDir, domain, raw)
	toSet, toDelete := cookiesToSet(existing, cookies, changed)

	for _, stale := range toDelete {
		err := proto.NetworkDeleteCookies{Name: stale.Name, Domain: stale.Domain, Path: stale.Path}.Call(page)
		if err != nil {
			return fmt.Errorf("failed to replace cookie %s: %w", stale.Name, err)
		}
	}
	if len(toSet) > 0 {
		if err := page.SetCookies(toSet); err != nil {
			return fmt.Errorf("failed to set page cookies: %w", err)
		}
	}

	if changed {
		log.Printf("applied new starting cookies for %s (%d)", domain, len(toSet))
		if err := rememberSeed(b.stateDir, domain, raw); err != nil {
			// The next run just applies the same seed again
			log.Printf("failed to remember the cookies applied for %s: %v", domain, err)
		}
	}
	return nil
}

// matchingCookies returns the cookies in existing that hasCookie would count as
// the given cookie.
func matchingCookies(existing []*proto.NetworkCookie, cookie *proto.NetworkCookieParam) []*proto.NetworkCookie {
	var matching []*proto.NetworkCookie
	for _, e := range existing {
		if hasCookie([]*proto.NetworkCookie{e}, cookie) {
			matching = append(matching, e)
		}
	}
	return matching
}

// hasCookie reports whether existing holds a cookie with the same name for the
// cookie's domain or one of its subdomains.
func hasCookie(existing []*proto.NetworkCookie, cookie *proto.NetworkCookieParam) bool {
	domain := strings.TrimPrefix(cookie.Domain, ".")
	for _, e := range existing {
		existingDomain := strings.TrimPrefix(e.Domain, ".")
		if e.Name == cookie.Name && (existingDomain == domain || strings.HasSuffix(existingDomain, "."+domain)) {
			return true
		}
	}
	return false
}

// NewPage opens a page, relaunching Chrome once if it died. With stealth the
// stealth evasions are applied to the page; without it the page is a plain one.
// The given cookies, parsed from raw, are set before
// the page is returned unless the browser already holds them (see
// cookiesToSet); they are scoped by their domain, so providers don't see each
// other's cookies.
func (b *Browser) NewPage(stealth bool, cookies []*proto.NetworkCookieParam, raw string) (*rod.Page, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	var lastErr error
	for range 2 {
		if b.rod == nil {
			if err := b.launch(); err != nil {
				return nil, err
			}
		}

		var page *rod.Page
		var err error
		if stealth {
			page, err = stealthlib.Page(b.rod)
		} else {
			page, err = b.rod.Page(proto.TargetCreateTarget{URL: "about:blank"})
		}
		if err == nil {
			if err := b.applySeed(page, cookies, raw); err != nil {
				_ = page.Close()
				return nil, err
			}
			return page, nil
		}

		lastErr = err
		log.Printf("failed to open browser page, relaunching: %v", err)
		b.shutdown()
	}

	return nil, fmt.Errorf("failed to open browser page: %w", lastErr)
}
