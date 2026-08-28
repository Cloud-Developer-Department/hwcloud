//! Events flowing from the ACP client into the UI loop.
//!
//! The SDK runs its JSON-RPC dispatch on a background task. Its
//! `on_receive_notification` / `on_receive_request` callbacks push these
//! `UiEvent`s onto an `mpsc` channel that the ratatui main loop drains on
//! every tick. This keeps all terminal rendering on the single UI thread
//! while the protocol work happens asynchronously.

use agent_client_protocol::schema::v1::{Cost, PermissionOption, SessionId, ToolCallUpdate};

/// A tool-call the agent has reported, kept in a map keyed by `tool_call_id`
/// so pending/completed/failed updates replace in place.
#[derive(Debug, Clone)]
pub struct ToolCallView {
    pub id: String,
    pub title: String,
    pub status: String,
    pub kind: String,
    /// Best-effort text summary of the tool's result content, if any.
    pub summary: Option<String>,
}

/// A pending permission request: the user must pick an option (or cancel)
/// before the turn can continue.
#[derive(Debug)]
pub struct PendingPermission {
    /// Which session the request belongs to. Retained for context/debugging
    /// even though the current UI renders only one session at a time.
    #[allow(dead_code)]
    pub session_id: SessionId,
    pub tool_call: ToolCallUpdate,
    pub options: Vec<PermissionOption>,
}

/// Status of a single plan step — mirrors the stable ACP `PlanEntryStatus`
/// (Pending / InProgress / Completed) without dragging the schema enum into
/// the UI layer.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum PlanStepStatus {
    Pending,
    InProgress,
    Completed,
}

/// Priority of a plan step — mirrors `PlanEntryPriority`.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum PlanStepPriority {
    High,
    Medium,
    Low,
}

/// One step in the agent's plan, shown in the right-hand sidebar.
#[derive(Debug, Clone)]
pub struct PlanStepView {
    pub content: String,
    pub priority: PlanStepPriority,
    pub status: PlanStepStatus,
}

/// The agent's current plan. ACP sends a *full replacement* on every update,
/// so this is stored whole on `App` and re-rendered each frame.
#[derive(Debug, Clone, Default)]
pub struct PlanView {
    pub steps: Vec<PlanStepView>,
}

/// Token/cost snapshot for the status bar.
#[derive(Debug, Clone, Default)]
pub struct UsageView {
    pub used: u64,
    pub size: u64,
    pub cost: Option<Cost>,
}

/// Why the agent stopped the current turn.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum StopView {
    EndTurn,
    MaxTokens,
    MaxTurnRequests,
    Refusal,
    Cancelled,
    Unknown,
}

impl StopView {
    pub fn label(self) -> &'static str {
        match self {
            StopView::EndTurn => "turn complete",
            StopView::MaxTokens => "max tokens",
            StopView::MaxTurnRequests => "max turn requests",
            StopView::Refusal => "refused",
            StopView::Cancelled => "cancelled",
            StopView::Unknown => "stopped",
        }
    }
}

/// Everything the UI needs to know about, produced by the ACP layer.
#[derive(Debug)]
pub enum UiEvent {
    /// The backend agent process was spawned (or failed to).
    AgentSpawned { ok: bool, detail: String },
    /// `initialize` + `session/new` handshake finished. Carries the agent's
    /// self-reported label (from `agent_info`) for the sidebar header.
    SessionReady { session_id: SessionId, agent_label: String },
    /// A chunk of assistant text to append to the streaming buffer.
    AgentMessageChunk { text: String },
    /// A chunk of the agent's internal reasoning (rendered dimmed).
    AgentThoughtChunk { text: String },
    /// A tool call appeared or was replaced wholesale.
    ToolCall(ToolCallView),
    /// A tool call's status/content was updated (merged into the map).
    ToolCallUpdate(ToolCallView),
    /// The agent reports current token/cost usage.
    Usage(UsageView),
    /// The agent's plan was updated (full replacement). Rendered in the
    /// right-hand sidebar. Boxed because a plan can carry many steps and
    /// `UiEvent` travels over an mpsc channel — keeping the enum body
    /// small avoids copying a large `Vec` on every send of any variant.
    Plan(Box<PlanView>),
    /// The agent requests permission for a tool call; render a modal.
    RequestPermission(PendingPermission),
    /// The turn ended with this stop reason.
    Stopped(StopView),
    /// The connection to the backend closed (process exited / errored).
    ConnectionClosed { detail: String },
    /// An internal error from the client layer.
    Error(String),
}
