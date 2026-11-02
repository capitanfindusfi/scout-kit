package browser

import (
	"fmt"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/proto"
)

// FingerprintCheckURL is the page /browsercheck opens: CreepJS scores how
// trustworthy the browser looks and lists the "lies" (tampered native
// functions, inconsistent values) that anti-bot scripts also look for.
const FingerprintCheckURL = "https://abrahamjuliot.github.io/creepjs/"

const (
	checkPageTimeout = 60 * time.Second
	// checkSettleTime is how long CreepJS gets to finish its analysis after the
	// page has loaded.
	checkSettleTime  = 20 * time.Second
	checkJPEGQuality = 80
)

// FingerprintCheck is the result of opening FingerprintCheckURL.
type FingerprintCheck struct {
	// Screenshot is a full page JPEG.
	Screenshot []byte
	UserAgent  string
}

// CheckFingerprint opens FingerprintCheckURL in the shared Chrome, with or
// without the stealth evasions, and screenshots it once it has settled.
func (b *Browser) CheckFingerprint(withStealth bool) (*FingerprintCheck, error) {
	var page *rod.Page
	var err error
	if withStealth {
		page, err = b.NewPage(true, nil, "")
	} else {
		page, err = b.NewPage(false, nil, "")
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = page.Close() }()

	if err := page.Timeout(checkPageTimeout).Navigate(FingerprintCheckURL); err != nil {
		return nil, fmt.Errorf("failed to open %s: %w", FingerprintCheckURL, err)
	}
	if err := page.Timeout(checkPageTimeout).WaitLoad(); err != nil {
		return nil, fmt.Errorf("failed to load %s: %w", FingerprintCheckURL, err)
	}
	time.Sleep(checkSettleTime)

	check := &FingerprintCheck{}
	if res, err := page.Eval("() => navigator.userAgent"); err == nil {
		check.UserAgent = res.Value.Str()
	}

	quality := checkJPEGQuality
	check.Screenshot, err = page.Timeout(checkPageTimeout).Screenshot(true, &proto.PageCaptureScreenshot{
		Format:  proto.PageCaptureScreenshotFormatJpeg,
		Quality: &quality,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to take the screenshot: %w", err)
	}
	return check, nil
}
