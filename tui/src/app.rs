//! UI state for the TUI.
//!
//! `App` owns the transcript, the input textarea, the tool-call map, any
//! pending permission request, and per-turn bookkeeping. It is driven by
//! `UiEvent`s pushed from the ACP layer and by keyboard input from the
//! ratatui loop. Outgoing actions (send prompt, cancel, answer a permission
//! request) are queued on an `Action` channel that the ACP task consumes.

use std::collections::HashMap;

use agent_client_protocol::schema::v1::{PermissionOptionId, SessionId};
use ratatui::layout::Rect;
use ratatui::style::Style;
use ratatui::text::{Line, Span};
use tui_textarea::TextArea;

use crate::event::{PendingPermission, PlanView, StopView, ToolCallView, UiEvent, UsageView};
use crate::theme;

/// What phase a single turn is in.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum TurnState {
    /// Idle: accepting input.
    Idle,
    /// A prompt is in flight; streaming updates are expected.
    Streaming,
    /// The agent is blocked on a permission request.
    AwaitingPermission,
    /// The turn just ended; banner shown until next input.
    Done,
}

/// One entry in the transcript. Rendering turns each into styled `Line`s;
/// the variant carries the raw payload so styling is computed at draw time
/// (e.g. a tool call's status glyph recolors when pending→completed).
///
/// `Thought` and `Tool` are *foldable*: `collapsed` controls whether they
/// render as a single summary line (`▸ …`) or the full content. Both default
/// to folded when pushed — the user expands one with `Tab` to inspect it,
/// matching Claude Code's "show the headline, hide the body" rhythm.
#[derive(Debug, Clone)]
pub enum TranscriptLine {
    User(String),
    Agent(String),
    /// Dimmed reasoning/thought text.
    Thought { text: String, collapsed: bool },
    /// A tool call. The `view` is the snapshot at creation; the latest status
    /// is re-looked-up from `tool_calls` at render time so the glyph recolors
    /// live (pending→completed) without mutating the transcript entry.
    Tool { view: ToolCallView, collapsed: bool },
    /// A full-width separator with a status message.
    System(String),
}

impl TranscriptLine {
    /// Whether this entry is foldable (has a collapsed/expanded state).
    /// Public so the mouse handler can decide whether a clicked row is
    /// actionable (only Thought/Tool rows toggle).
    pub fn is_foldable(&self) -> bool {
        matches!(self, TranscriptLine::Thought { .. } | TranscriptLine::Tool { .. })
    }

    /// Toggle the `collapsed` flag on a foldable entry (no-op for others).
    pub fn toggle_collapse(&mut self) {
        match self {
            TranscriptLine::Thought { collapsed, .. }
            | TranscriptLine::Tool { collapsed, .. } => *collapsed = !*collapsed,
            _ => {}
        }
    }
}

/// Which pane currently holds keyboard focus. `Tab` toggles the focused
/// collapsible section in `History`; `Shift-Tab` swaps focus between the
/// two panes. The plan sidebar is read-only for now, but carrying the
/// state keeps the focus model explicit (and ready for future interaction).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum Focus {
    #[default]
    History,
    Plan,
}

/// A collapsible section in the right-hand sidebar. Each carries its own
/// folded state; `selected` tracks which section `Tab` targets when the
/// sidebar has focus. Order here is the render order (Session, Context,
/// Plan) — top to bottom.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SidebarSection {
    Session,
    Context,
    Plan,
}

impl SidebarSection {
    /// All sections in render order. Public so the mouse handler can walk
    /// them to map a clicked row to a section.
    pub const ALL: [SidebarSection; 3] =
        [SidebarSection::Session, SidebarSection::Context, SidebarSection::Plan];

    /// The next section below this one (wraps around).
    fn next(self) -> Self {
        match self {
            SidebarSection::Session => SidebarSection::Context,
            SidebarSection::Context => SidebarSection::Plan,
            SidebarSection::Plan => SidebarSection::Session,
        }
    }

    /// The previous section above this one (wraps around).
    fn prev(self) -> Self {
        match self {
            SidebarSection::Session => SidebarSection::Plan,
            SidebarSection::Context => SidebarSection::Session,
            SidebarSection::Plan => SidebarSection::Context,
        }
    }

    /// The display label for the section header.
    fn label(self) -> &'static str {
        match self {
            SidebarSection::Session => "Session",
            SidebarSection::Context => "Context",
            SidebarSection::Plan => "Plan",
        }
    }
}

/// Actions the UI asks the ACP layer to perform.
#[derive(Debug)]
pub enum Action {
    /// Send a user prompt to the active session.
    Prompt { session_id: SessionId, text: String },
    /// Cancel the in-flight turn.
    Cancel { session_id: SessionId },
    /// Answer a pending permission request with a chosen option id, or
    /// `None` to cancel. The id is validated against the offered options
    /// before the response is sent.
    AnswerPermission { option_id: Option<PermissionOptionId> },
    /// Shut the connection down (user quit).
    Quit,
}

pub struct App {
    pub transcript: Vec<TranscriptLine>,
    /// The accumulating assistant text for the current streaming turn,
    /// flushed to the transcript on stop.
    pub agent_buffer: String,
    /// Same, for thought/reasoning chunks.
    pub thought_buffer: String,
    pub tool_calls: HashMap<String, ToolCallView>,
    pub usage: UsageView,
    pub turn: TurnState,
    pub last_stop: Option<StopView>,
    pub session_id: Option<SessionId>,
    pub pending_permission: Option<PendingPermission>,
    /// Selected option index in the permission modal. Moved by Up/Down and
    /// mouse clicks; Enter (or a click on an option) confirms it. Reset to 0
    /// whenever a new `RequestPermission` arrives.
    pub permission_cursor: usize,
    /// Cached geometry of the permission modal's option list (from the last
    /// `draw_permission_modal`), used to hit-test mouse clicks. `(x, y, width,
    /// height)` of the list area inside the modal border.
    pub permission_modal_list_rect: Rect,
    pub input: TextArea<'static>,
    /// The user has asked to quit.
    pub quit: bool,
    /// Scroll offset (in lines) from the bottom of the transcript; 0 = tail.
    pub scroll: usize,
    /// Diagnostic shown in the status bar (errors, spawn status, etc.).
    pub notice: String,
    /// Animation frame counter, bumped on every render. Drives the
    /// thinking/streaming spinner so the screen feels alive during a turn.
    pub frame: usize,
    /// The agent's current plan, shown in the right-hand sidebar. Replaced
    /// whole on every `UiEvent::Plan` (ACP sends a full replacement).
    pub plan: Option<PlanView>,
    /// Which pane holds focus — drives `Tab`/`Shift-Tab` behavior.
    pub focus: Focus,
    /// Index into `transcript` of the "selected" row, used to target
    /// `Tab` (toggle collapse) at a specific collapsible section. In idle
    /// mode the user moves it with Up/Down; while streaming it snaps to the
    /// tail so new content is in view. Not a scroll offset — the separate
    /// `scroll` field still controls the viewport.
    pub cursor: usize,
    /// The agent's self-reported label (from `initialize` → `agent_info`),
    /// shown in the sidebar Session header. Falls back to the binary name.
    pub agent_label: String,
    /// Per-sidebar-section folded state. `true` = collapsed to a single
    /// header line. Session/Context default open (always-want-to-see
    /// glance info); Plan defaults open too (its whole point is visibility).
    pub session_folded: bool,
    pub context_folded: bool,
    pub plan_folded: bool,
    /// Which sidebar section is selected (target of `Tab` when the sidebar
    /// has focus). Up/Down move it while `focus == Plan`.
    pub selected_section: SidebarSection,
    // ── Cached render geometry for mouse hit-testing ──
    // Written every frame by `draw()` / `draw_history()`, read by
    // `handle_mouse()` to map a clicked screen row back to a transcript
    // entry or sidebar section. Storing these on `App` avoids recomputing
    // the layout + line wrapping at click time (which would need the same
    // width the renderer used).
    /// The terminal `Rect` from the most recent `draw()`. Used to recompute
    /// pane rects at click time by replaying the same `Layout` constraints.
    pub last_area: Rect,
    /// Maps each rendered history line index → the transcript entry index
    /// it belongs to. Built in `history_lines()` alongside the `Line`s so
    /// mouse clicks on a wrapped body line still resolve to the right entry.
    pub row_to_entry: Vec<usize>,
    /// The `top` slice offset of the currently visible history window
    /// (`lines[top..bottom]`). A click at relative row `r` maps to
    /// `row_to_entry[history_top + r]`.
    pub history_top: usize,
    /// The history pane height (rows) from the last render. Clicks below
    /// this are off-pane.
    pub history_pane_height: usize,
    /// The history pane's left column (x) from the last render, for
    /// hit-testing whether a click is inside history vs. the sidebar.
    pub history_pane_right: u16,
    /// The history pane's top row (y) from the last render.
    pub history_pane_y: u16,
    /// The sidebar pane's top row (y) and left column (x) from the last
    /// render, for hit-testing sidebar clicks.
    pub sidebar_y: u16,
    pub sidebar_x: u16,
}

impl App {
    pub fn new() -> Self {
        let mut input = TextArea::default();
        // Placeholder so the input box isn't an empty void before first use.
        input.set_placeholder_text("Message the agent…  (Enter to send, Esc to cancel, Ctrl-C to quit)");
        Self {
            transcript: Vec::new(),
            agent_buffer: String::new(),
            thought_buffer: String::new(),
            tool_calls: HashMap::new(),
            usage: UsageView::default(),
            turn: TurnState::Idle,
            last_stop: None,
            session_id: None,
            pending_permission: None,
            permission_cursor: 0,
            permission_modal_list_rect: Rect::default(),
            input,
            quit: false,
            scroll: 0,
            notice: String::from("connecting…"),
            frame: 0,
            plan: None,
            focus: Focus::History,
            cursor: 0,
            agent_label: String::new(),
            session_folded: false,
            context_folded: false,
            plan_folded: false,
            selected_section: SidebarSection::Session,
            last_area: Rect::default(),
            row_to_entry: Vec::new(),
            history_top: 0,
            history_pane_height: 0,
            history_pane_right: 0,
            history_pane_y: 0,
            sidebar_y: 0,
            sidebar_x: 0,
        }
    }

    /// Apply a `UiEvent`; return whether the screen needs a redraw.
    pub fn handle_event(&mut self, ev: UiEvent) -> bool {
        match ev {
            UiEvent::AgentSpawned { ok, detail } => {
                self.notice = if ok {
                    format!("backend spawned: {detail}")
                } else {
                    format!("backend failed: {detail}")
                };
                true
            }
            UiEvent::SessionReady { session_id, agent_label } => {
                self.session_id = Some(session_id);
                self.agent_label = agent_label;
                self.notice = "session ready".into();
                self.transcript
                    .push(TranscriptLine::System("— session ready —".into()));
                true
            }
            UiEvent::AgentMessageChunk { text } => {
                self.agent_buffer.push_str(&text);
                self.turn = TurnState::Streaming;
                true
            }
            UiEvent::AgentThoughtChunk { text } => {
                self.thought_buffer.push_str(&text);
                self.turn = TurnState::Streaming;
                true
            }
            UiEvent::ToolCall(view) => {
                // Store the raw view; rendering computes the styled line from
                // the latest entry in `tool_calls`, so status changes
                // (pending→completed) recolor the glyph live. Tool calls
                // start folded — the user expands one with `Tab` to inspect.
                self.transcript
                    .push(TranscriptLine::Tool { view: view.clone(), collapsed: true });
                self.tool_calls.insert(view.id.clone(), view);
                self.snap_cursor_to_tail();
                true
            }
            UiEvent::ToolCallUpdate(view) => {
                // Merge: if we've seen this tool call before, update its
                // fields; otherwise insert as new. We don't append another
                // transcript line for a pure status change — the existing
                // line already represents the call.
                let id = view.id.clone();
                let summary = view.summary.clone().or_else(|| {
                    self.tool_calls.get(&id).and_then(|t| t.summary.clone())
                });
                let merged = ToolCallView {
                    summary,
                    ..view
                };
                self.tool_calls.insert(id, merged);
                true
            }
            UiEvent::Usage(u) => {
                self.usage = u;
                true
            }
            UiEvent::Plan(plan) => {
                // Full replacement (ACP semantics). `None` entries (the
                // backend clears the panel on plan-mode exit) collapses to
                // an empty plan view — the sidebar shows its empty state.
                self.plan = Some(*plan);
                true
            }
            UiEvent::RequestPermission(p) => {
                self.pending_permission = Some(p);
                // New request → selection starts at the first option.
                self.permission_cursor = 0;
                self.turn = TurnState::AwaitingPermission;
                true
            }
            UiEvent::Stopped(stop) => {
                // Flush any buffered assistant/thought text into the transcript.
                // Thoughts start folded (one summary line) to keep the flow
                // readable; the user expands with `Tab` if they want detail.
                if !self.thought_buffer.is_empty() {
                    self.transcript.push(TranscriptLine::Thought {
                        text: std::mem::take(&mut self.thought_buffer),
                        collapsed: true,
                    });
                }
                if !self.agent_buffer.is_empty() {
                    self.transcript
                        .push(TranscriptLine::Agent(std::mem::take(&mut self.agent_buffer)));
                }
                self.last_stop = Some(stop);
                self.turn = TurnState::Done;
                self.notice = format!("turn: {}", stop.label());
                self.snap_cursor_to_tail();
                // If the turn was cancelled while a permission was pending,
                // drop the modal (the responder is answered separately).
                if stop == StopView::Cancelled {
                    self.pending_permission = None;
                }
                true
            }
            UiEvent::ConnectionClosed { detail } => {
                self.notice = format!("connection closed: {detail}");
                self.turn = TurnState::Idle;
                self.transcript
                    .push(TranscriptLine::System(format!("— connection closed: {detail} —")));
                true
            }
            UiEvent::Error(msg) => {
                self.notice = format!("error: {msg}");
                true
            }
        }
    }

    /// Reset scroll to the tail (called on any new content).
    pub fn snap_to_tail(&mut self) {
        self.scroll = 0;
    }

    /// Advance the animation frame. Called by the render loop on every tick
    /// so spinners progress even when no protocol event arrives (the "agent
    /// is thinking" gap before the first token).
    pub fn tick(&mut self) {
        self.frame = self.frame.wrapping_add(1);
    }

    /// The current spinner glyph for an active turn. Braille dots cycling
    /// every ~3 frames (~240ms at 80ms tick) — a calm, Claude-Code-like
    /// cadence rather than a frenetic spin.
    pub fn spinner(&self) -> &'static str {
        const FRAMES: [&str; 10] = ["⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"];
        FRAMES[(self.frame / 3) % FRAMES.len()]
    }

    /// Build the styled lines to render in the history pane, **pre-wrapped to
    /// `width` columns** so that one logical line == one screen row.
    ///
    /// This is critical for mouse hit-testing: because we hard-wrap here
    /// instead of letting ratatui's `Paragraph::wrap` fold at draw time, the
    /// returned `row_to_entry` vector maps every screen row directly to the
    /// transcript entry it belongs to. A click at relative row `r` then
    /// resolves to `row_to_entry[top + r]` with no wrap-induced off-by-N.
    ///
    /// Returns `(lines, row_to_entry)` where `row_to_entry[i]` is the
    /// transcript index that produced line `i`. Streaming-only "tail" lines
    /// (the spinner, live buffers) are mapped to `usize::MAX` since they
    /// don't correspond to a committed transcript entry and aren't
    /// foldable/clickable.
    pub fn history_lines(&self, width: u16) -> (Vec<Line<'static>>, Vec<usize>) {
        let mut lines: Vec<Line<'static>> = Vec::new();
        let mut row_to_entry: Vec<usize> = Vec::new();
        // Helper closures keep the row_to_entry push in sync with every
        // lines.push/extend — easy to miss otherwise.
        let push_line = |lines: &mut Vec<_>, row_map: &mut Vec<_>, idx: usize, line: Line<'static>| {
            lines.push(line);
            row_map.push(idx);
        };
        let push_lines = |lines: &mut Vec<_>,
                          row_map: &mut Vec<_>,
                          idx: usize,
                          new: Vec<Line<'static>>| {
            for l in new {
                lines.push(l);
                row_map.push(idx);
            }
        };

        for (idx, entry) in self.transcript.iter().enumerate() {
            let is_cursor = idx == self.cursor && self.focus == Focus::History;
            match entry {
                TranscriptLine::User(t) => {
                    push_line(&mut lines, &mut row_to_entry, idx, speaker_line("●", "you", theme::USER));
                    push_lines(&mut lines, &mut row_to_entry, idx, body_lines(t, theme::BODY, width));
                    push_line(&mut lines, &mut row_to_entry, idx, blank());
                }
                TranscriptLine::Agent(t) => {
                    push_line(&mut lines, &mut row_to_entry, idx, speaker_line("✻", "agent", theme::AGENT));
                    push_lines(&mut lines, &mut row_to_entry, idx, body_lines(t, theme::BODY, width));
                    push_line(&mut lines, &mut row_to_entry, idx, blank());
                }
                TranscriptLine::Thought { text, collapsed } => {
                    if *collapsed {
                        // One-line folded summary. The marker `▸` (filled
                        // triangle) signals "click to expand" — paired with
                        // `▾` when open. Keep it chrome-quiet so it recedes
                        // behind the agent's actual message above it.
                        let preview = truncate_single(text, 50);
                        let mut spans = vec![
                            Span::raw("  ▸ "),
                            Span::styled("thinking", theme::CHROME),
                        ];
                        if !preview.is_empty() {
                            spans.push(Span::styled(format!("  {preview}"), theme::CHROME));
                        }
                        push_line(&mut lines, &mut row_to_entry, idx, maybe_cursor_line(Line::from(spans), is_cursor));
                    } else {
                        let spans = vec![
                            Span::raw("  ▾ "),
                            Span::styled("thinking", theme::CHROME),
                        ];
                        push_line(&mut lines, &mut row_to_entry, idx, maybe_cursor_line(Line::from(spans), is_cursor));
                        push_lines(&mut lines, &mut row_to_entry, idx, body_lines(text, theme::CHROME, width));
                        push_line(&mut lines, &mut row_to_entry, idx, blank());
                    }
                }
                TranscriptLine::Tool { view, collapsed } => {
                    // Render from the latest known state so the status glyph
                    // recolors live (pending → completed).
                    let latest = self.tool_calls.get(&view.id).unwrap_or(view);
                    if *collapsed {
                        // Folded: the one-line tool row, prefixed with `▸`.
                        let mut line = folded_tool_line(latest);
                        if is_cursor {
                            line = cursor_line(line);
                        }
                        push_line(&mut lines, &mut row_to_entry, idx, line);
                        push_line(&mut lines, &mut row_to_entry, idx, blank());
                    } else {
                        // Expanded: the headline row (with `▾`) + the summary
                        // body below it, if any.
                        let mut head = expanded_tool_head(latest);
                        if is_cursor {
                            head = cursor_line(head);
                        }
                        push_line(&mut lines, &mut row_to_entry, idx, head);
                        if let Some(s) = latest.summary.as_deref().filter(|s| !s.is_empty()) {
                            push_lines(&mut lines, &mut row_to_entry, idx, body_lines(s, theme::CHROME, width));
                        }
                        push_line(&mut lines, &mut row_to_entry, idx, blank());
                    }
                }
                TranscriptLine::System(t) => {
                    push_line(&mut lines, &mut row_to_entry, idx, Line::styled(format!("  {t}"), theme::CHROME));
                    push_line(&mut lines, &mut row_to_entry, idx, blank());
                }
            }
        }
        // Live streaming buffers (not yet flushed): show tokens arriving in
        // real time. When the turn is active but no text has arrived yet
        // (the "thinking" gap), show an animated spinner line so the screen
        // isn't frozen. These tail rows map to usize::MAX (not clickable).
        let streaming = matches!(self.turn, TurnState::Streaming | TurnState::AwaitingPermission);
        if !self.thought_buffer.is_empty() {
            push_line(&mut lines, &mut row_to_entry, usize::MAX, speaker_line("✻", "thinking", theme::CHROME));
            push_lines(&mut lines, &mut row_to_entry, usize::MAX, body_lines(&self.thought_buffer, theme::CHROME, width));
        }
        if !self.agent_buffer.is_empty() {
            push_line(&mut lines, &mut row_to_entry, usize::MAX, speaker_line("✻", "agent", theme::AGENT));
            let mut body = body_lines(&self.agent_buffer, theme::BODY, width);
            // Streaming cursor at the tail so the user sees the turn is live.
            if let Some(last) = body.last_mut() {
                last.spans.push(Span::styled(" ▌", theme::AGENT));
            }
            push_lines(&mut lines, &mut row_to_entry, usize::MAX, body);
        } else if streaming && self.thought_buffer.is_empty() {
            // Nothing has arrived yet — show a spinner so the UI breathes.
            push_line(&mut lines, &mut row_to_entry, usize::MAX, Line::from(vec![
                Span::raw("  "),
                Span::styled(self.spinner(), theme::AGENT),
                Span::styled(" thinking…", theme::CHROME),
            ]));
        }
        (lines, row_to_entry)
    }

    /// Lines for the right-hand sidebar — a stack of collapsible sections:
    /// Session (agent label + session id), Context (token/cost usage), and
    /// Plan (the agent's task list). Each section header is `▾ Label` when
    /// open, `▸ Label` when folded; the selected section's header is
    /// highlighted when the sidebar has focus.
    pub fn sidebar_lines(&self) -> Vec<Line<'static>> {
        let mut lines: Vec<Line<'static>> = Vec::new();
        for section in SidebarSection::ALL {
            let folded = self.section_folded(section);
            let selected = self.focus == Focus::Plan && self.selected_section == section;
            // Header line.
            let marker = if folded { "▸" } else { "▾" };
            let header_spans = match section {
                SidebarSection::Plan => {
                    // Plan header carries a progress count when there's a plan.
                    let count = self
                        .plan
                        .as_ref()
                        .map(|p| {
                            let total = p.steps.len();
                            let done = p
                                .steps
                                .iter()
                                .filter(|s| s.status == crate::event::PlanStepStatus::Completed)
                                .count();
                            format!("  {done}/{total}")
                        })
                        .unwrap_or_default();
                    vec![
                        Span::raw(" "),
                        Span::styled(marker, theme::CHROME),
                        Span::raw(" "),
                        Span::styled("Plan", theme::BRAND),
                        Span::styled(count, theme::CHROME),
                    ]
                }
                _ => vec![
                    Span::raw(" "),
                    Span::styled(marker, theme::CHROME),
                    Span::raw(" "),
                    Span::styled(section.label(), theme::BRAND),
                ],
            };
            lines.push(maybe_cursor_line(Line::from(header_spans), selected));

            if !folded {
                match section {
                    SidebarSection::Session => {
                        let label = if self.agent_label.is_empty() {
                            "hwcloud".to_string()
                        } else {
                            self.agent_label.clone()
                        };
                        lines.push(Line::styled(format!("  {}", truncate(&label, 24)), theme::BODY));
                        lines.push(Line::styled(
                            format!("  {}", self.session_label()),
                            theme::CHROME,
                        ));
                    }
                    SidebarSection::Context => {
                        if self.usage.size > 0 {
                            let pct = ((self.usage.used as f64 / self.usage.size as f64) * 100.0)
                                .round() as u64;
                            lines.push(Line::styled(
                                format!("  {} tokens", comma(self.usage.used)),
                                theme::BODY,
                            ));
                            lines.push(Line::styled(
                                format!("  {}% used", pct),
                                theme::CHROME,
                            ));
                        } else {
                            lines.push(Line::styled("  —", theme::CHROME));
                        }
                        if let Some(c) = &self.usage.cost {
                            lines.push(Line::styled(
                                format!("  {:.4} {}", c.amount, c.currency),
                                theme::CHROME,
                            ));
                        }
                    }
                    SidebarSection::Plan => {
                        let empty = self
                            .plan
                            .as_ref()
                            .map(|p| p.steps.is_empty())
                            .unwrap_or(true);
                        if empty {
                            lines.push(Line::styled("  no active plan", theme::CHROME));
                        } else if let Some(plan) = &self.plan {
                            for step in &plan.steps {
                                let (glyph, gstyle) = match step.status {
                                    crate::event::PlanStepStatus::Completed => {
                                        ("✓", theme::SUCCESS)
                                    }
                                    crate::event::PlanStepStatus::InProgress => {
                                        ("◐", theme::WARN)
                                    }
                                    crate::event::PlanStepStatus::Pending => {
                                        ("○", theme::CHROME)
                                    }
                                };
                                let pstyle = match step.priority {
                                    crate::event::PlanStepPriority::High => theme::WARN,
                                    _ => theme::CHROME,
                                };
                                lines.push(Line::from(vec![
                                    Span::raw(" "),
                                    Span::styled(glyph, gstyle),
                                    Span::raw(" "),
                                    Span::styled(wrap_step(&step.content), pstyle),
                                ]));
                            }
                        }
                    }
                }
            }
            // Breathing room between sections.
            lines.push(blank());
        }
        lines
    }

    /// Read a section's folded state.
    pub fn section_folded(&self, section: SidebarSection) -> bool {
        match section {
            SidebarSection::Session => self.session_folded,
            SidebarSection::Context => self.context_folded,
            SidebarSection::Plan => self.plan_folded,
        }
    }

    /// Toggle a section's folded state. Public so the mouse handler can
    /// toggle an arbitrary section directly (not just the selected one).
    pub fn toggle_section(&mut self, section: SidebarSection) {
        match section {
            SidebarSection::Session => self.session_folded = !self.session_folded,
            SidebarSection::Context => self.context_folded = !self.context_folded,
            SidebarSection::Plan => self.plan_folded = !self.plan_folded,
        }
    }

    /// How many screen rows a sidebar section occupies (header + body lines
    /// + trailing blank). Deterministic from App state — used by the mouse
    /// handler to map a clicked row to a section without walking rendered
    /// output. Must stay in sync with `sidebar_lines()`.
    pub fn section_line_count(&self, section: SidebarSection) -> usize {
        let folded = self.section_folded(section);
        let body = if folded {
            0
        } else {
            match section {
                SidebarSection::Session => 2, // label + session_label
                SidebarSection::Context => {
                    let tokens = if self.usage.size > 0 { 2 } else { 1 };
                    let cost = if self.usage.cost.is_some() { 1 } else { 0 };
                    tokens + cost
                }
                SidebarSection::Plan => {
                    let n = self
                        .plan
                        .as_ref()
                        .map(|p| p.steps.len())
                        .unwrap_or(0);
                    n.max(1) // at least the "no active plan" placeholder
                }
            }
        };
        1 + body + 1 // header + body + trailing blank
    }

    /// The option ids of the current permission modal, if any.
    pub fn permission_options(&self) -> Vec<(PermissionOptionId, String)> {
        self.pending_permission
            .as_ref()
            .map(|p| {
                p.options
                    .iter()
                    .map(|o| (o.option_id.clone(), o.name.clone()))
                    .collect()
            })
            .unwrap_or_default()
    }

    /// Tear down the permission modal after the user has answered (or
    /// cancelled). Clears the pending request and — crucially — flips the
    /// turn back to `Streaming` so the status bar and key routing stop
    /// treating the screen as modal. The answer is in flight to the ACP
    /// layer; the next chunk (or stop) will arrive shortly and set the
    /// correct terminal state. Without this, the modal vanishes but
    /// `turn == AwaitingPermission` lingers, so `handle_key` keeps routing
    /// every key into `handle_permission_key` and the status bar still reads
    /// "awaiting permission".
    pub fn clear_permission(&mut self) {
        self.pending_permission = None;
        self.turn = TurnState::Streaming;
    }

    /// Clamp the cursor to the transcript range (called after any mutation
    /// that could shrink the transcript, and to pin it to the tail while
    /// streaming).
    pub fn snap_cursor_to_tail(&mut self) {
        if self.transcript.is_empty() {
            self.cursor = 0;
        } else {
            self.cursor = self.transcript.len() - 1;
        }
    }

    /// Move the history cursor up, landing on the previous *foldable* entry
    /// (Thought/Tool). Skipping non-foldable rows (user/agent messages)
    /// means `Tab` always targets something expandable — no dead stops on a
    /// plain message where toggling does nothing.
    pub fn move_cursor_up(&mut self) {
        let mut i = self.cursor;
        while i > 0 {
            i -= 1;
            if self.transcript.get(i).is_some_and(|e| e.is_foldable()) {
                self.cursor = i;
                return;
            }
        }
        // No foldable entry above — stay put rather than landing on a
        // non-foldable row.
    }

    /// Move the history cursor down to the next foldable entry (see
    /// `move_cursor_up` for the skip rationale).
    pub fn move_cursor_down(&mut self) {
        let mut i = self.cursor;
        while i + 1 < self.transcript.len() {
            i += 1;
            if self.transcript.get(i).is_some_and(|e| e.is_foldable()) {
                self.cursor = i;
                return;
            }
        }
    }

    /// Toggle the collapsed state of the entry under the cursor. If the
    /// cursor isn't on a foldable entry (e.g. it's pinned to the tail on an
    /// agent message after a turn), find the nearest foldable entry —
    /// searching backward first, then forward — and toggle that. This makes
    /// `Tab` "just work" without forcing the user to navigate the cursor
    /// onto a foldable row first.
    pub fn toggle_collapse_at_cursor(&mut self) {
        if self.transcript.is_empty() {
            return;
        }
        // Fast path: cursor is already on a foldable entry.
        if self
            .transcript
            .get(self.cursor)
            .is_some_and(|e| e.is_foldable())
        {
            if let Some(entry) = self.transcript.get_mut(self.cursor) {
                entry.toggle_collapse();
            }
            return;
        }
        // Search backward from the cursor for the nearest foldable entry.
        let mut i = self.cursor;
        while i > 0 {
            i -= 1;
            if self.transcript.get(i).is_some_and(|e| e.is_foldable()) {
                self.cursor = i;
                if let Some(entry) = self.transcript.get_mut(i) {
                    entry.toggle_collapse();
                }
                return;
            }
        }
        // None above — search forward.
        let mut j = self.cursor;
        while j + 1 < self.transcript.len() {
            j += 1;
            if self.transcript.get(j).is_some_and(|e| e.is_foldable()) {
                self.cursor = j;
                if let Some(entry) = self.transcript.get_mut(j) {
                    entry.toggle_collapse();
                }
                return;
            }
        }
        // No foldable entry anywhere — nothing to toggle.
    }

    /// Cycle focus between the History and Plan panes.
    pub fn cycle_focus(&mut self) {
        self.focus = match self.focus {
            Focus::History => Focus::Plan,
            Focus::Plan => Focus::History,
        };
    }

    /// Move the sidebar selection up one section (wraps around). Only
    /// meaningful when `focus == Plan`.
    pub fn move_section_up(&mut self) {
        self.selected_section = self.selected_section.prev();
    }

    /// Move the sidebar selection down one section (wraps around).
    pub fn move_section_down(&mut self) {
        self.selected_section = self.selected_section.next();
    }

    /// Toggle the folded state of the currently-selected sidebar section.
    /// Called by `Tab` when `focus == Plan`.
    pub fn toggle_selected_section(&mut self) {
        self.toggle_section(self.selected_section);
    }
}

/// A speaker-label line: `  <glyph> <label>` with the label styled.
fn speaker_line(glyph: &str, label: &str, style: Style) -> Line<'static> {
    Line::from(vec![
        Span::raw("  "),
        Span::styled(format!("{glyph} {label}"), style),
    ])
}

/// Body text for a message: each source line is hard-wrapped to `width`
/// columns (indented 4 spaces), producing one `Line` per screen row. We wrap
/// ourselves rather than relying on ratatui's `Paragraph::wrap` so that the
/// `row_to_entry` map stays an exact 1:1 with screen rows — which mouse
/// hit-testing depends on.
///
/// Wrapping is char-count based (not display-width aware), so wide CJK
/// glyphs may run a cell or two past the edge. Acceptable for v1; the
/// alternative is pulling in `unicode-width`.
fn body_lines(text: &str, style: Style, width: u16) -> Vec<Line<'static>> {
    const INDENT: &str = "    ";
    let indent_w = INDENT.chars().count();
    // Available columns for text after the indent. Floor at 1 to avoid
    // infinite loops on tiny panes.
    let avail = (width as usize).saturating_sub(indent_w).max(1);
    let mut out: Vec<Line<'static>> = Vec::new();
    for src_line in text.lines() {
        for chunk in wrap_text(src_line, avail) {
            out.push(Line::styled(format!("{INDENT}{chunk}"), style));
        }
    }
    if out.is_empty() {
        // An empty source line still renders as an indented blank row so the
        // entry's vertical rhythm is preserved.
        out.push(Line::styled(INDENT.to_string(), style));
    }
    out
}

/// An empty line used as breathing room between transcript entries.
fn blank() -> Line<'static> {
    Line::raw("")
}

/// Apply the cursor highlight to a line by restyling its spans onto the
/// `SELECTED` (inverted) background. We keep the per-span foreground colors
/// where they read as identity (tool name, glyph) and only flip the
/// background, so the highlighted row still "means" what it did before.
fn cursor_line(line: Line<'static>) -> Line<'static> {
    let spans: Vec<Span<'static>> = line
        .spans
        .into_iter()
        .map(|mut s| {
            s.style = s.style.patch(theme::SELECTED);
            s
        })
        .collect();
    Line::from(spans)
}

/// Conditionally apply the cursor highlight.
fn maybe_cursor_line(line: Line<'static>, is_cursor: bool) -> Line<'static> {
    if is_cursor {
        cursor_line(line)
    } else {
        line
    }
}

/// Folded one-line tool row: `  ▸ <name>  <title>  <glyph>[: <summary>]`.
/// The `▸` prefix signals "expandable"; the rest mirrors the old tool row
/// so a folded tool call still reads as a tool call.
fn folded_tool_line(view: &ToolCallView) -> Line<'static> {
    let (glyph, gstyle) = status_glyph(&view.status);
    let (name, title) = tool_name_title(view);
    let mut spans = vec![
        Span::raw("  ▸ "),
        Span::styled(name, theme::TOOL_NAME),
        Span::raw(title),
        Span::raw("  "),
        Span::styled(glyph, gstyle),
    ];
    if let Some(s) = view.summary.as_deref().filter(|s| !s.is_empty()) {
        spans.push(Span::styled(format!(": {}", truncate(s, 50)), theme::CHROME));
    }
    Line::from(spans)
}

/// Expanded tool headline: same row but with `▾` (open marker) and no
/// summary (the summary body is rendered below it by the caller).
fn expanded_tool_head(view: &ToolCallView) -> Line<'static> {
    let (glyph, gstyle) = status_glyph(&view.status);
    let (name, title) = tool_name_title(view);
    Line::from(vec![
        Span::raw("  ▾ "),
        Span::styled(name, theme::TOOL_NAME),
        Span::raw(title),
        Span::raw("  "),
        Span::styled(glyph, gstyle),
    ])
}

/// Shared status → (glyph, style) mapping for tool calls.
fn status_glyph(status: &str) -> (&'static str, Style) {
    match status {
        "completed" => ("✓", theme::SUCCESS),
        "failed" => ("✗", theme::ERROR),
        "in_progress" => ("⏳", theme::WARN),
        _ => ("·", theme::CHROME),
    }
}

/// Shared (name, title) extraction: if `kind` is a real tool kind, show
/// `kind` as the name and `title` indented after it; otherwise the title
/// alone stands in for the name.
fn tool_name_title(view: &ToolCallView) -> (String, String) {
    if view.kind.is_empty() || view.kind == "other" {
        (view.title.clone(), String::new())
    } else {
        (view.kind.clone(), format!("  {}", view.title))
    }
}

/// Truncate to `n` chars (by char count), appending `…` if cut.
fn truncate(s: &str, n: usize) -> String {
    if s.chars().count() <= n {
        s.to_string()
    } else {
        let mut out: String = s.chars().take(n.saturating_sub(1)).collect();
        out.push('…');
        out
    }
}

/// Hard-wrap a single line of text to `width` columns, returning one string
/// per screen row. Wraps on word boundaries (spaces) when possible; a word
/// longer than `width` is broken mid-word so it still fits. Char-count based
/// (see `body_lines` caveat about wide glyphs).
///
/// Empty input yields a single empty string (one blank row) so the caller's
/// row accounting stays consistent.
fn wrap_text(line: &str, width: usize) -> Vec<String> {
    if width == 0 {
        return vec![line.to_string()];
    }
    if line.is_empty() {
        return vec![String::new()];
    }
    let mut rows: Vec<String> = Vec::new();
    let mut current = String::new();
    for word in line.split_whitespace() {
        let word_len = word.chars().count();
        // `current` already holds some text — decide if `word` fits after a
        // space, else flush and start a new row.
        if !current.is_empty() {
            let current_len = current.chars().count();
            let with_space = current_len + 1 + word_len;
            if with_space <= width {
                current.push(' ');
                current.push_str(word);
                continue;
            }
            // Doesn't fit — flush current.
            rows.push(std::mem::take(&mut current));
        }
        // `current` is now empty; place `word` (breaking it if it alone
        // exceeds the width).
        if word_len <= width {
            current.push_str(word);
        } else {
            // Break the over-long word into width-sized chunks. Each chunk
            // is its own row; `current` stays empty for the next word.
            let mut chars = word.chars();
            loop {
                let chunk: String = chars.by_ref().take(width).collect();
                if chunk.is_empty() {
                    break;
                }
                rows.push(chunk);
            }
        }
    }
    if !current.is_empty() || rows.is_empty() {
        rows.push(current);
    }
    rows
}

/// Like `truncate` but collapses to a single line first (folding a
/// multi-line thought preview onto one row for the folded summary).
fn truncate_single(s: &str, n: usize) -> String {
    let one_line: String = s.split_whitespace().collect::<Vec<_>>().join(" ");
    truncate(&one_line, n)
}

/// A plan step's content, trimmed of surrounding whitespace and truncated
/// to keep the sidebar column tidy. Wrapping is left to ratatui at draw
/// time, but we cap at a generous length so a runaway step doesn't blow
/// out the column.
fn wrap_step(content: &str) -> String {
    let trimmed = content.trim();
    truncate(trimmed, 48)
}

/// Format an integer with thousands separators (`7,649`). Reads better than
/// a bare `7649` in the Context panel.
fn comma(n: u64) -> String {
    let s = n.to_string();
    let bytes = s.as_bytes();
    let mut out = String::with_capacity(s.len() + s.len() / 3);
    for (i, b) in bytes.iter().enumerate() {
        if i > 0 && (bytes.len() - i).is_multiple_of(3) {
            out.push(',');
        }
        out.push(*b as char);
    }
    out
}
