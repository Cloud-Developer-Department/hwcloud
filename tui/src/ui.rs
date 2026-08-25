//! ratatui rendering for the TUI — two-column borderless layout.
//!
//! Layout:
//!   ┌──────────────────────────────┬──────────────┐  (no real borders)
//!   │ hwcloud · session …a1b2c3  │              │  top bar (1 row, full width)
//!   │                              │  ▾ Session   │
//!   │   ● you                      │    hwcloud │  history (left, ~68%)
//!   │     read the README          │    …a1b2c3   │  sidebar (right, ~32%,
//!   │                              │  ▾ Context   │   always shown) — a
//!   │   ✻ agent                    │    7,649 tok │   stack of collapsible
//!   │     The first line is …      │    0% used   │   sections: Session,
//!   │   ▸ read  README.md  ✓       │    $0.00     │   Context, Plan.
//!   │                              │  ▾ Plan 2/5  │
//!   │                              │    ✓ step 1  │
//!   ╭──────────────────────────────┴──────────────╮  input (rounded, full width)
//!   │ Message the agent…                           │
//!   ╰──────────────────────────────────────────────╯
//!   ● streaming                                    │  status (1 row, full width)
//!
//! The sidebar is always shown (never collapses to a single-pane frame).
//! A thin vertical rule separates the columns. `Shift-Tab` moves focus
//! between history and the sidebar; in the sidebar, `Tab` toggles a
//! section's fold and Up/Down move between sections.
//!
//! When a permission request is pending, a centered modal overlays the
//! whole frame. The mechanism (Clear + centered Rect) is unchanged; only
//! the styling is modernized.

use ratatui::layout::{Alignment, Constraint, Direction, Layout, Rect};
use ratatui::text::{Line, Span};
use ratatui::widgets::{
    Block, BorderType, Borders, Clear, List, ListItem, ListState, Paragraph, Wrap,
};
use ratatui::Frame;

use crate::app::{App, TurnState};
use crate::theme;

/// Render the whole frame.
pub fn draw(f: &mut Frame, app: &mut App) {
    let area = f.area();
    // Cache the terminal rect so the mouse handler can replay the exact
    // same Layout constraints to recover pane rects at click time.
    app.last_area = area;

    // Four rows: top bar (1) + body (fills the rest) + input + status (1).
    // The body is then split horizontally into history + plan sidebar.
    let rows = Layout::default()
        .direction(Direction::Vertical)
        .constraints(
            [
                Constraint::Length(1),
                Constraint::Min(8),
                Constraint::Length(app.input_height()),
                Constraint::Length(1),
            ]
            .as_ref(),
        )
        .split(area);

    draw_topbar(f, app, rows[0]);

    // Always split the body into history (left) + plan sidebar (right). The
    // sidebar is permanent — when there's no plan it shows a quiet
    // "no active plan" placeholder rather than vanishing and leaving a
    // single-pane frame (which is what the user saw and called "one big box").
    let cols = Layout::default()
        .direction(Direction::Horizontal)
        .constraints([Constraint::Percentage(68), Constraint::Percentage(32)].as_ref())
        .split(rows[1]);
    // Thin vertical rule between the columns — a single-cell gutter drawn
    // in chrome so the eye separates the panes without a heavy box frame.
    draw_vrule(f, cols[0].right(), cols[0].top(), cols[0].bottom());

    // Cache the pane geometry for mouse hit-testing.
    app.history_pane_y = cols[0].y;
    app.history_pane_right = cols[0].right();
    app.sidebar_y = cols[1].y;
    app.sidebar_x = cols[1].x;

    draw_history(f, app, cols[0]);
    draw_sidebar(f, app, cols[1]);

    draw_input(f, app, rows[2]);
    draw_status(f, app, rows[3]);

    if app.pending_permission.is_some() {
        draw_permission_modal(f, app, area);
    }
}

/// One-line brand + session header. No border, no background — just text
/// sitting at the top of the screen.
fn draw_topbar(f: &mut Frame, app: &App, area: Rect) {
    let line = Line::from(vec![
        Span::styled(" hwcloud", theme::BRAND),
        Span::styled(format!(" · {}", app.session_label()), theme::CHROME),
    ]);
    f.render_widget(Paragraph::new(line), area);
}

/// The scrollable transcript pane. No surrounding box — the top bar above
/// and the input box below provide all the visual framing needed.
///
/// Lines are pre-wrapped in `history_lines(width)` (no `Paragraph::wrap`),
/// so the returned `row_to_entry` map is an exact 1:1 with screen rows —
/// which the mouse handler relies on. The map + window offsets are cached
/// back onto `App` each frame for click-time lookup.
fn draw_history(f: &mut Frame, app: &mut App, area: Rect) {
    let (lines, row_map) = app.history_lines(area.width);
    let total = lines.len();

    // `scroll` is an offset from the bottom; 0 = pinned to tail.
    let visible = area.height as usize;
    let bottom = total.saturating_sub(app.scroll.min(total));
    let top = bottom.saturating_sub(visible);
    let window: Vec<Line<'_>> = lines[top..bottom].to_vec();

    // Cache the geometry the mouse handler needs to resolve a click.
    app.row_to_entry = row_map;
    app.history_top = top;
    app.history_pane_height = visible;

    // No `.wrap()` — lines are already hard-wrapped to `area.width`.
    let paragraph = Paragraph::new(window).alignment(Alignment::Left);
    f.render_widget(paragraph, area);
}

/// The input editor — the only bordered element. Rounded, thin, chrome-gray
/// border so it reads as a field without shouting.
fn draw_input(f: &mut Frame, app: &mut App, area: Rect) {
    let block = Block::default()
        .borders(Borders::ALL)
        .border_type(BorderType::Rounded)
        .border_style(theme::CHROME);
    app.input.set_block(block);
    f.render_widget(&app.input, area);
}

/// The right-hand sidebar — a stack of collapsible sections (Session,
/// Context, Plan). Renders `app.sidebar_lines()` with wrapping. The
/// selected section's header is highlighted when the pane has focus.
fn draw_sidebar(f: &mut Frame, app: &mut App, area: Rect) {
    let lines = app.sidebar_lines();
    let para = Paragraph::new(lines)
        .wrap(Wrap { trim: false })
        .alignment(Alignment::Left);
    f.render_widget(para, area);
}

/// Draw a thin vertical separator at column `x` between `top` and `bottom`
/// (inclusive). Used as the gutter between history and the plan sidebar —
/// lighter than a full `Block` border, heavier than nothing.
fn draw_vrule(f: &mut Frame, x: u16, top: u16, bottom: u16) {
    if x == 0 {
        return;
    }
    // Clamp to the frame so we never paint outside the area.
    let y_start = top.max(f.area().top());
    let y_end = bottom.min(f.area().bottom().saturating_sub(1));
    for y in y_start..=y_end {
        let cell = Rect::new(x, y, 1, 1);
        f.render_widget(
            Paragraph::new(Line::from(Span::styled("│", theme::CHROME))),
            cell,
        );
    }
}

/// The one-line status bar. No borders, no `│` separators, no forced
/// background — just the turn mode and any transient notice. Token/cost
/// usage lives in the sidebar's Context section now, so it doesn't crowd
/// the bottom row.
fn draw_status(f: &mut Frame, app: &App, area: Rect) {
    let mode = match app.turn {
        TurnState::Idle => Span::styled("● idle", theme::CHROME),
        TurnState::Streaming => Span::styled(
            format!("{} streaming", app.spinner()),
            theme::WARN,
        ),
        TurnState::AwaitingPermission => Span::styled(
            format!("{} awaiting permission", app.spinner()),
            theme::WARN,
        ),
        TurnState::Done => Span::styled("● done", theme::SUCCESS),
    };

    let notice = if app.notice.is_empty() {
        Span::raw("")
    } else {
        Span::styled(format!("· {}", app.notice), theme::CHROME)
    };

    let line = Line::from(vec![Span::raw(" "), mode, Span::raw("  "), notice]);

    f.render_widget(Paragraph::new(line), area);
}

/// The centered permission-request modal. Clear + centered Rect (mechanism
/// unchanged); styling modernized — no bold yellow header, rounded list
/// border, accent-colored highlight.
fn draw_permission_modal(f: &mut Frame, app: &mut App, area: Rect) {
    let width = area.width.min(70).max(40);
    let height = area
        .height
        .min(app.permission_options().len() as u16 + 6)
        .max(7);
    let x = area.x + (area.width - width) / 2;
    let y = area.y + (area.height - height) / 2;
    let modal = Rect::new(x, y, width, height);

    f.render_widget(Clear, modal);

    let options = app.permission_options();

    let title = app
        .pending_permission
        .as_ref()
        .and_then(|p| p.tool_call.fields.title.clone())
        .unwrap_or_else(|| "permission request".into());

    let kind = app
        .pending_permission
        .as_ref()
        .and_then(|p| p.tool_call.fields.kind)
        .map(|k| format!("{:?}", k).to_lowercase())
        .unwrap_or_default();

    // Header: plain paragraph, no border. Two lines of context.
    let header = vec![
        Line::from(Span::styled("Permission required", theme::WARN)),
        Line::styled(format!("{} {}", title, kind), theme::CHROME),
    ];
    f.render_widget(
        Paragraph::new(header),
        Rect::new(modal.x, modal.y, modal.width, 2),
    );

    // Option list below the header, with a rounded border.
    let mut state = ListState::default();
    // Persist selection across renders from `app.permission_cursor` (moved by
    // Up/Down / mouse). Clamp to the options range so a stale cursor after the
    // option set changes can't panic the select.
    let selected = app.permission_cursor.min(options.len().saturating_sub(1));
    state.select(Some(selected));

    let items: Vec<ListItem> = options
        .iter()
        .enumerate()
        .map(|(i, (_id, name))| ListItem::new(format!("  {}. {}", i + 1, name)))
        .collect();

    let list = List::new(items)
        .block(
            Block::default()
                .borders(Borders::ALL)
                .border_type(BorderType::Rounded)
                .border_style(theme::CHROME),
        )
        .highlight_style(theme::SELECTED);

    // Cache the list's screen rect so mouse clicks can hit-test against it.
    let list_rect = Rect::new(modal.x, modal.y + 2, modal.width, modal.height - 2);
    app.permission_modal_list_rect = list_rect;

    f.render_stateful_widget(list, list_rect, &mut state);
}

/// Helper accessors kept on `App` via inherent methods.
impl App {
    /// How many terminal rows the input widget should occupy.
    pub fn input_height(&self) -> u16 {
        // tui-textarea exposes its line count; fall back to 1.
        let lines = self.input.lines().len().max(1) as u16;
        lines + 2 // rounded border (top + bottom)
    }

    /// A short label for the top bar.
    pub fn session_label(&self) -> String {
        match &self.session_id {
            Some(id) => {
                let s = id.to_string();
                let short = if s.len() > 12 { &s[s.len() - 12..] } else { &s };
                format!("session …{}", short)
            }
            None => "no session".into(),
        }
    }
}
