//! `hwcloud-tui` — interactive TUI client for hwcloud.
//!
//! A ratatui front-end that talks to any ACP backend over stdio via the
//! official Rust SDK. The backend launch command is resolved at runtime:
//!
//! ```text
//! hwcloud-tui                              # `<AGENT_BIN> serve --acp` (default)
//! hwcloud-tui --backend "myagent serve --acp"
//! hwcloud-tui --backend "npx -y @zed/claude-code-acp"
//! hwcloud-tui --backend "ENV=val opencode acp"
//! ```
//!
//! `--backend` takes a full shell-style command line and can target *any*
//! ACP backend, not just hwcloud. The default is
//! `<AGENT_BIN> serve --acp`, where `AGENT_BIN` is the compile-time binary
//! name (mirrors Go `version.Name`: `hwcloud`, or `myagent` in a branded
//! build — both injected by `build.sh`).
//!
//! Keys:
//!   Enter         send the current prompt
//!   Esc           cancel the in-flight turn
//!   Ctrl-C        quit
//!   Up/Down       move selection (history cursor / sidebar section) or scroll
//!   Tab           expand/collapse the selected thinking/tool section (history)
//!                 or the selected sidebar section (sidebar)
//!   Shift-Tab     cycle focus between history and the sidebar
//!   PageUp/Down   scroll history by a page
//!   Up/Down       move selection in the permission modal
//!   Enter         confirm the selected permission option
//!   1..9          choose a permission option when the modal is shown
//!   Esc           dismiss the permission modal (cancels the tool call)
//!   Click         on a permission option → select and confirm it
//!
//! Mouse (enabled):
//!   Click         on a thinking/tool header line → expand/collapse it;
//!                 on a sidebar section header → fold/unfold that section
//!   Wheel         scroll the history pane (down = older, up = newer)

mod acp;
mod app;
mod event;
mod theme;
mod ui;

use std::io;
use std::time::Duration;

use clap::Parser;
use crossterm::event::{
    DisableMouseCapture, EnableMouseCapture, Event, KeyCode, KeyEvent, KeyEventKind, KeyModifiers,
    MouseButton, MouseEvent, MouseEventKind,
};
use crossterm::terminal::{
    disable_raw_mode, enable_raw_mode, EnterAlternateScreen, LeaveAlternateScreen,
};
use crossterm::ExecutableCommand;
use futures::StreamExt;
use futures::channel::mpsc;
use ratatui::backend::CrosstermBackend;
use ratatui::Terminal;
use tui_textarea::Input;

use crate::app::{Action, App, Focus, SidebarSection, TranscriptLine, TurnState};
use crate::event::UiEvent;

/// Command-line interface.
#[derive(Parser)]
#[command(
    name = "hwcloud-tui",
    about = "Interactive TUI client for hwcloud (ACP over stdio)",
    long_about = None,
)]
struct Cli {
    /// Full backend launch command (shell-style). Targets *any* ACP backend,
    /// not just hwcloud: `npx -y @zed/claude-code-acp`,
    /// `ENV=val opencode acp`, `./mywrapper hwcloud serve --acp`, …
    /// Defaults to `<AGENT_BIN> serve --acp` (AGENT_BIN is the compile-time
    /// binary name: `hwcloud`, or `myagent` in a branded build).
    #[arg(long, value_name = "COMMAND")]
    backend: Option<String>,
}

#[tokio::main]
async fn main() -> io::Result<()> {
    let cli = Cli::parse();

    // Resolve the backend launch command. The default is
    // `<AGENT_BIN> serve --acp` — AGENT_BIN is baked in at compile time by
    // build.sh (mirrors Go version.Name). `--backend` overrides the whole
    // command so the TUI can drive any ACP backend.
    let command = cli
        .backend
        .unwrap_or_else(|| format!("{} serve --acp", acp::AGENT_BIN));

    // Channels:
    //   events_tx  ← ACP layer pushes UiEvents; UI drains them.
    //   actions_tx ← UI pushes Actions;     ACP layer drains them.
    let (events_tx, mut events_rx) = mpsc::unbounded::<UiEvent>();
    let (actions_tx, actions_rx) = mpsc::unbounded::<Action>();

    // Spawn the ACP client on a background task. It owns the backend process.
    let acp_task = tokio::spawn(acp::run(events_tx, actions_rx, command));

    // ----- terminal setup -----
    let mut stdout = io::stdout();
    stdout.execute(EnterAlternateScreen)?;
    // Enable mouse capture so the EventStream yields `Event::Mouse(...)`.
    // Without this, clicks never reach us — the terminal intercepts them
    // for text selection instead.
    stdout.execute(EnableMouseCapture)?;
    enable_raw_mode()?;
    let backend = CrosstermBackend::new(stdout);
    let mut terminal = Terminal::new(backend)?;

    let mut app = App::new();

    // crossterm async event stream for keyboard + mouse input.
    let mut events = crossterm::event::EventStream::new();

    // Terminal restore on any exit path.
    let restore = || -> io::Result<()> {
        disable_raw_mode()?;
        io::stdout().execute(DisableMouseCapture)?;
        io::stdout().execute(LeaveAlternateScreen)?;
        Ok(())
    };

    let tick = Duration::from_millis(80);
    loop {
        // Advance the animation frame on every render so the spinner
        // progresses during the "thinking" gap before the first token.
        app.tick();
        // Render.
        terminal.draw(|f| ui::draw(f, &mut app)).ok();

        // Wait for a keyboard event or a UI event from the ACP layer. While a
        // turn is active (streaming / awaiting permission), also poll on a
        // tick so the spinner animates and streaming buffers repaint; when
        // idle, skip the tick so we don't burn CPU re-rendering a static
        // screen.
        let key_fut = events.next();
        let ui_fut = events_rx.next();
        let animating = matches!(
            app.turn,
            TurnState::Streaming | TurnState::AwaitingPermission
        );
        tokio::select! {
            biased;

            // ACP-layer events take priority so streaming tokens land flush.
            Some(ev) = ui_fut => {
                app.handle_event(ev);
                app.snap_to_tail();
            }

            maybe_ev = key_fut => {
                let Some(Ok(ev)) = maybe_ev else { continue; };
                match ev {
                    Event::Key(key) => {
                        if key.kind != KeyEventKind::Press { continue; }
                        if !handle_key(&mut app, key, &actions_tx) {
                            // `false` means quit.
                            let _ = actions_tx.unbounded_send(Action::Quit);
                            break;
                        }
                    }
                    Event::Mouse(me) => {
                        handle_mouse(&mut app, me, &actions_tx);
                    }
                    // Resize/FocusGained/etc. — the next `draw()` replays the
                    // layout on the new area, so nothing to do here.
                    _ => {}
                }
            }

            _ = tokio::time::sleep(tick), if animating => {
                // Animation tick — re-render so the spinner advances.
            }
        }

        if app.quit {
            break;
        }

        // If the ACP task finished (backend died), stop the UI.
        if acp_task.is_finished() {
            // Drain any final events.
            while let Some(ev) = events_rx.next().await {
                app.handle_event(ev);
            }
            break;
        }
    }

    restore()?;
    // Give the ACP task a moment to tear down; ignore its result.
    let _ = tokio::time::timeout(Duration::from_secs(2), acp_task).await;
    Ok(())
}

/// Handle a key press. Returns `false` if the user wants to quit.
fn handle_key(app: &mut App, key: KeyEvent, actions_tx: &mpsc::UnboundedSender<Action>) -> bool {
    // Permission modal takes over key handling.
    if app.turn == TurnState::AwaitingPermission {
        return handle_permission_key(app, key, actions_tx);
    }

    match (key.code, key.modifiers) {
        (KeyCode::Char('c'), KeyModifiers::CONTROL) => {
            app.quit = true;
            false
        }
        (KeyCode::Char('d'), KeyModifiers::CONTROL) => {
            // Ctrl-D quits only when the input is empty (EOF convention).
            if app.input.lines().iter().all(|l| l.is_empty()) {
                app.quit = true;
                return false;
            }
            app.input.input(Input::from(key));
            true
        }
        (KeyCode::Esc, _) => {
            // Cancel the in-flight turn if streaming.
            if app.turn == TurnState::Streaming {
                if let Some(sid) = &app.session_id {
                    let _ = actions_tx.unbounded_send(Action::Cancel {
                        session_id: sid.clone(),
                    });
                    app.notice = "cancelling…".into();
                }
            }
            true
        }
        (KeyCode::Tab, _) => {
            // Context-dependent: in History, toggle the folded transcript
            // entry under the cursor; in the sidebar, toggle the selected
            // section's fold. Only while idle/done — during streaming the
            // user is reading, not browsing.
            if app.turn == TurnState::Idle || app.turn == TurnState::Done {
                match app.focus {
                    Focus::History => app.toggle_collapse_at_cursor(),
                    Focus::Plan => app.toggle_selected_section(),
                }
            }
            true
        }
        (KeyCode::BackTab, _) => {
            // Shift-Tab cycles focus between the history and sidebar panes.
            app.cycle_focus();
            true
        }
        (KeyCode::Enter, _) => {
            // Submit the prompt (multi-line: Enter submits, Alt-Enter for
            // newline — tui-textarea maps Alt+Enter to a newline insert).
            let text: String = app.input.lines().join("\n");
            if text.trim().is_empty() {
                return true;
            }
            if let Some(sid) = &app.session_id {
                let _ = actions_tx.unbounded_send(Action::Prompt {
                    session_id: sid.clone(),
                    text,
                });
                // Mirror the user's message into the transcript.
                app.transcript
                    .push(TranscriptLine::User(app.input.lines().join("\n")));
                app.input = tui_textarea::TextArea::default();
                app.input.set_placeholder_text(
                    "Message the agent…  (Enter to send, Esc to cancel, Ctrl-C to quit)",
                );
                app.snap_to_tail();
                app.snap_cursor_to_tail();
            } else {
                app.notice = "no session yet".into();
            }
            true
        }
        // Navigation. In the sidebar (focus == Plan), Up/Down always move
        // between sections regardless of turn state (the sidebar is a
        // status panel, not streaming content). In History, idle Up/Down
        // move the selection cursor; streaming Up/Down scroll the viewport.
        (KeyCode::Up, m) if !m.contains(KeyModifiers::SHIFT) => {
            if app.focus == Focus::Plan {
                app.move_section_up();
            } else if app.turn == TurnState::Idle || app.turn == TurnState::Done {
                app.move_cursor_up();
            } else {
                app.scroll = app.scroll.saturating_add(1);
            }
            true
        }
        (KeyCode::Down, m) if !m.contains(KeyModifiers::SHIFT) => {
            if app.focus == Focus::Plan {
                app.move_section_down();
            } else if app.turn == TurnState::Idle || app.turn == TurnState::Done {
                app.move_cursor_down();
            } else {
                app.scroll = app.scroll.saturating_sub(1);
            }
            true
        }
        (KeyCode::PageUp, _) => {
            app.scroll = app.scroll.saturating_add(10);
            true
        }
        (KeyCode::PageDown, _) => {
            app.scroll = app.scroll.saturating_sub(10);
            true
        }
        // Everything else goes to the textarea — and a typing key means the
        // user's attention is back on the input, so snap focus to History
        // (otherwise the cursor highlight stays on the sidebar while they
        // type, which reads as "nothing happened").
        _ => {
            app.input.input(Input::from(key));
            app.focus = Focus::History;
            true
        }
    }
}

/// Key handling while the permission modal is up.
fn handle_permission_key(
    app: &mut App,
    key: KeyEvent,
    actions_tx: &mpsc::UnboundedSender<Action>,
) -> bool {
    let options = app.permission_options();
    let last = options.len().saturating_sub(1);
    match key.code {
        KeyCode::Char('c') if key.modifiers.contains(KeyModifiers::CONTROL) => {
            app.quit = true;
            false
        }
        KeyCode::Esc => {
            // Cancel the tool call.
            let _ = actions_tx.unbounded_send(Action::AnswerPermission { option_id: None });
            app.clear_permission();
            true
        }
        // Arrow-key navigation through the options list.
        KeyCode::Up => {
            app.permission_cursor = app.permission_cursor.saturating_sub(1);
            true
        }
        KeyCode::Down => {
            if app.permission_cursor < last {
                app.permission_cursor += 1;
            }
            true
        }
        // Enter confirms the currently-selected option.
        KeyCode::Enter => {
            if let Some((id, _name)) = options.get(app.permission_cursor) {
                let _ = actions_tx.unbounded_send(Action::AnswerPermission {
                    option_id: Some(id.clone()),
                });
                app.clear_permission();
            }
            true
        }
        // 1..9 jump-selects and confirms in one keystroke.
        KeyCode::Char(c) if c.is_ascii_digit() && c != '0' => {
            let idx = (c as u8 - b'1') as usize;
            if let Some((id, _name)) = options.get(idx) {
                let _ = actions_tx.unbounded_send(Action::AnswerPermission {
                    option_id: Some(id.clone()),
                });
                app.clear_permission();
            }
            true
        }
        _ => true,
    }
}

/// Handle a mouse event. Left-click toggles the foldable entry under the
/// cursor (history) or the sidebar section under the cursor; the wheel
/// scrolls the history pane. Everything else is ignored.
///
/// Pane hit-testing reuses the geometry cached on `App` by the last
/// `draw()` — `history_pane_y/right`, `sidebar_x/y`, and the `row_to_entry`
/// map + `history_top` window offset. We don't replay the `Layout` here
/// because the cached fields already give us the pane bounds directly.
fn handle_mouse(app: &mut App, me: MouseEvent, actions_tx: &mpsc::UnboundedSender<Action>) {
    // The permission modal takes over mouse handling while it's up: a click
    // on an option selects+confirms it. Scrolling inside the modal cycles the
    // selection.
    if app.turn == TurnState::AwaitingPermission {
        match me.kind {
            MouseEventKind::Down(MouseButton::Left) => {
                handle_permission_mouse_click(app, me.column, me.row, actions_tx);
            }
            MouseEventKind::ScrollDown => {
                let last = app.permission_options().len().saturating_sub(1);
                if app.permission_cursor < last {
                    app.permission_cursor += 1;
                }
            }
            MouseEventKind::ScrollUp => {
                app.permission_cursor = app.permission_cursor.saturating_sub(1);
            }
            _ => {}
        }
        return;
    }
    match me.kind {
        MouseEventKind::ScrollDown => {
            // Wheel down → look at older content (scroll offset grows).
            app.scroll = app.scroll.saturating_add(3);
        }
        MouseEventKind::ScrollUp => {
            // Wheel up → return to the tail (scroll offset shrinks).
            app.scroll = app.scroll.saturating_sub(3);
        }
        MouseEventKind::Down(MouseButton::Left) => {
            handle_mouse_click(app, me.column, me.row);
        }
        // Up/Drag/Moved — not actionable.
        _ => {}
    }
}

/// Resolve a left-click at screen `(col, row)` to a toggle action.
fn handle_mouse_click(app: &mut App, col: u16, row: u16) {
    // History pane: rows in [history_pane_y, history_pane_y + height) and
    // columns left of the vrule (history_pane_right).
    let in_history = row >= app.history_pane_y
        && row < app.history_pane_y + app.history_pane_height as u16
        && col < app.history_pane_right;
    // Sidebar pane: rows at/after sidebar_y and columns at/after sidebar_x.
    // Use the same body height bound as history (they share the body row).
    let in_sidebar = row >= app.sidebar_y
        && row < app.history_pane_y + app.history_pane_height as u16
        && col >= app.sidebar_x;

    if in_history {
        handle_history_click(app, row);
    } else if in_sidebar {
        handle_sidebar_click(app, row);
    }
    // Clicks on topbar/input/status — no-op.
}

/// A click inside the history pane. Map the screen row to a transcript
/// entry via the cached `row_to_entry` + `history_top` window; if that
/// entry is foldable, toggle it and move the cursor there so the highlight
/// follows.
fn handle_history_click(app: &mut App, row: u16) {
    let rel = (row - app.history_pane_y) as usize;
    let idx = app.history_top + rel;
    let Some(&entry_idx) = app.row_to_entry.get(idx) else {
        return;
    };
    // usize::MAX marks streaming-only tail rows (spinner, live buffers) —
    // not a committed entry, not clickable.
    if entry_idx == usize::MAX {
        return;
    }
    let foldable = app
        .transcript
        .get(entry_idx)
        .is_some_and(|e| e.is_foldable());
    if !foldable {
        return;
    }
    app.focus = Focus::History;
    app.cursor = entry_idx;
    if let Some(entry) = app.transcript.get_mut(entry_idx) {
        entry.toggle_collapse();
    }
}

/// A click inside the sidebar. Walk the three sections in order, subtracting
/// each section's line count (cached-computable from App state) until the
/// click's relative row falls within a section — then toggle that section
/// and mark it selected so the highlight follows.
fn handle_sidebar_click(app: &mut App, row: u16) {
    let rel = (row - app.sidebar_y) as usize;
    let mut offset = 0usize;
    for section in SidebarSection::ALL {
        let count = app.section_line_count(section);
        if rel < offset + count {
            app.focus = Focus::Plan;
            app.selected_section = section;
            app.toggle_section(section);
            return;
        }
        offset += count;
    }
    // Below the last section's content — no-op.
}

/// A click while the permission modal is up. The modal's option-list rect is
/// cached on `App` by `draw_permission_modal`; a click inside it maps to an
/// option index (row 0 = first option), which we select and confirm in one
/// action — matching the "click to choose" affordance the user expects.
fn handle_permission_mouse_click(
    app: &mut App,
    col: u16,
    row: u16,
    actions_tx: &mpsc::UnboundedSender<Action>,
) {
    let r = app.permission_modal_list_rect;
    // Inside the list's bordered area?
    if col < r.x || col >= r.x + r.width || row < r.y || row >= r.y + r.height {
        // Click outside the modal — ignore (no dismiss-on-outside-click; the
        // user must explicitly pick or Esc-cancel).
        return;
    }
    // The list has a 1-cell border, so option rows start at r.y + 1. Each
    // option occupies exactly one rendered line.
    let rel = row.saturating_sub(r.y + 1) as usize;
    let options = app.permission_options();
    let Some((id, _name)) = options.get(rel) else {
        return;
    };
    app.permission_cursor = rel;
    let _ = actions_tx.unbounded_send(Action::AnswerPermission {
        option_id: Some(id.clone()),
    });
    app.clear_permission();
}
