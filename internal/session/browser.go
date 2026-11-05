package session

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/inspector"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

//go:embed inject.js
var injectJS string

const (
	isolatedWorld = "__collab"
	selectBinding = "__collabSelect"
	pageBinding   = "__collabPage" // text-field focus and positions (see inject.js)
)

// BrowserHooks receives events from the shared browser. Implementations
// must not block.
type BrowserHooks interface {
	OnFrame(jpeg []byte)
	OnNavigated(url string)
	OnLoading()
	OnLoaded(url, title string)
	OnTitle(title string)
	OnDialogOpen(kind, message, defaultPrompt string)
	OnDialogClosed()
	OnSelectOpen(payload json.RawMessage)
	// OnPageEvent receives text-field reports from the injected script.
	OnPageEvent(payload json.RawMessage)
	// OnNavigationBlocked is called when the page tried to leave the
	// allowed URLs (link, redirect, script); the page stays where it was.
	OnNavigationBlocked(url string)
	OnFileChooser()
	OnBrowserLost()
	OnRendererCrashed()
	// ResolvePopup decides whether a popup's URL may be opened in the
	// shared tab (it is validated by the caller).
	ResolvePopup(url string)
}

type BrowserOptions struct {
	ChromePath    string
	NoSandbox     bool
	UserDataDir   string
	ProxyAddr     string
	Width, Height int
	JPEGQuality   int
	// AllowURL, if set, is checked for every top-level page request,
	// including each redirect hop. Requests it rejects are aborted.
	AllowURL func(url string) bool
}

// Browser is one Chromium process with exactly one shared tab.
type Browser struct {
	opts  BrowserOptions
	hooks BrowserHooks

	allocCancel context.CancelFunc
	ctx         context.Context // tab context
	cancel      context.CancelFunc
	targetID    target.ID

	mu          sync.Mutex
	mainFrameID cdp.FrameID
	popups      map[target.ID]bool
	closed      bool
}

// profilePrefs turns off everything that would keep what a person typed:
// address and card autofill, password saving. Pages in a session may take
// patient data and card numbers; the profile is deleted at the end anyway,
// but nothing should be stored in the first place.
const profilePrefs = `{
  "autofill": {"profile_enabled": false, "credit_card_enabled": false},
  "credentials_enable_service": false,
  "profile": {"password_manager_enabled": false},
  "translate": {"enabled": false}
}`

func writeProfilePrefs(userDataDir string) error {
	dir := filepath.Join(userDataDir, "Default")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "Preferences")); err == nil {
		return nil // a restarted browser keeps the session's profile
	}
	return os.WriteFile(filepath.Join(dir, "Preferences"), []byte(profilePrefs), 0o600)
}

func LaunchBrowser(parent context.Context, opts BrowserOptions, hooks BrowserHooks) (*Browser, error) {
	if err := writeProfilePrefs(opts.UserDataDir); err != nil {
		return nil, fmt.Errorf("profile preferences: %w", err)
	}
	allocOpts := []chromedp.ExecAllocatorOption{
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.Flag("headless", true),
		chromedp.UserDataDir(opts.UserDataDir),
		chromedp.WindowSize(opts.Width, opts.Height),
		// All traffic goes through the guarded egress proxy, including
		// loopback (Chromium bypasses the proxy for loopback by default;
		// "<-loopback>" removes that implicit rule).
		chromedp.ProxyServer("http://" + opts.ProxyAddr),
		chromedp.Flag("proxy-bypass-list", "<-loopback>"),
		// Pages must not open UDP sockets that bypass the proxy.
		chromedp.Flag("force-webrtc-ip-handling-policy", "disable_non_proxied_udp"),
		chromedp.Flag("webrtc-ip-handling-policy", "disable_non_proxied_udp"),
		// Keep rendering at full rate even though nobody "looks" at the window.
		chromedp.Flag("disable-background-timer-throttling", true),
		chromedp.Flag("disable-backgrounding-occluded-windows", true),
		chromedp.Flag("disable-renderer-backgrounding", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-breakpad", true),
		chromedp.Flag("disable-sync", true),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("disable-default-apps", true),
		chromedp.Flag("disable-features", "Translate,MediaRouter,OptimizationHints"),
		chromedp.Flag("disable-client-side-phishing-detection", true),
		chromedp.Flag("disable-component-update", true),
		chromedp.Flag("metrics-recording-only", true),
		chromedp.Flag("password-store", "basic"),
		chromedp.Flag("use-mock-keychain", true),
		chromedp.Flag("force-color-profile", "srgb"),
		chromedp.Flag("autoplay-policy", "no-user-gesture-required"),
		chromedp.Flag("mute-audio", true),
		// The profile (and so the page cache) lives in /tmp, which is memory
		// in the Docker setup: keep it bounded.
		chromedp.Flag("disk-cache-size", "104857600"),
		chromedp.Flag("lang", "en-US"),
	}
	if opts.ChromePath != "" {
		allocOpts = append(allocOpts, chromedp.ExecPath(opts.ChromePath))
	}
	if opts.NoSandbox {
		allocOpts = append(allocOpts, chromedp.NoSandbox)
	}

	allocCtx, allocCancel := chromedp.NewExecAllocator(parent, allocOpts...)
	ctx, cancel := chromedp.NewContext(allocCtx, chromedp.WithLogf(func(string, ...any) {}))
	b := &Browser{
		opts: opts, hooks: hooks, allocCancel: allocCancel, ctx: ctx, cancel: cancel,
		popups: map[target.ID]bool{},
	}

	// Starts the process and attaches to the first tab. The context given
	// to the first Run owns the browser's lifetime, so it must be the tab
	// context itself, not a derived timeout context.
	launchTimer := time.AfterFunc(30*time.Second, cancel)
	err := chromedp.Run(ctx)
	launchTimer.Stop()
	if err != nil {
		b.Close()
		return nil, fmt.Errorf("launch chromium: %w", err)
	}
	c := chromedp.FromContext(ctx)
	b.targetID = c.Target.TargetID

	chromedp.ListenTarget(ctx, b.onTargetEvent)
	chromedp.ListenBrowser(ctx, b.onBrowserEvent)

	if err := b.setup(); err != nil {
		b.Close()
		return nil, err
	}

	go func() {
		<-c.Browser.LostConnection
		b.mu.Lock()
		closed := b.closed
		b.mu.Unlock()
		if !closed {
			hooks.OnBrowserLost()
		}
	}()
	return b, nil
}

func (b *Browser) setup() error {
	ctx, cancel := context.WithTimeout(b.ctx, 20*time.Second)
	defer cancel()
	return chromedp.Run(ctx,
		chromedp.ActionFunc(func(ctx context.Context) error {
			// Fixed shared viewport, DPR 1, for the whole session.
			if err := emulation.SetDeviceMetricsOverride(int64(b.opts.Width), int64(b.opts.Height), 1, false).Do(ctx); err != nil {
				return err
			}
			// Headless Chromium advertises itself as "HeadlessChrome",
			// which many sites block.
			_, product, _, ua, _, err := browser.GetVersion().Do(ctx)
			if err == nil {
				_ = product
				ua = strings.ReplaceAll(ua, "HeadlessChrome", "Chrome")
				if err := emulation.SetUserAgentOverride(ua).WithAcceptLanguage("en-US,en").Do(ctx); err != nil {
					return err
				}
			}
			if err := browser.SetDownloadBehavior(browser.SetDownloadBehaviorBehaviorDeny).Do(ctx); err != nil {
				log.Printf("browser: download policy: %v", err)
			}
			if err := page.SetInterceptFileChooserDialog(true).Do(ctx); err != nil {
				log.Printf("browser: file chooser intercept: %v", err)
			}
			// <select> popups are native widgets that the page screencast
			// does not capture. A script in an isolated world reports
			// them, and participants pick options from the overlay.
			if err := runtime.AddBinding(selectBinding).WithExecutionContextName(isolatedWorld).Do(ctx); err != nil {
				return err
			}
			if err := runtime.AddBinding(pageBinding).WithExecutionContextName(isolatedWorld).Do(ctx); err != nil {
				return err
			}
			if b.opts.AllowURL != nil {
				// Pause every document request so off-list navigations can
				// be stopped before anything loads.
				if err := fetch.Enable().WithPatterns([]*fetch.RequestPattern{{
					URLPattern: "*", ResourceType: network.ResourceTypeDocument, RequestStage: fetch.RequestStageRequest,
				}}).Do(ctx); err != nil {
					return err
				}
			}
			if _, err := page.AddScriptToEvaluateOnNewDocument(injectJS).WithWorldName(isolatedWorld).WithRunImmediately(true).Do(ctx); err != nil {
				return err
			}
			if err := target.SetDiscoverTargets(true).Do(cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Browser)); err != nil {
				log.Printf("browser: discover targets: %v", err)
			}
			tree, err := page.GetFrameTree().Do(ctx)
			if err == nil && tree != nil && tree.Frame != nil {
				b.mu.Lock()
				b.mainFrameID = tree.Frame.ID
				b.mu.Unlock()
			}
			return page.StartScreencast().
				WithFormat(page.ScreencastFormatJpeg).
				WithQuality(int64(b.opts.JPEGQuality)).
				WithMaxWidth(int64(b.opts.Width)).
				WithMaxHeight(int64(b.opts.Height)).
				WithEveryNthFrame(1).
				Do(ctx)
		}),
	)
}

func (b *Browser) isMainFrame(id cdp.FrameID) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.mainFrameID == "" || b.mainFrameID == id
}

func (b *Browser) onTargetEvent(ev any) {
	switch e := ev.(type) {
	case *page.EventScreencastFrame:
		sid := e.SessionID
		go func() { _ = chromedp.Run(b.ctx, page.ScreencastFrameAck(sid)) }()
		data, err := base64.StdEncoding.DecodeString(e.Data)
		if err == nil {
			b.hooks.OnFrame(data)
		}
	case *page.EventFrameNavigated:
		if e.Frame != nil && e.Frame.ParentID == "" {
			b.mu.Lock()
			b.mainFrameID = e.Frame.ID
			b.mu.Unlock()
			b.hooks.OnNavigated(e.Frame.URL + e.Frame.URLFragment)
		}
	case *page.EventNavigatedWithinDocument:
		if b.isMainFrame(e.FrameID) {
			b.hooks.OnNavigated(e.URL)
		}
	case *page.EventFrameStartedLoading:
		if b.isMainFrame(e.FrameID) {
			b.hooks.OnLoading()
		}
	case *page.EventLoadEventFired:
		go func() {
			var url, title string
			ctx, cancel := context.WithTimeout(b.ctx, 3*time.Second)
			defer cancel()
			_ = chromedp.Run(ctx, chromedp.Location(&url), chromedp.Title(&title))
			b.hooks.OnLoaded(url, title)
		}()
	case *page.EventJavascriptDialogOpening:
		b.hooks.OnDialogOpen(string(e.Type), e.Message, e.DefaultPrompt)
	case *page.EventJavascriptDialogClosed:
		b.hooks.OnDialogClosed()
	case *page.EventFileChooserOpened:
		b.hooks.OnFileChooser()
	case *fetch.EventRequestPaused:
		b.onDocumentRequest(e)
	case *runtime.EventBindingCalled:
		switch {
		case e.Name == selectBinding && len(e.Payload) < 256<<10:
			b.hooks.OnSelectOpen(json.RawMessage(e.Payload))
		case e.Name == pageBinding && len(e.Payload) < 64<<10:
			b.hooks.OnPageEvent(json.RawMessage(e.Payload))
		}
	case *inspector.EventTargetCrashed:
		b.hooks.OnRendererCrashed()
	}
}

// onDocumentRequest enforces AllowURL on top-level page loads. Frames
// inside the page (iframes) aren't restricted. Aborting (rather than
// failing) the request leaves the current page on screen instead of an
// error page.
func (b *Browser) onDocumentRequest(e *fetch.EventRequestPaused) {
	allow := b.opts.AllowURL == nil || e.Request == nil || !b.isMainFrame(e.FrameID) || b.opts.AllowURL(e.Request.URL)
	id := e.RequestID
	// CDP calls can't be made from inside the event handler.
	go func() {
		if allow {
			_ = b.run(5*time.Second, fetch.ContinueRequest(id))
			return
		}
		_ = b.run(5*time.Second, fetch.FailRequest(id, network.ErrorReasonAborted))
		b.hooks.OnNavigationBlocked(e.Request.URL)
	}()
}

func (b *Browser) onBrowserEvent(ev any) {
	switch e := ev.(type) {
	case *target.EventTargetCreated:
		b.maybePopup(e.TargetInfo)
	case *target.EventTargetInfoChanged:
		if e.TargetInfo != nil && e.TargetInfo.TargetID == b.targetID {
			if e.TargetInfo.Title != "" {
				b.hooks.OnTitle(e.TargetInfo.Title)
			}
			return
		}
		b.maybePopup(e.TargetInfo)
	}
}

// maybePopup folds new tabs/windows opened by the page back into the
// single shared tab: the popup's URL is opened in the shared tab and the
// popup is closed. (Multi-tab support can build on this later.)
func (b *Browser) maybePopup(info *target.Info) {
	if info == nil || info.Type != "page" || info.TargetID == b.targetID {
		return
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	first := !b.popups[info.TargetID]
	b.popups[info.TargetID] = true
	b.mu.Unlock()

	id := info.TargetID
	url := info.URL
	closeIt := func() {
		ctx, cancel := context.WithTimeout(b.ctx, 3*time.Second)
		defer cancel()
		_ = target.CloseTarget(id).Do(cdp.WithExecutor(ctx, chromedp.FromContext(b.ctx).Browser))
	}
	if url != "" && url != "about:blank" {
		b.mu.Lock()
		delete(b.popups, id)
		b.mu.Unlock()
		go func() {
			closeIt()
			b.hooks.ResolvePopup(url)
		}()
		return
	}
	if first {
		// Popups often start at about:blank and navigate shortly after.
		go func() {
			time.Sleep(3 * time.Second)
			b.mu.Lock()
			pending := b.popups[id]
			delete(b.popups, id)
			b.mu.Unlock()
			if pending {
				closeIt()
			}
		}()
	}
}

// run executes CDP actions against the shared tab with a timeout.
func (b *Browser) run(timeout time.Duration, actions ...chromedp.Action) error {
	ctx, cancel := context.WithTimeout(b.ctx, timeout)
	defer cancel()
	return chromedp.Run(ctx, actions...)
}

// Do executes a single low-level action (used by the input worker).
func (b *Browser) Do(a chromedp.Action) error { return b.run(5*time.Second, a) }

// Navigate opens url in the shared tab. Returns the browser's error text
// (e.g. net::ERR_NAME_NOT_RESOLVED) if navigation failed.
func (b *Browser) Navigate(url string) error {
	return b.run(45*time.Second, chromedp.ActionFunc(func(ctx context.Context) error {
		_, _, errText, err := page.Navigate(url).Do(ctx)
		if err != nil {
			return err
		}
		if errText != "" {
			return errors.New(errText)
		}
		return nil
	}))
}

func (b *Browser) historyStep(delta int) error {
	return b.run(15*time.Second, chromedp.ActionFunc(func(ctx context.Context) error {
		cur, entries, err := page.GetNavigationHistory().Do(ctx)
		if err != nil {
			return err
		}
		i := int(cur) + delta
		if i < 0 || i >= len(entries) {
			return nil
		}
		return page.NavigateToHistoryEntry(entries[i].ID).Do(ctx)
	}))
}

func (b *Browser) Back() error    { return b.historyStep(-1) }
func (b *Browser) Forward() error { return b.historyStep(1) }
func (b *Browser) Reload() error  { return b.run(15*time.Second, page.Reload()) }

func (b *Browser) HandleDialog(accept bool, text string) error {
	p := page.HandleJavaScriptDialog(accept)
	if text != "" {
		p = p.WithPromptText(text)
	}
	return b.run(5*time.Second, p)
}

// ChooseSelectOption sets a <select>'s value in the page's main world and
// fires input/change so frameworks see the change.
func (b *Browser) ChooseSelectOption(id string, index int) error {
	idJSON, _ := json.Marshal(id)
	expr := fmt.Sprintf(`(() => {
  const id = %s, idx = %d;
  const s = Array.from(document.querySelectorAll('select[data-collab-select]')).find(el => el.dataset.collabSelect === id);
  if (!s || idx < 0 || idx >= s.options.length || s.options[idx].disabled) return false;
  s.focus();
  if (s.selectedIndex !== idx) {
    s.selectedIndex = idx;
    s.dispatchEvent(new Event('input', { bubbles: true }));
    s.dispatchEvent(new Event('change', { bubbles: true }));
  }
  return true;
})()`, idJSON, index)
	var ok bool
	return b.run(5*time.Second, chromedp.Evaluate(expr, &ok))
}

// Eval runs a JS expression in the page (used by tests and diagnostics).
func (b *Browser) Eval(expr string, out any) error {
	return b.run(10*time.Second, chromedp.Evaluate(expr, out))
}

func (b *Browser) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	b.mu.Unlock()
	if b.cancel != nil {
		b.cancel()
	}
	if b.allocCancel != nil {
		b.allocCancel() // waits for the process to exit
	}
}
