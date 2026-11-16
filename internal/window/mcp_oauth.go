package window

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/diamondburned/gotk4-adwaita/pkg/adw"
	"github.com/diamondburned/gotk4/pkg/glib/v2"
	"github.com/diamondburned/gotk4/pkg/gtk/v4"
	"github.com/diamondburned/gotkit/gtkutil"
	"libdb.so/gluppencode/internal/opencode"
)

const (
	mcpOAuthPollMS       = 500
	mcpOAuthSyncTimeout  = 30 * time.Second
	mcpOAuthCancelTimeout = 5 * time.Second
)

type mcpOAuthDialog struct {
	mgr           *mcpManager
	w             *Window
	serverName    string
	integrationID string
	dir           string

	dlg               *adw.Dialog
	toastOverlay      *adw.ToastOverlay
	browserWarnBanner *adw.Banner
	viewStack         *gtk.Stack

	startingPage   *adw.StatusPage
	waitingRow     *adw.ActionRow
	openBrowserBtn *gtk.Button
	copyLinkBtn    *gtk.Button
	formGroup      *adw.PreferencesGroup
	codeEntryRow   *adw.EntryRow
	submitBtn      *gtk.Button
	entryErrLabel  *gtk.Label

	completingPage *adw.StatusPage

	errorPage      *adw.StatusPage
	errDetailLabel *gtk.Label
	retryBtn       *gtk.Button
	errCloseBtn    *gtk.Button

	gen             uint64
	pollGen         uint64
	syncGen         uint64
	attemptID       string
	authURL         string
	attemptMode     string
	attemptPaste    bool
	pasteAccepted   bool
	submitting      bool
	authCompleted   bool
	syncDone        bool
	retrySyncOnly   bool
	closed          bool
	lastUnreachable string
	pollInFlight    bool
	pollTimer       glib.SourceHandle
	deadlineTimer   glib.SourceHandle
	syncTimer       glib.SourceHandle
	startCancel     context.CancelFunc
	closedFlag      atomic.Bool
}

func (m *mcpManager) closeAuthDialog() {
	if m == nil || m.authDlg == nil {
		return
	}
	m.authDlg.close()
}

func (m *mcpManager) openAuthDialog(serverName string) {
	if m.isDisposed() || m.w.client == nil {
		return
	}
	srv, ok := m.findServer(serverName)
	if !ok || strings.TrimSpace(srv.IntegrationID) == "" {
		m.showToast("Sign in via OpenCode CLI or web UI")
		return
	}
	if m.authDlg != nil {
		if m.authDlg.serverName == serverName && !m.authDlg.closed {
			if m.w.ApplicationWindow != nil {
				m.authDlg.dlg.Present(m.w.ApplicationWindow)
			}
			return
		}
		m.closeAuthDialog()
	}

	d := newMcpOAuthDialog(m, srv.Name, srv.IntegrationID, m.dir)
	m.authDlg = d
	if m.w.ApplicationWindow != nil {
		d.dlg.Present(m.w.ApplicationWindow)
	}
	d.startAttempt()
}

func newMcpOAuthDialog(mgr *mcpManager, serverName, integrationID, dir string) *mcpOAuthDialog {
	d := &mcpOAuthDialog{
		mgr:           mgr,
		w:             mgr.w,
		serverName:    serverName,
		integrationID: integrationID,
		dir:           dir,
	}

	dlg := adw.NewDialog()
	dlg.SetTitle("Sign in to " + serverName)
	dlg.SetContentWidth(460)
	dlg.SetFollowsContentSize(true)

	toolbar := adw.NewToolbarView()
	toolbar.SetTopBarStyle(adw.ToolbarFlat)

	header := adw.NewHeaderBar()
	headerTitle := adw.NewWindowTitle("Sign in to "+serverName, shortDir(dir))
	header.SetTitleWidget(headerTitle)
	toolbar.AddTopBar(header)

	browserWarnBanner := adw.NewBanner("")
	browserWarnBanner.SetUseMarkup(false)
	browserWarnBanner.SetRevealed(false)
	toolbar.AddTopBar(browserWarnBanner)

	toastOverlay := adw.NewToastOverlay()

	outerScroll := gtk.NewScrolledWindow()
	outerScroll.SetPolicy(gtk.PolicyNever, gtk.PolicyAutomatic)
	outerScroll.SetPropagateNaturalHeight(true)
	outerScroll.SetMaxContentHeight(520)

	clamp := adw.NewClamp()
	clamp.SetMaximumSize(440)

	viewStack := gtk.NewStack()
	viewStack.SetVhomogeneous(false)
	viewStack.SetHhomogeneous(false)

	// 1. Starting page
	startingPage := adw.NewStatusPage()
	startingPage.AddCSSClass("compact")
	startingPage.SetPaintable(adw.NewSpinnerPaintable(startingPage))
	startingPage.SetTitle("Starting sign-in…")
	startingPage.SetDescription("Contacting OpenCode to prepare OAuth authorization.")
	startCancelBtn := gtk.NewButtonWithLabel("Cancel")
	startCancelBtn.AddCSSClass("pill")
	startCancelBtn.SetHAlign(gtk.AlignCenter)
	startCancelBtn.ConnectClicked(func() {
		dlg.Close()
	})
	startingPage.SetChild(startCancelBtn)
	viewStack.AddNamed(startingPage, "starting")

	// 2. Waiting page
	waitingBox := gtk.NewBox(gtk.OrientationVertical, 16)
	waitingBox.SetMarginTop(8)
	waitingBox.SetMarginBottom(20)
	waitingBox.SetMarginStart(20)
	waitingBox.SetMarginEnd(20)

	statusGroup := adw.NewPreferencesGroup()
	waitingRow := adw.NewActionRow()
	waitingRow.SetUseMarkup(false)
	waitingRow.SetSubtitleLines(0)
	waitingRow.SetSubtitleSelectable(true)
	waitSpinner := adw.NewSpinner()
	waitSpinner.SetSizeRequest(16, 16)
	waitSpinner.SetVAlign(gtk.AlignCenter)
	waitingRow.AddPrefix(waitSpinner)
	waitingRow.SetTitle("Waiting for browser authorization…")
	statusGroup.Add(waitingRow)
	waitingBox.Append(statusGroup)

	btnRow := gtk.NewBox(gtk.OrientationHorizontal, 10)
	btnRow.SetHAlign(gtk.AlignCenter)

	openBrowserBtn, _ := newIconTextButton("open-in-new-symbolic", "Open Browser")
	openBrowserBtn.AddCSSClass("pill")
	openBrowserBtn.ConnectClicked(func() {
		d.launchBrowser()
	})
	btnRow.Append(openBrowserBtn)

	copyLinkBtn, _ := newIconTextButton("edit-copy-symbolic", "Copy Link")
	copyLinkBtn.AddCSSClass("pill")
	copyLinkBtn.ConnectClicked(func() {
		if d.authURL != "" {
			d.w.copyTextToClipboard(d.authURL)
			d.showToast("Authorization link copied")
		}
	})
	btnRow.Append(copyLinkBtn)
	waitingBox.Append(btnRow)

	formBox := gtk.NewBox(gtk.OrientationVertical, 6)
	formGroup := adw.NewPreferencesGroup()
	codeEntryRow := adw.NewEntryRow()
	codeEntryRow.SetUseMarkup(false)
	codeEntryRow.SetTitle("Redirect URL or code")
	codeEntryRow.ConnectChanged(func() {
		d.setEntryError("")
	})
	codeEntryRow.ConnectEntryActivated(func() {
		d.submitCode()
	})

	submitBtn := gtk.NewButtonWithLabel("Submit")
	submitBtn.SetVAlign(gtk.AlignCenter)
	submitBtn.ConnectClicked(func() {
		d.submitCode()
	})
	codeEntryRow.AddSuffix(submitBtn)
	formGroup.Add(codeEntryRow)
	formBox.Append(formGroup)

	entryErrLabel := gtk.NewLabel("")
	entryErrLabel.SetUseMarkup(false)
	entryErrLabel.AddCSSClass("error")
	entryErrLabel.AddCSSClass("caption")
	entryErrLabel.SetXAlign(0)
	entryErrLabel.SetWrap(true)
	entryErrLabel.SetSelectable(true)
	entryErrLabel.SetVisible(false)
	formBox.Append(entryErrLabel)
	waitingBox.Append(formBox)

	viewStack.AddNamed(waitingBox, "waiting")

	// 3. Completing page
	completingPage := adw.NewStatusPage()
	completingPage.AddCSSClass("compact")
	completingPage.SetPaintable(adw.NewSpinnerPaintable(completingPage))
	completingPage.SetTitle("Finishing sign-in…")
	completingPage.SetDescription("Refreshing MCP status and connecting server.")
	viewStack.AddNamed(completingPage, "completing")

	// 4. Error page
	errorPage := adw.NewStatusPage()
	errorPage.AddCSSClass("compact")
	errorPage.SetIconName("shield-symbolic")
	errorPage.SetTitle("Sign-in Failed")

	errBody := gtk.NewBox(gtk.OrientationVertical, 14)
	errBody.SetHAlign(gtk.AlignCenter)

	errDetailLabel := gtk.NewLabel("")
	errDetailLabel.SetUseMarkup(false)
	errDetailLabel.AddCSSClass("dim-label")
	errDetailLabel.SetWrap(true)
	errDetailLabel.SetJustify(gtk.JustifyCenter)
	errDetailLabel.SetSelectable(true)
	errBody.Append(errDetailLabel)

	errBtnBox := gtk.NewBox(gtk.OrientationHorizontal, 10)
	errBtnBox.SetHAlign(gtk.AlignCenter)

	retryBtn, _ := newIconTextButton("refresh-symbolic", "Try Again")
	retryBtn.AddCSSClass("pill")
	retryBtn.AddCSSClass("suggested-action")
	retryBtn.ConnectClicked(func() {
		if d.retrySyncOnly {
			d.gen++
			d.startSyncAndConnect(d.gen)
		} else {
			d.startAttempt()
		}
	})
	errBtnBox.Append(retryBtn)

	errCloseBtn := gtk.NewButtonWithLabel("Close")
	errCloseBtn.AddCSSClass("pill")
	errCloseBtn.ConnectClicked(func() {
		dlg.Close()
	})
	errBtnBox.Append(errCloseBtn)

	errBody.Append(errBtnBox)
	errorPage.SetChild(errBody)
	viewStack.AddNamed(errorPage, "error")

	clamp.SetChild(viewStack)
	outerScroll.SetChild(clamp)
	toastOverlay.SetChild(outerScroll)
	toolbar.SetContent(toastOverlay)
	dlg.SetChild(toolbar)

	dlg.ConnectClosed(func() {
		d.onDialogClosed()
	})

	d.dlg = dlg
	d.toastOverlay = toastOverlay
	d.browserWarnBanner = browserWarnBanner
	d.viewStack = viewStack
	d.startingPage = startingPage
	d.waitingRow = waitingRow
	d.openBrowserBtn = openBrowserBtn
	d.copyLinkBtn = copyLinkBtn
	d.formGroup = formGroup
	d.codeEntryRow = codeEntryRow
	d.submitBtn = submitBtn
	d.entryErrLabel = entryErrLabel
	d.completingPage = completingPage
	d.errorPage = errorPage
	d.errDetailLabel = errDetailLabel
	d.retryBtn = retryBtn
	d.errCloseBtn = errCloseBtn

	return d
}

func (d *mcpOAuthDialog) showToast(msg string) {
	if d.closed || d.toastOverlay == nil || strings.TrimSpace(msg) == "" {
		return
	}
	toast := adw.NewToast(msg)
	toast.SetUseMarkup(false)
	toast.SetTimeout(3)
	d.toastOverlay.AddToast(toast)
}

func (d *mcpOAuthDialog) setBrowserError(err error) {
	if d.closed || d.browserWarnBanner == nil {
		return
	}
	if err == nil {
		d.browserWarnBanner.SetRevealed(false)
		return
	}
	msg := opencode.HumanizeAPIError(err)
	if msg == "" {
		msg = "browser launch failed"
	}
	d.browserWarnBanner.SetTitle(fmt.Sprintf("Could not open browser automatically (%s). Open the link or copy it below.", msg))
	d.browserWarnBanner.SetRevealed(true)
}

func (d *mcpOAuthDialog) setEntryError(msg string) {
	if d.entryErrLabel == nil {
		return
	}
	msg = strings.TrimSpace(msg)
	d.entryErrLabel.SetText(msg)
	d.entryErrLabel.SetVisible(msg != "")
}

func (d *mcpOAuthDialog) clearPollTimer() {
	if d.pollTimer != 0 {
		glib.SourceRemove(d.pollTimer)
		d.pollTimer = 0
	}
}

func (d *mcpOAuthDialog) clearDeadlineTimer() {
	if d.deadlineTimer != 0 {
		glib.SourceRemove(d.deadlineTimer)
		d.deadlineTimer = 0
	}
}

func (d *mcpOAuthDialog) clearSyncTimer() {
	if d.syncTimer != 0 {
		glib.SourceRemove(d.syncTimer)
		d.syncTimer = 0
	}
}

func (d *mcpOAuthDialog) clearAllTimers() {
	d.clearPollTimer()
	d.clearDeadlineTimer()
	d.clearSyncTimer()
}

func (d *mcpOAuthDialog) cancelStartInFlight() {
	if d.startCancel != nil {
		d.startCancel()
		d.startCancel = nil
	}
}

func (d *mcpOAuthDialog) startAttempt() {
	if d.closed || d.w == nil || d.w.client == nil || d.w.ctx == nil || d.w.ctx.Err() != nil {
		return
	}
	d.cancelStartInFlight()
	d.clearAllTimers()
	d.gen++
	d.pollGen++
	d.syncGen++
	gen := d.gen

	if d.attemptID != "" && !d.authCompleted && !d.pasteAccepted {
		d.cancelServerAttemptAsync(d.attemptID)
	}

	d.attemptID = ""
	d.authURL = ""
	d.attemptMode = ""
	d.attemptPaste = false
	d.pasteAccepted = false
	d.submitting = false
	d.authCompleted = false
	d.syncDone = false
	d.retrySyncOnly = false
	d.lastUnreachable = ""
	d.pollInFlight = false

	d.codeEntryRow.SetText("")
	d.codeEntryRow.SetSensitive(true)
	d.submitBtn.SetSensitive(true)
	d.setEntryError("")
	d.setBrowserError(nil)
	d.viewStack.SetVisibleChildName("starting")

	client := d.w.client
	integrationID := d.integrationID
	dir := d.dir
	infoCtx, infoCancel := context.WithTimeout(d.w.ctx, mcpFetchTimeout)
	d.startCancel = infoCancel

	gtkutil.Async(d.w.ctx, func() func() {
		defer infoCancel()

		info, err := client.GetIntegration(infoCtx, integrationID, dir)
		if err != nil {
			return d.finishStartAttempt(gen, nil, err)
		}
		if d.closedFlag.Load() {
			return nil
		}
		method, err := opencode.SelectOAuthMethod(info)
		if err != nil {
			return d.finishStartAttempt(gen, nil, err)
		}
		// Once POST .../connect/oauth starts, let it receive the server response so
		// a close-in-flight still learns the attemptID and issues DELETE cleanup.
		startCtx, startCancel := context.WithTimeout(context.Background(), mcpFetchTimeout)
		defer startCancel()
		attempt, err := client.StartIntegrationOAuth(startCtx, integrationID, dir, opencode.OAuthConnectRequest{
			MethodID: method.ID,
		})
		return d.finishStartAttempt(gen, attempt, err)
	})
}

func (d *mcpOAuthDialog) finishStartAttempt(gen uint64, attempt *opencode.OAuthAttempt, err error) func() {
	return func() {
		if d.closed || d.gen != gen {
			if attempt != nil && attempt.AttemptID != "" {
				d.cancelServerAttemptAsync(attempt.AttemptID)
			}
			return
		}
		d.startCancel = nil

		if err != nil {
			d.showError("Could Not Start Sign-In", opencode.HumanizeAPIError(err), "shield-symbolic", false)
			return
		}
		if attempt == nil || strings.TrimSpace(attempt.AttemptID) == "" {
			d.showError("Could Not Start Sign-In", "OpenCode did not return an OAuth attempt ID.", "shield-symbolic", false)
			return
		}

		validURL, urlErr := opencode.ValidateExternalAuthURL(attempt.URL)
		if urlErr != nil {
			d.cancelServerAttemptAsync(attempt.AttemptID)
			d.showError("Invalid Authorization URL", urlErr.Error(), "shield-symbolic", false)
			return
		}

		d.attemptID = attempt.AttemptID
		d.authURL = validURL
		d.attemptMode = attempt.Mode
		d.attemptPaste = attempt.Paste

		d.setBrowserError(nil)
		d.setEntryError("")
		d.openBrowserBtn.SetSensitive(true)
		d.copyLinkBtn.SetSensitive(true)

		instructions := strings.TrimSpace(attempt.Instructions)
		if instructions == "" {
			switch {
			case attempt.Mode == "code":
				instructions = "Sign in in your browser, then paste the authorization code below."
			case attempt.Paste:
				instructions = "Sign in in your browser. If it does not return automatically, paste the redirected URL from your address bar below."
			default:
				instructions = "Finish signing in via your browser. This dialog will update automatically once complete."
			}
		}
		if attempt.Mode == "code" {
			d.waitingRow.SetTitle("Waiting for authorization code…")
		} else {
			d.waitingRow.SetTitle("Waiting for browser authorization…")
		}
		d.waitingRow.SetSubtitle(instructions)

		showForm := attempt.Mode == "code" || attempt.Paste
		d.formGroup.SetVisible(showForm)
		d.entryErrLabel.SetVisible(false)
		d.openBrowserBtn.RemoveCSSClass("suggested-action")
		d.submitBtn.RemoveCSSClass("suggested-action")
		if attempt.Mode == "code" {
			d.formGroup.SetTitle("Authorization Code")
			d.formGroup.SetDescription("Paste the code provided after signing in.")
			d.codeEntryRow.SetTitle("Authorization code")
			d.submitBtn.AddCSSClass("suggested-action")
		} else if attempt.Paste {
			d.formGroup.SetTitle("Manual Completion")
			d.formGroup.SetDescription("If the browser redirect cannot reach OpenCode, paste the full redirect URL below.")
			d.codeEntryRow.SetTitle("Redirect URL or code")
			d.openBrowserBtn.AddCSSClass("suggested-action")
		} else {
			d.openBrowserBtn.AddCSSClass("suggested-action")
		}

		d.viewStack.SetVisibleChildName("waiting")
		if attempt.Mode == "code" {
			d.codeEntryRow.GrabFocus()
		}

		d.launchBrowser()
		d.startDeadlineTimer(gen, attempt.Lifetime())
		if attempt.Mode != "code" {
			d.scheduleNextPoll(gen, mcpOAuthPollMS)
		}
	}
}

func (d *mcpOAuthDialog) launchBrowser() {
	if d.closed || d.w == nil || d.authURL == "" {
		return
	}
	gen := d.gen
	d.setBrowserError(nil)
	d.w.launchExternalURI(d.authURL, func(err error) {
		if d.closed || d.gen != gen || (d.w.ctx != nil && d.w.ctx.Err() != nil) {
			return
		}
		if errors.Is(err, context.Canceled) {
			return
		}
		d.setBrowserError(err)
	})
}

func (d *mcpOAuthDialog) startDeadlineTimer(gen uint64, lifetime time.Duration) {
	d.clearDeadlineTimer()
	ms := lifetime.Milliseconds()
	if ms < 1 {
		ms = 1
	}
	if ms > math.MaxUint32 {
		ms = math.MaxUint32
	}
	var id glib.SourceHandle
	id = glib.TimeoutAdd(uint(ms), func() bool {
		if d.deadlineTimer == id {
			d.deadlineTimer = 0
		}
		if d.closed || d.gen != gen || d.authCompleted {
			return false
		}
		d.clearPollTimer()
		d.pollGen++
		d.pollInFlight = false

		if d.attemptID != "" && !d.pasteAccepted {
			d.cancelServerAttemptAsync(d.attemptID)
			d.attemptID = ""
		}
		if d.lastUnreachable != "" {
			d.showError("Sign-in Failed", d.lastUnreachable, "cloud-off-symbolic", false)
		} else {
			d.showError("Sign-in Expired", "The authorization session timed out before completing. Start a new attempt to try again.", "shield-symbolic", false)
		}
		return false
	})
	d.deadlineTimer = id
}

func (d *mcpOAuthDialog) scheduleNextPoll(gen uint64, delayMS uint) {
	d.clearPollTimer()
	if d.closed || d.gen != gen || d.authCompleted || d.attemptID == "" {
		return
	}
	var id glib.SourceHandle
	id = glib.TimeoutAdd(delayMS, func() bool {
		if d.pollTimer == id {
			d.pollTimer = 0
		}
		if d.closed || d.gen != gen || d.authCompleted {
			return false
		}
		d.pollStatusOnce(gen)
		return false
	})
	d.pollTimer = id
}

func (d *mcpOAuthDialog) pollStatusOnce(gen uint64) {
	if d.closed || d.gen != gen || d.authCompleted || d.pollInFlight || d.attemptID == "" {
		return
	}
	if d.w == nil || d.w.client == nil || d.w.ctx == nil || d.w.ctx.Err() != nil {
		return
	}
	d.pollInFlight = true
	pollGen := d.pollGen
	attemptID := d.attemptID
	client := d.w.client
	integrationID := d.integrationID
	dir := d.dir

	gtkutil.Async(d.w.ctx, func() func() {
		reqCtx, cancel := context.WithTimeout(d.w.ctx, mcpFetchTimeout)
		defer cancel()
		status, err := client.GetIntegrationOAuthStatus(reqCtx, integrationID, attemptID, dir)
		return func() {
			if d.closed || d.gen != gen || d.pollGen != pollGen || d.authCompleted || d.attemptID != attemptID {
				return
			}
			d.pollInFlight = false

			if err != nil {
				if isOAuthAttemptNotFoundErr(err) {
					d.clearDeadlineTimer()
					d.attemptID = ""
					d.showError("Sign-in Expired", opencode.HumanizeAPIError(err), "shield-symbolic", false)
					return
				}
				d.lastUnreachable = opencode.HumanizeAPIError(err)
				d.scheduleNextPoll(gen, mcpOAuthPollMS)
				return
			}
			if status == nil {
				d.scheduleNextPoll(gen, mcpOAuthPollMS)
				return
			}
			d.lastUnreachable = ""

			switch status.Status {
			case "pending":
				d.scheduleNextPoll(gen, mcpOAuthPollMS)
			case "complete":
				d.clearPollTimer()
				d.clearDeadlineTimer()
				d.authCompleted = true
				d.startSyncAndConnect(gen)
			case "expired":
				d.clearPollTimer()
				d.clearDeadlineTimer()
				d.attemptID = ""
				d.showError("Sign-in Expired", "The authorization session timed out before completing.", "shield-symbolic", false)
			case "failed":
				d.clearPollTimer()
				d.clearDeadlineTimer()
				d.attemptID = ""
				msg := strings.TrimSpace(status.Message)
				if msg == "" {
					msg = "Authorization failed."
				}
				d.showError("Sign-in Failed", msg, "shield-symbolic", false)
			default:
				d.scheduleNextPoll(gen, mcpOAuthPollMS)
			}
		}
	})
}

func (d *mcpOAuthDialog) submitCode() {
	if d.closed || d.submitting || d.pasteAccepted || d.authCompleted || d.attemptID == "" {
		return
	}
	if d.w == nil || d.w.client == nil || d.w.ctx == nil || d.w.ctx.Err() != nil {
		return
	}
	code := strings.TrimSpace(d.codeEntryRow.Text())
	if code == "" {
		if d.attemptMode == "code" {
			d.setEntryError("Enter the authorization code first.")
		} else {
			d.setEntryError("Paste the redirect URL or code first.")
		}
		d.codeEntryRow.GrabFocus()
		return
	}

	d.setEntryError("")
	d.submitting = true
	d.codeEntryRow.SetSensitive(false)
	d.submitBtn.SetSensitive(false)
	d.waitingRow.SetTitle("Submitting…")

	gen := d.gen
	attemptID := d.attemptID
	client := d.w.client
	integrationID := d.integrationID
	dir := d.dir

	gtkutil.Async(d.w.ctx, func() func() {
		reqCtx, cancel := context.WithTimeout(d.w.ctx, mcpFetchTimeout)
		defer cancel()
		err := client.CompleteIntegrationOAuth(reqCtx, integrationID, attemptID, dir, code)
		return func() {
			if d.closed || d.gen != gen || d.attemptID != attemptID {
				return
			}
			d.submitting = false
			if err != nil {
				if isOAuthAttemptNotFoundErr(err) {
					d.clearPollTimer()
					d.clearDeadlineTimer()
					d.attemptID = ""
					d.showError("Sign-in Expired", opencode.HumanizeAPIError(err), "shield-symbolic", false)
					return
				}
				d.codeEntryRow.SetSensitive(true)
				d.submitBtn.SetSensitive(true)
				if d.attemptMode == "code" {
					d.waitingRow.SetTitle("Waiting for authorization code…")
				} else {
					d.waitingRow.SetTitle("Waiting for browser authorization…")
				}
				d.setEntryError(opencode.HumanizeAPIError(err))
				d.codeEntryRow.GrabFocus()
				return
			}

			d.pasteAccepted = true
			d.codeEntryRow.SetSensitive(false)
			d.submitBtn.SetSensitive(false)
			d.waitingRow.SetTitle("Completing sign-in…")

			// Supersede any in-flight poll and check status immediately.
			d.clearPollTimer()
			d.pollGen++
			d.pollInFlight = false
			d.pollStatusOnce(gen)
		}
	})
}

func (d *mcpOAuthDialog) startSyncAndConnect(gen uint64) {
	if d.closed || d.w == nil || d.w.client == nil || d.w.ctx == nil || d.w.ctx.Err() != nil {
		return
	}
	d.clearSyncTimer()
	d.syncGen++
	syncGen := d.syncGen
	d.retrySyncOnly = false
	d.viewStack.SetVisibleChildName("completing")

	var timerID glib.SourceHandle
	timerID = glib.TimeoutAdd(uint(mcpOAuthSyncTimeout.Milliseconds()), func() bool {
		if d.syncTimer == timerID {
			d.syncTimer = 0
		}
		if d.closed || d.gen != gen || d.syncGen != syncGen {
			return false
		}
		d.syncGen++
		d.showError("Could Not Refresh MCP Status", "Timed out waiting for OpenCode to refresh MCP servers.", "cloud-off-symbolic", true)
		return false
	})
	d.syncTimer = timerID

	client := d.w.client
	serverName := d.serverName
	dir := d.dir
	ctx := d.w.ctx

	gtkutil.Async(ctx, func() func() {
		reqCtx, cancel := context.WithTimeout(ctx, mcpOAuthSyncTimeout)
		defer cancel()

		mcpRes, listErr := client.ListMCP(reqCtx, dir)
		if listErr != nil {
			return d.finishSyncAndConnect(gen, syncGen, nil, nil, nil, listErr)
		}

		var connErr error
		if srv, ok := findServerInSlice(mcpRes.Servers, serverName); ok {
			if srv.Status.Status == "disabled" || srv.Status.Status == "needs_auth" {
				connErr = client.ConnectMCP(reqCtx, serverName, dir)
				if mcpRes2, listErr2 := client.ListMCP(reqCtx, dir); listErr2 == nil && mcpRes2 != nil {
					mcpRes = mcpRes2
				} else if connErr == nil && listErr2 != nil {
					return d.finishSyncAndConnect(gen, syncGen, nil, nil, nil, listErr2)
				}
			}
		}

		var resCat *opencode.McpResourceListResult
		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			if r, err := client.ListMCPResources(reqCtx, dir); err == nil {
				resCat = r
			}
		}()
		wg.Wait()

		return d.finishSyncAndConnect(gen, syncGen, mcpRes, resCat, connErr, nil)
	})
}

func (d *mcpOAuthDialog) finishSyncAndConnect(
	gen, syncGen uint64,
	mcpRes *opencode.McpListResult,
	resCat *opencode.McpResourceListResult,
	connErr, listErr error,
) func() {
	return func() {
		if d.closed || d.gen != gen || d.syncGen != syncGen {
			return
		}
		d.clearSyncTimer()

		if listErr != nil {
			d.showError("Could Not Refresh MCP Status", opencode.HumanizeAPIError(listErr), "cloud-off-symbolic", true)
			return
		}

		if mcpRes != nil && d.w != nil {
			d.w.ensureMcpMaps()
			resolvedKey := normalizeProjectDir(mcpRes.Location.Directory)
			if resolvedKey == "" {
				resolvedKey = normalizeProjectDir(d.dir)
			}
			if resolvedKey == "" {
				resolvedKey = d.w.defaultServerDir
			}
			if resolvedKey != "" {
				d.w.mcpByDir[resolvedKey] = mcpRes.Servers
				d.w.mcpLoadedDirs[resolvedKey] = true
			}
			d.w.refreshMcpStatusChip()

			if d.mgr != nil && !d.mgr.isDisposed() && d.w.mcpMgr == d.mgr && normalizeProjectDir(d.mgr.dir) == resolvedKey {
				if mcpRes.Location.Directory != "" {
					d.mgr.setResolvedDir(mcpRes.Location.Directory)
				}
				if resCat != nil {
					d.mgr.resources = resCat.Catalog
				}
				d.mgr.applyServers(mcpRes.Servers, "")
			}
		}

		if connErr != nil {
			d.showError("Connection Failed", opencode.HumanizeAPIError(connErr), "shield-symbolic", true)
			return
		}

		var srvStatus opencode.McpStatus
		var hasSrv bool
		if mcpRes != nil {
			if srv, ok := findServerInSlice(mcpRes.Servers, d.serverName); ok {
				srvStatus = srv.Status
				hasSrv = true
			}
		}
		if hasSrv && srvStatus.Status == "needs_auth" {
			msg := strings.TrimSpace(srvStatus.Error)
			if msg == "" {
				msg = "Server still reports that authentication is required."
			}
			d.showError("Authentication Incomplete", msg, "shield-symbolic", false)
			return
		}
		if hasSrv && srvStatus.Status == "failed" {
			msg := strings.TrimSpace(srvStatus.Error)
			if msg == "" {
				msg = "Sign-in succeeded, but the MCP server failed to start."
			}
			d.showError("Connection Failed", msg, "shield-symbolic", true)
			return
		}

		d.syncDone = true
		toastMsg := d.serverName + " connected"
		if hasSrv && srvStatus.Status == "pending" {
			toastMsg = d.serverName + " signed in; connecting…"
		}
		d.close()
		if d.mgr != nil && d.mgr.isOpen() && d.w != nil && d.w.mcpMgr == d.mgr {
			d.mgr.showToast(toastMsg)
		}
	}
}

func (d *mcpOAuthDialog) showError(title, detail, iconName string, retrySyncOnly bool) {
	if d.closed {
		return
	}
	d.retrySyncOnly = retrySyncOnly
	d.errorPage.SetTitle(title)
	if iconName != "" {
		d.errorPage.SetIconName(iconName)
	}
	d.errDetailLabel.SetText(strings.TrimSpace(detail))
	d.errDetailLabel.SetVisible(strings.TrimSpace(detail) != "")
	if retrySyncOnly {
		d.retryBtn.SetTooltipText("Refresh MCP status and reconnect")
	} else {
		d.retryBtn.SetTooltipText("Start a new sign-in attempt")
	}
	d.viewStack.SetVisibleChildName("error")
}

func (d *mcpOAuthDialog) cancelServerAttemptAsync(attemptID string) {
	if attemptID == "" || d.w == nil || d.w.client == nil {
		return
	}
	client := d.w.client
	integrationID := d.integrationID
	dir := d.dir
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), mcpOAuthCancelTimeout)
		defer cancel()
		_ = client.CancelIntegrationOAuth(ctx, integrationID, attemptID, dir)
	}()
}

func (d *mcpOAuthDialog) close() {
	if d == nil {
		return
	}
	d.onDialogClosed()
	if d.dlg != nil {
		d.dlg.ForceClose()
	}
}

func (d *mcpOAuthDialog) onDialogClosed() {
	if d == nil || d.closed {
		return
	}
	d.closed = true
	d.closedFlag.Store(true)
	d.gen++
	d.pollGen++
	d.syncGen++
	d.cancelStartInFlight()
	d.clearAllTimers()

	if d.mgr != nil && d.mgr.authDlg == d {
		d.mgr.authDlg = nil
	}

	if d.attemptID != "" && !d.authCompleted && !d.pasteAccepted {
		attemptID := d.attemptID
		d.attemptID = ""
		d.cancelServerAttemptAsync(attemptID)
	} else if d.pasteAccepted && !d.syncDone && d.w != nil && d.w.ctx != nil && d.w.ctx.Err() == nil {
		d.w.refreshMcpForDir(d.dir, true)
	}

	if d.mgr != nil && d.w != nil && d.w.ctx != nil && d.w.ctx.Err() == nil && d.mgr.isOpen() {
		d.mgr.focusDetailPage()
	}
}

func isOAuthAttemptNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(err.Error()), "http 404:")
}
