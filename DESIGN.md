# Gluppencode

A GTK4 + Libadwaita frontend for OpenCode V2 written in Go with `gotk4`.

## Requirements

The following requirements serve as the design source of truth for Gluppencode:

1. **Global Tabs via `AdwViewStack`**:
   - Global session tabs across all projects within the same window using [`AdwViewStack`](https://gnome.pages.gitlab.gnome.org/libadwaita/doc/1-latest/class.ViewStack.html).
   - Users can choose between:
     - **Vertical tabs**: [`AdwViewSwitcherSidebar`](https://gnome.pages.gitlab.gnome.org/libadwaita/doc/1-latest/class.ViewSwitcherSidebar.html), grouping sessions into project sections (`starts-section` and `section-title`). Project headers display a `folder-symbolic` icon to the left of the project name (`filepath.Base(dir)`) in a horizontal box (`spacing: 8`) with a tooltip indicating the shortened path, separated by subtle horizontal dividers. Conversation rows underneath are subtly indented (`margin-left: 14px`). Always chosen over horizontal tabs on mobile/narrow widths; on desktop, can be collapsed/hidden via the top-left button in the sidebar/headerbar.
     - **Per-Project Conversation Limits**: Max 5 conversations displayed per project section by default (sorted newest-first). If a project contains more than 5 conversations, a clickable `"Show N more…"` footer button expands all conversations for that project. The currently viewed conversation is always displayed even if it sits beyond the top 5 while collapsed.
     - **Horizontal tabs**: [`AdwViewSwitcher`](https://gnome.pages.gitlab.gnome.org/libadwaita/doc/1-latest/class.ViewSwitcher.html) in the header bar.
   - Tab pages do not use tab icons (`icon-name` unset).
   - Right side of each view switcher item contains a dynamic status indicator:
     - An active working spinner (`AdwSpinner`) while a session is actively streaming or executing tools.
     - A compact relative timemark (e.g. `"now"`, `"2m"`, `"1h"`, `"3d"`, `"2w"`) indicating last updated time when the session is idle and unfocused (hidden when the tab is currently focused).
     - Numeric badges are kept cleared (`SetBadgeNumber(0)`) to avoid duplicate badge clutter.
   - `needs-attention` indicators highlight unfocused tabs when background activity occurs.

2. **Fuzzy Session Search (`Ctrl+K`)**:
   - Sidebar header includes a dedicated search button (`searchSessionsBtn`) and global `Ctrl+K` shortcut.
   - Opens a modal dialog with a `GtkSearchEntry` and scrollable `boxed-list` querying across up to 200 sessions.
   - Token-based and subsequence fuzzy matching with scoring bonuses for word boundaries and recent activity.
   - Activating a search result immediately switches to the session and ensures it is displayed in the sidebar.

3. **Material Icons**:
   - Bundle Material symbolic SVG icons via `glib-compile-resources` into an embedded `GResource` (`internal/icons`), following Dissent's pattern.

4. **Antigravity-Style 2-Row Composer, Status Bar & Active Background Shells**:
   - 2-row composer card (`glup-composer-frame`) and persistent native Libadwaita bottom bar (`statusBarBox`):
     - **Composer frame**:
       - Top row: `GtkSourceView` text view with subtle placeholder (`Ask anything, @ to mention, / for actions`) that hides on text entry.
       - Bottom controls row: `+` attach button (`list-add-symbolic`), flat `^ <Agent>` and `^ <Model>` menu buttons opening dedicated `GtkPopover`s (`agentPopover` and `modelPopover`, positioned above the composer with search filtering and top-6 recent models), and 32x32 circular Send / Stop action buttons.
     - **Bottom status bar**:
       - Left side: Separate `.glup-status-chip` pills for the project directory (`folder-symbolic` + `<project>`) and cached Git branch (`git-branch-symbolic` + `<branch>`, visible when inside a Git repository), resolving from `.git/HEAD` or worktree pointer files.
       - Right side: Active background shells chip (`terminal-symbolic` + `N shells`, green chip in system UI font, shown only when running shells > 0), active subagents counter (`arrow-downward-symbolic` + `N subagents`), live tokens-per-second status chip, and context usage (`xxxK`).
   - Clicking the bottom bar opens [`AdwBottomSheet`](https://gnome.pages.gitlab.gnome.org/libadwaita/doc/1-latest/class.BottomSheet.html) strictly for bottom-bar runtime details:
     - Drag handle sits cleanly at the top of the sheet without headerbar label collision.
     - **Active background shells list**: queries `/api/shell` and listens to `shell.created`, `shell.exited`, `shell.deleted` SSE events, showing command, PID, and a Stop button (`DELETE /api/shell/{id}`).
     - **Active subagents list**: displays only currently active/running subagents (succeeded or failed subagents are filtered out). When viewing a subagent, the section remains accessible to provide a "Back to Main Conversation" button.
   - When viewing a subagent conversation:
     - Top headerbar shows breadcrumb navigation (`Main Conv > Subagent Conv`), where clicking `Main Conv` navigates back to the parent conversation.
     - Sidebar continues to highlight the main conversation.

5. **GtkBuilder `.ui` Files**:
   - Define widget hierarchies declaratively in embedded `.ui` XML files (`//go:embed *.ui` + `gtk.NewBuilderFromString`), matching Dissent's convention.

6. **Minimal Forum-Style Linear Flow & Tight Density**:
   - Display the conversation as a single linear, high-density flow.
   - System fonts used everywhere across UI labels, reserving monospace strictly for tool bodies, diffs, and code blocks.
   - User messages rendered inside distinct card boxes (`.glup-user-card`) without author labels, featuring a subtle corner Undo button (`edit-undo-symbolic`, `.glup-user-undo-btn`, `opacity: 0.5` default, `1.0` on hover) in the bottom-right corner overlay with right text margin to prevent text overlap.
   - Assistant turns rendered cleanly inline without author/model headers or divider borders.
   - Consecutive tool calls stack tightly with 1px vertical spacing.
   - Thinking blocks displayed as subtle left blockquotes (`.glup-reasoning-inline`) without `"thinking"` headers.
   - Prettier OpenCode Web UI-inspired tool call formatting:
     - Clear titles, subtitles, and badges (`+N -M` diff lines, `lines X–Y` for reads, code execution subtitles).
     - Inline unified diffs and output files without duplicate raw JSON dumps.
     - Direct "View Session →" action button for subagents and continuation calls.
   - Compaction dividers (`──── Compaction · X in · Y out ────`) with inline markdown summaries.
   - Synthetic completion rows:
     - Subagents: `↳ <Agent> finished · <Description>` with a "View Session →" button and expandable response text.
     - Shells: `↳ Shell finished · <Command>` with a `terminal-symbolic` icon and expandable command output.
   - Agent-switched timeline notices.
   - Lazy scroll-up history pagination (`ListMessagesPage`, 50 messages/page, scroll locking at top) and automatic tail reconciliation on live SSE execution events.

7. **Inline Images & Lightbox Image Viewer**:
   - User-attached images and tool output files render as clickable inline `84×84` square thumbnails (`.glup-image-thumb`) stacked horizontally side-by-side.
   - Clicking any thumbnail opens the modal Lightbox Image Viewer window featuring fit-to-window or 1:1 view toggle, "Copy Image to Clipboard", and Escape-to-close.

8. **Multi-Window Support**:
   - Running `./gluppencode` multiple times or clicking the "New Window" button in the headerbar (or pressing `Ctrl+Shift+N`) spawns an independent top-level window.
   - Each window maintains its own cancellable context, cleanly terminating background tickers and SSE event listeners upon window close without affecting other windows.

9. **Prefix-Cached & Batched Streaming Markdown**:
   - Avoid re-parsing and re-rendering the entire message on every streamed token.
   - Batch incoming token deltas per conversation in a background goroutine (~30–40ms tick), parse off the GTK main thread, and incrementally commit stable block prefixes while only updating the active tail block on the GTK thread.

10. **Development Harness (`.draft/`)**:
    - Headless UI testing and inspection harness using `gtk4-broadwayd` + Chrome DevTools MCP in `.draft/`.
