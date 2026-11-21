# AGENTS.md

Quick-reference guide for coding agents working on **gluppencode** (`libdb.so/gluppencode`), a GTK4 + Libadwaita + GtkSourceView5 desktop frontend for OpenCode V2 written in Go (`gotk4`).

See [`DESIGN.md`](DESIGN.md) for UI/UX design requirements.

---

## Build, Test, & Dev Environment

`gotk4` requires CGO and GTK4 / Libadwaita / GtkSourceView5 / PCRE2 C headers provided by the Nix flake `devShell` (`flake.nix`, `nix/base.nix`). Only pure-Go `internal/opencode` builds outside `nix develop`. There is no `Makefile`, `Justfile`, or `.golangci.yml`.

```sh
# Run all tests (requires GTK4 dev headers from Nix shell)
nix develop -c go test ./...

# Run pure-Go API client tests only (works without GTK headers)
go test ./internal/opencode/...

# Build binary / Nix package
nix develop -c go build .
nix build

# Vet
nix develop -c go vet ./...

# Regenerate embedded GResource bundle after changing internal/icons/ SVGs or XML
nix develop -c go generate ./internal/icons

# Update nix/gomod2nix.toml after go.mod changes
nix develop -c gomod2nix

# Run app
nix run        # or ./gluppencode after go build
```

### Headless E2E Testing (`.draft/`)
`.draft/` is gitignored and holds scratch notes plus the headless Broadway harness:
- `./.draft/e2e-harness.sh [DISPLAY_NUM] [OPENCODE_PORT]` (wrapped by `./.draft/broadway-run.sh`): builds `gluppencode` with an isolated test `appID` and launches standalone `opencode serve`, `gtk4-broadwayd`, and `gluppencode` for browser/DevTools inspection without touching host `service.json`.
- `.draft/chrome-recipes/`: Chrome DevTools E2E recipes (e.g., `mcp-manager-e2e.md`).

---

## Repository & Package Map

### Root & Packaging
- `main.go`: Entrypoint (`appID = "so.libdb.gluppencode"`). Calls `adw.Init()`, `gtksource.Init()`, `app.New`, `window.RegisterAppActions`, `opencode.Discover`, and `window.NewWindow`.
- `flake.nix`, `nix/base.nix`, `nix/gomod2nix.toml`, `nix/so.libdb.gluppencode.{desktop,service}`: Nix Flake (`gotk4-nix`, `gomod2nix`) and desktop/D-Bus service files.
- `so.libdb.gluppencode.yml`, `so.libdb.gluppencode.metainfo.xml`: Flatpak manifest and AppStream metadata.

### `internal/icons/`
- `icons.go`, `gluppencode.gresource.xml`: `//go:generate glib-compile-resources gluppencode.gresource.xml` and `//go:embed gluppencode.gresource`. Packs app icons, Material symbolic action SVGs, and `gluppencode-eyes.png` under `/so/libdb/gluppencode/icons/scalable/actions/` (resolved via `IconTheme.AddResourcePath("/so/libdb/gluppencode/icons")`).

### `internal/autoscroll/`
- `autoscroll.go`: `Window` (embeds `*gtk.ScrolledWindow`) managing bottom-pinning, `LockScroll()` (preserves viewport position when prepending older history pages), and `ScrollToBottom()` / `ScrollToBottomIfBottomed()`.

### `internal/mdstream/`
Incremental prefix-caching Markdown parser and GTK4 renderer:
- `stream.go`:
  - `Engine`: parses uncommitted Markdown suffix via `goldmark` (GFM), freezing completed prefix blocks into `RenderPatch.Committed` while keeping the active/unclosed trailing block(s) in `RenderPatch.Tail`.
  - `Streamer` (`Batcher` alias): coalesces incoming token deltas on a background goroutine (`DefaultInterval = 35ms`).
- `widget.go`: `View` (`*gtk.Box`) with `SetStatic`, `AppendDelta`, `Finalize`, `Close`; renders `CodeBlock` (Chroma syntax highlighting via `chatkit/md/hl` + copy button), `ListBlock` / `ListItemBlock`, `InlineBlock` (website links with inline `earth-symbolic` icons), and `TableGrid`; supports `WithAsyncSessionResolver` (`ses_...` IDs → titles) and `WithLinkHandler`.

### `internal/opencode/`
OpenCode V2 HTTP + SSE client and local MCP helpers (pure Go, no GTK dependency):
- `client.go`:
  - `Discover` / `Rediscover` / `SetServerURL`: resolves URL from `OPENCODE_SERVER_URL`, `$XDG_STATE_HOME/opencode/service.json`, `opencode service status`, or `http://127.0.0.1:4096` (password from `OPENCODE_SERVER_PASSWORD` or state/config `service.json`).
  - `do` / `doJSON`: retries once on network error or `401` **only if `Rediscover` finds a changed URL or password**.
  - `Event`, `SubscribeEvents`, `SubscribeEventsLoop`: `/api/event` SSE reader with fixed reconnect delay and `onReconnect` hook.
  - Endpoints for sessions (`ListSessions`, `SearchSessions`, `ListChildSessions`, `MoveSession`, `StageRevert`/`ClearRevert`/`CommitRevert`, `CompactSession`, `Interrupt`, `BackgroundSession`), paged messages (`ListMessagesPage`), inbox (`SendPrompt`, `ListInbox`, `UpdateInboxDelivery`, `CancelInbox`), commands, skills, agents, models, `GenerateText`, `FindFiles`, shells, subagent jobs (`ListJobs`), forms, and permissions.
- `events.go`: Typed SSE payload structs (`SessionCreatedEvent` and `PermissionAskedEvent` implement custom `UnmarshalJSON` for multi-shape payloads).
- `diff.go`: `SessionDiff`, `ExtractToolModification`, `CountDiffChanges`, `writeMinimalLineDiff`.
- `mcp.go`: `ListMCP`, `ListMCPResources`, `ListConfig`, `ConnectMCP`, `DisconnectMCP`, `SanitizeMcpURL`, `MergeMcpConfigs`, `HumanizeAPIError`. `ConfigEntry.UnmarshalJSON` strips OAuth secrets, discards env/header values (retaining only `EnvKeys` and `HeaderKeys`), and sanitizes URLs.
- `mcplog.go`: `McpLogTailer` for tailing local OpenCode logs (`DefaultLogDir`, `ResolveLogFile`, `ReadInitialTail`, `PollNewLines` with `.trim` lock and in-place compaction handling, `ParseLogLine`).
- `integration.go`: Integration & MCP OAuth helpers (`GetIntegration`, `StartIntegrationOAuth`, `GetIntegrationOAuthStatus`, `CompleteIntegrationOAuth`, `CancelIntegrationOAuth`, `SelectOAuthMethod`, `ValidateExternalAuthURL`).

### `internal/window/`
GTK4 / Libadwaita UI, state, and event orchestration:
- `window.ui`, `message.ui`, `tool.ui`, `style.css`: Declarative GtkBuilder widget templates and application CSS.
- `window.go`: Main `Window` struct, tab/subagent lifecycle, sidebar, composer dispatch, message reconciliation, status bar/sheet, and SSE dispatch (see entrypoint map below).
- `message.go`: Timeline widgets — `UserMessageWidget` (undo/steer/cancel + drag-reorder), `AssistantMessageWidget`, `ToolWidget` (diffs, lazy expandable tool details, live shell tail, subagent jump), `CompactionWidget`, `SyntheticSubagentWidget` (subagent and shell completion rows), `SystemNoticeWidget`, `AgentSwitchedWidget` (also used by `NewModelSwitchedWidget` and `NewLocationSwitchedWidget`), `RevertBannerWidget`, `QuestionFormWidget`, `PermissionPromptWidget`.
- `composer.go`: Multiline expand/collapse, `composerEnterAction` (Enter / Shift+Enter / Alt+Enter / Ctrl+Enter), client-side Markdown span tagging (`scanComposerMarkdownSpans`).
- `completion.go`: `CompletionProvider` & `CompletionProposal` (`coreglib.RegisterSubclass` for `GtkSourceCompletion`), `/` commands (`builtinSlashCommands` + server commands), `@` skills/agents/workspace files, `parseMentions`.
- `mcp.go`, `mcp_oauth.go`: `mcpManager`, `mcpDetailView` (resources, config, 500ms `McpLogTailer` polling), `Ctrl+Shift+M` shortcut, and `mcpOAuthDialog` (`adw.Dialog`).
- `state.go`: Atomic JSON persistence (`$GLUPPENCODE_STATE_FILE` or `$XDG_DATA_HOME/opencode/gluppencode.json`) for `visibleSessions`, `archivedSessions`, `collapsedProjects`, `projectOrder`, `inputHistory`, `sidebarWidth`, `lastSelectedAgent`, `lastSelectedModel`, `GreeterProfile`, and `Preferences`.
- `draft.go`: Per-session composer draft + binary attachment persistence (`.../gluppencode/drafts/<sessionID>/`) guarded by `draftWriteGen` and `draftWriteMu`.
- `preferences.go`: `adw.PreferencesDialog` and live `applyPreferenceDelta` sync across windows.
- `notifications.go`: Per-`gtk.Application` `notificationCoordinator` (`RegisterAppActions`, `app.open-session`, focus withdrawal, cross-window preference sync).
- `suspend.go`: Idle tab suspension (`tabSuspendAfter`, `isTabSuspended`, `awakeTabDirs`).
- `greeter.go`, `fuzzy.go`, `image.go`: `GreeterWidget` + async `GreeterProfile` generation, `fuzzyMatchScore`, image texture decoding + `newImageThumbnail` + `showImageViewer`.
- `*_test.go`: Unit and GTK widget reconciliation tests covering timeline/SSE reconciliation, activity phases, sidebar, MCP UI, drafts, composer/completion, notifications, tab suspension, and fuzzy matching.

---

## Navigating `internal/window/window.go`

`window.go` is large; jump by symbol (`grep` / `ag`) rather than scanning sequentially:

- **Core structs**: `Window`, `sessionTab`, `convTimeline`, `pendingPrompt`, `switcherIndicator`.
- **Window init & chrome**: `NewWindow`, `openNewWindow`, `initOfflineScreen`, `setupComposerFade`, `syncNavigationChrome`, `setupSidebarResize`, `syncSidebarBreakpoint`.
- **Sidebar & session tabs**: `setupSidebarList`, `syncSidebar`, `buildConversationRow`, `updateSwitcherIndicators`, `newProjectHeaderBox`, `ensureSessionTab`, `selectSession`, `archiveSession`, `clearProjectSessions`, `openSessionSearchDialog` (`Ctrl+K`).
- **Initial load & turn settle**: `loadAll`, `populateSessionTabs`, `loadPendingFormsForAllDirs`, `maybeScheduleRootTurnCompletion`, `verifyAndFinalizeRootTurn`.
- **Take Charge & interactive prompts**: `TakeChargeDirective`, `setTakeCharge`, `replyFormTakeCharge`, `replyPermissionTakeCharge`, `upsertPendingForm`, `upsertPendingPermission`.
- **Subagents & history pagination**: `onVisibleTabChanged`, `openSubagent`, `closeSubagent`, `loadConvMessages`, `loadOlderConvMessages`, `reconcileTail`, `reconcileConvMessages`.
- **Status bar & bottom sheet**: `updateBottomBar`, `resolveGitBranch`, `refreshShells`, `pollActiveShellOutputs`, `openBottomSheet`, `rebuildSubagentList`, `rebuildContextSection`.
- **Agents, models, & prompt dispatch**: `rebuildAgentPills`, `rebuildModelList`, `switchAgent`, `switchModel`, `sendPromptWithDelivery`, `reorderPendingPrompt`, `syncQueuedOrderOnServer`, `undoLastMessage`, `redoRevert`, `interruptSession`, `backgroundCurrentSession`.
- **SSE loop**: `runEventLoop`, `resyncOnReconnect`, `handleEvent`.

---

## Key Architectural Invariants & Gotchas

1. **GTK Thread Safety**: Background API calls run via `gtkutil.Async` or goroutines; every GTK widget mutation must execute on the GTK main loop (`glib.IdleAdd` or `gtkutil.Async`'s returned UI closure). `mdstream.Streamer` parses Markdown off-thread and schedules `applyPatch` via `glib.IdleAdd`.
2. **Epoch Guards (`activityGen`, `draftWriteGen`)**:
   - `convTimeline.activityGen` increments on live activity transitions so stale async `loadConvMessages` / `reconcileTail` completions do not overwrite newer streaming/activity state.
   - `draftWriteGen` prevents in-flight async draft writes from resurrecting drafts after a send/clear.
3. **Queue Reorder Guard**: Server-side inbox reordering (`syncQueuedOrderOnServer`) re-admits the queued suffix with `NoResume: true` and cancels originals; `reorderPendingPrompt` rejects concurrent reorders while any prompt has `rewriting == true` to prevent duplicate inbox items.
4. **No Blind POST Replay**: `opencode.Client.do` retries at most once and only when `Rediscover` finds a changed server URL or password, never replaying non-idempotent POSTs against the same endpoint.
5. **Tab Suspension**: `suspend.go` skips background form/permission polling and reconnect resyncs for idle tabs unvisited beyond `tabSuspendAfter` (`5m`), waking immediately on visit or SSE activity.
6. **Secret Stripping**: `opencode.ConfigEntry.UnmarshalJSON` drops MCP OAuth secrets and env/header values on decode and redacts credentials/sensitive query params via `SanitizeMcpURL`.

---

## Keeping Docs in Sync

- Update `AGENTS.md` and [`DESIGN.md`](DESIGN.md) whenever changing any files, packages, symbols, commands, shortcuts, or UI/UX behaviors they describe. Keep entries concise and symbol-anchored — never add fast-drifting details like LOC counts or line-number ranges.
- Before finalizing work on a feature, do one final check against `AGENTS.md` and `DESIGN.md` to verify neither has drifted from the codebase.
