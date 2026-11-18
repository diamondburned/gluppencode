# Gluppencode

A GTK4 + Libadwaita + GtkSourceView5 desktop frontend for OpenCode V2 written in Go with `gotk4`.

## Requirements

The following requirements serve as the design source of truth for Gluppencode:

1. **Sidebar, Global Tabs & Window Lifecycle**:
   - Global root-session tabs across all projects within the same window using [`AdwViewStack`](https://gnome.pages.gitlab.gnome.org/libadwaita/doc/1-latest/class.ViewStack.html) inside an [`AdwOverlaySplitView`](https://gnome.pages.gitlab.gnome.org/libadwaita/doc/1-latest/class.OverlaySplitView.html).
   - **Resizable Project-Grouped Sidebar**:
     - Custom `GtkListBox` (`sidebarList`) in the split-view sidebar, drag-resizable on desktop (`sidebarResizeHandle`, clamped to `180–600px`, persisted as `sidebarWidth`), pinned side-by-side when wide (no hide button), and collapsed into an overlay with headerbar toggle buttons via a dynamic `AdwBreakpoint` (`max(600px, sidebarWidth*2)`) on narrow screens.
     - Sessions are grouped under project section headers (collapse chevron, `folder-symbolic`, basename title, shortened path subtitle, collapsed count, hover `+` New Session button, and subtle separator) supporting drag-and-drop project reordering (`projectOrder`), collapse/expand state (`collapsedProjects`), and a project context menu (New Session, Open in Files, Copy Path, Collapse/Expand Project, Archive Project).
     - Conversation rows sit indented beneath their project header with hover close/archive actions.
   - **Per-Project Conversation Limits**:
     - Configurable max visible conversations per project (default `5`, range `1–50` via Preferences `MaxVisibleSessions`), sorted newest-first.
     - Projects exceeding the limit render a clickable `"Show N more…"` footer row to expand all sessions; the currently active session remains visible even if outside the top N.
   - **Dynamic Tab Status & Attention Indicators**:
     - Right side of each conversation row displays an `AdwSpinner` while streaming or executing tools, an attention/pending-input indicator when unfocused tabs finish or await user input (`needs-attention`), or a compact relative timemark (`"now"`, `"2m"`, `"1h"`, `"3d"`, `"2w"`) when idle and unfocused.
     - Window title reflects the active session and global unread attention count; desktop notifications (`gio.Notification`) support click-to-focus (`app.open-session`) and automatic withdrawal when the window/tab becomes active.
   - **Inactive Tab Suspension**:
     - Idle, unvisited root tabs (`tabSuspendAfter = 5m`) are skipped by reconnect resyncs and background directory sweeps so the server does not wake inactive workspaces unnecessarily; visiting the tab or receiving a live SSE event wakes it immediately.
   - **Offline Screen**:
     - Connection failures transition `windowStack` to an `AdwStatusPage` offline view with error details and a "Retry Connection" action.

2. **Fuzzy Session & Project Search (`Ctrl+K`)**:
   - Sidebar header includes a search button (`searchSessionsBtn`) on the left and main menu (`appMenuSidebarBtn`: Preferences, MCP Servers, About) on the right, with `Ctrl+K` bound globally for search.
   - Opens a modal dialog with a `GtkSearchEntry` querying recent sessions (`ListSessions`) plus live server session search (`SearchSessions`), known project directories, and matching filesystem paths (`matchFilesystemDirs`).
   - Uses token-based, substring, and subsequence fuzzy matching (`fuzzyMatchScore`) with bonuses for word boundaries, consecutive characters, and active status.
   - Activating a session switches to it immediately (unarchiving/revealing it in the sidebar if needed); activating a directory starts a new session there.

3. **Material Icons & Embedded Assets**:
   - Bundle Material symbolic action SVGs, app icons (`so.libdb.gluppencode`), and assets via `glib-compile-resources` into an embedded `GResource` (`internal/icons`), exposed on the display icon theme path `/so/libdb/gluppencode/icons`.

4. **Antigravity-Style Composer, Status Bar & Bottom Sheet**:
   - **Floating Composer Card (`.glup-composer-frame`)**:
     - Floats over the bottom of the timeline inside a `GtkOverlay`, paired with a dynamic bottom fade ramp (`timelineBottomFade`) and bottom scroll inset (`applyComposerInset`).
     - Attachment tray (`AdwWrapBox`) for pending image attachments (file chooser or `Ctrl+V` clipboard paste).
     - `GtkSourceView` (`inputView`) with placeholder (`Ask anything, @ to mention, / for actions`), live client-side Markdown span highlighting, multiline expand/collapse toggle (`composerExpandBtn`), Up/Down input history, and `GtkSourceCompletion` popups for `/` commands (built-in + server) and `@` mentions (skills, agents, workspace files).
     - Bottom controls row: `+` attach button, flat `^ <Agent>` and searchable `^ <Model>` popovers (positioned above the composer with recent/filtered models), expand button, and circular Send / Stop action buttons.
     - Supports configurable `Enter` vs `Ctrl+Enter` send mode (with `Shift+Enter` always inserting newlines and `Alt+Enter` queueing in expanded multiline mode), immediate **Steer** (`Ctrl+Enter`) vs **Queue** (`Enter`) delivery (`opencode.InboxDelivery`), drag-and-drop queue reordering, and built-in slash commands (`/undo`, `/redo`, `/new`, `/cd`, `/compact`, `/mcp` / `/mcps`).
   - **Persistent Bottom Status Bar (`statusBarBox`)**:
     - Left chips (`.glup-status-chip`): project directory (`folder-symbolic`), cached Git branch (`git-branch-symbolic`, resolved locally from `.git/HEAD` or worktree pointers), and MCP summary (`bolt-symbolic`).
     - Right chips: double-`Esc` stop confirmation chip (`Press Esc again to stop`, pulsing `stopBtn` while armed), `Ctrl+B to background` action chip (appears after 5s of blocking foreground shell/subagent execution), active background shells (`terminal-symbolic` + `N shells`), active subagents (`arrow-downward-symbolic` + `N subagents`), live tokens-per-second rate, and context usage (`xxxK`).
   - **Bottom Sheet (`AdwBottomSheet` + `AdwNavigationView`)**:
     - Clicking the status bar (or pressing `Ctrl+Shift+M` for MCP) opens the bottom sheet containing:
       - **Active Shells**: queries `/api/shell` and tracks `shell.*` SSE events, showing command, PID, live tail output, and Stop action.
       - **Active Subagents**: lists currently running subagents (clicking navigates into the subagent timeline) and provides "Back to Main Conversation" when viewing a subagent.
       - **MCP Servers (`mcpManager`)**: per-directory MCP server status/config, search filter, connect/disconnect/refresh, navigation detail page (`AdwNavigationPage`) with resources/templates, live log tailing (`McpLogTailer`), and integration OAuth dialog (`mcpOAuthDialog`).
       - **Session & Context**: copyable conversation ID and token/cost/model metadata grid.
   - **Headerbar Subagent Breadcrumbs & Take Charge**:
     - Viewing a subagent switches `headerTitleStack` to `Main Conv > Subagent Conv` breadcrumbs while the sidebar keeps the parent root session highlighted.
     - Headerbar includes a `Take Charge` toggle (`takeChargeBtn`) that autonomously answers incoming `FormInfo` questions using a configurable directive and auto-approves `PermissionRequest` prompts (`Allow Once`) while away, plus a `New Window` button (`Ctrl+Shift+N`), `Refresh`, and session rename popover.

5. **GtkBuilder `.ui` Files & Styling**:
   - Define widget hierarchies declaratively in embedded `.ui` XML files (`window.ui`, `message.ui`, `tool.ui`) and application CSS in `style.css`.

6. **Minimal Forum-Style Linear Flow & Tight Density**:
   - Single linear, high-density conversation flow inside `adw.Clamp` (configurable `ChatWidth`, default `920px`).
   - System fonts across UI labels, reserving monospace strictly for tool bodies, diffs, and code blocks.
   - **Empty Greeter (`GreeterWidget`)**: personalized greeting card (`GreeterProfile` cached in state and refreshed asynchronously) with clickable starter prompt chips when a session has no messages.
   - **User & Queued Prompt Cards (`.glup-user-card`)**: distinct cards without author labels, featuring corner Undo/Revert (`edit-undo-symbolic`) on delivered messages, Steer and Cancel controls on queued prompts, and drag-to-reorder support.
   - **Assistant Turns & Tools**: inline reasoning blockquotes (`.glup-reasoning-inline`), streaming Markdown blocks, and tightly stacked `ToolWidget`s with title/subtitle/badges (`+N -M`, `lines X–Y`), inline diffs (truncated to `DiffTruncationLines`, defaulting to 8 lines with an inline `"show more"` / `"show less"` link), lazy `read` bodies, live scrollable shell output, and "View Session →" buttons for subagents.
   - **Timeline Controls & Notices**: `RevertBannerWidget` (reverted count, rolled-back file diffs, and Redo button for staged `/undo`), compaction dividers with inline summaries, synthetic subagent/shell completion rows (`SyntheticSubagentWidget`), system/agent/model/location notices, and turn duration labels.
   - **Inline Interactive Prompts**: multi-step `QuestionFormWidget` for server `FormInfo` requests and `PermissionPromptWidget` (`Deny`, `Allow Once`, `Always Allow`).
   - **History Pagination & Reconciliation**: lazy scroll-up history pagination (`ListMessagesPage`, 50 messages/page with `autoscroll.Window.LockScroll()`), live tail reconciliation (`reconcileTail`), and SSE reconnect resync (`resyncOnReconnect`).

7. **Inline Images & Lightbox Image Viewer**:
   - User-attached images and tool output images render as clickable `64×64` square thumbnails (`.glup-image-thumb`).
   - Clicking any thumbnail opens the modal Lightbox Image Viewer (`showImageViewer`) with 1:1 / Fit-to-window toggle, "Copy Image to Clipboard", and Escape-to-close.

8. **Multi-Window Support, State & Preferences**:
   - Spawning multiple instances or pressing `Ctrl+Shift+N` opens independent top-level windows, each with its own cancellable context terminating background tickers and SSE loops on close.
   - Multi-window `notificationCoordinator` (`notifications.go`) syncs preference changes and desktop notifications across open windows.
   - Atomic JSON persistence (`state.go`) stores visible/archived sessions, collapsed/ordered projects, sidebar width, input history, greeter profile, and user preferences (`$GLUPPENCODE_STATE_FILE` or `$XDG_DATA_HOME/opencode/gluppencode.json`), alongside per-session draft directories (`gluppencode/drafts/<sessionID>/` in `draft.go`).
   - `adw.PreferencesDialog` (`preferences.go`) configures chat width, max visible sessions, color scheme, `GtkSourceView` / Chroma syntax theme, shell tail lines, diff truncation lines, `Ctrl+Enter` send mode, Take Charge directive, greeter overrides, notifications, and server URL override.

9. **Prefix-Cached & Batched Streaming Markdown (`internal/mdstream`)**:
   - Avoid re-parsing and re-rendering the entire message on every streamed token.
   - Coalesce incoming token deltas per conversation (~35ms `Streamer` tick), parse uncommitted suffix blocks with `goldmark` off the GTK main thread, and incrementally append frozen `Committed` block widgets while replacing only the active `Tail` block widget on the GTK thread.
   - Supports Chroma-highlighted `CodeBlock`s, `TableGrid`s, `ListBlock`s, `InlineBlock` link icons, and async `ses_...` session ID title resolution with clickable links.

10. **Development & E2E Harness (`.draft/`)**:
    - Headless UI testing and inspection harness using `opencode serve` + `gtk4-broadwayd` + `gluppencode` in `.draft/e2e-harness.sh` (and `.draft/broadway-run.sh`), plus Chrome DevTools recipes in `.draft/chrome-recipes/`.
