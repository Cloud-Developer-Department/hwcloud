//! Centralized visual theme for the TUI.
//!
//! One place to tune the whole look. The palette is deliberately restrained:
//! a single accent (Blue) for the agent identity, Cyan to distinguish the
//! user, semantic Green/Yellow/Red for status, and DarkGray for all chrome
//! (borders, separators, labels, quiet status-bar text). No high-saturation
//! title bars, no `bg(Black)` overrides — the terminal's default background
//! shows through everywhere.
//!
//! Styleguide: body text stays `Color::Reset` (inherits the terminal
//! foreground) so the screen reads as text-first, chrome-last — the inverse
//! of the old ncurses look where every pane wore a bold colored title.

use ratatui::style::{Color, Modifier, Style};

// ── Identity / labels ──

/// Agent speaker label (`✻ agent`) and the streaming cursor.
pub const AGENT: Style = Style::new().fg(Color::Blue).add_modifier(Modifier::BOLD);

/// User speaker label (`● you`).
pub const USER: Style = Style::new().fg(Color::Cyan).add_modifier(Modifier::BOLD);

// ── Status semantics ──

pub const SUCCESS: Style = Style::new().fg(Color::Green);
pub const WARN: Style = Style::new().fg(Color::Yellow);
pub const ERROR: Style = Style::new().fg(Color::Red);

// ── Chrome (quiet) ──

/// Borders, separators, the session id in the top bar, thought text,
/// system notices — anything that should recede behind body text.
pub const CHROME: Style = Style::new().fg(Color::DarkGray);

/// Tool-call name (`read`, `bash`, …) — accent but not bold-loud.
pub const TOOL_NAME: Style = Style::new().fg(Color::Blue);

/// Selected item in the permission modal: invert on the accent color.
pub const SELECTED: Style = Style::new().bg(Color::Blue).fg(Color::Black);

/// The brand word in the top bar.
pub const BRAND: Style = Style::new().fg(Color::Blue).add_modifier(Modifier::BOLD);

/// Body text — inherit the terminal foreground, no override.
pub const BODY: Style = Style::new().fg(Color::Reset);
