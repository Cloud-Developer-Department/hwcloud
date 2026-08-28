//! ACP client integration.
//!
//! Owns the connection lifecycle:
//! 1. Spawns the backend agent process via `AcpAgent::from_str("<bin> serve --acp")`.
//! 2. Builds a `Client` with a notification handler (translating every
//!    `SessionUpdate` variant into a `UiEvent`) and a request handler for
//!    `session/requestPermission` (blocks on a oneshot until the UI answers).
//! 3. Runs `initialize` + `session/new`, then drains the `Action` channel —
//!    sending prompts, issuing `session/cancel`, and answering permission
//!    requests — for the lifetime of the UI.
//!
//! The dispatch loop lives on this task; the UI only ever touches the
//! `mpsc::UnboundedSender<UiEvent>` (events out) and the
//! `mpsc::UnboundedReceiver<Action>` (commands in).

use std::path::PathBuf;
use std::str::FromStr;
use std::sync::Arc;

use agent_client_protocol::schema::ProtocolVersion;
use agent_client_protocol::schema::v1::{
    CancelNotification, ContentBlock, ContentChunk, InitializeRequest, NewSessionRequest,
    PermissionOptionId, PlanEntryPriority, PlanEntryStatus, PromptRequest, PromptResponse,
    RequestPermissionOutcome, RequestPermissionRequest, RequestPermissionResponse,
    SelectedPermissionOutcome, SessionNotification, SessionUpdate, StopReason, TextContent,
    ToolCallContent,
};
use agent_client_protocol::{
    AcpAgent, Agent, Client, ConnectionTo, Result, on_receive_notification, on_receive_request,
};
use futures::lock::Mutex;
use futures::channel::{mpsc, oneshot};
use futures::StreamExt;

use crate::app::Action;
use crate::event::{
    PendingPermission, PlanStepPriority, PlanStepStatus, PlanStepView, PlanView, StopView,
    ToolCallView, UiEvent, UsageView,
};

/// The backend binary name baked in at compile time by `build.sh`, mirroring
/// the Go `version.Name` (default `hwcloud`; a branded build injects e.g.
/// `myagent` via ldflags into Go and via `HWCLOUD_BINARY_NAME` into this
/// Rust build). main.rs forms the default launch command as
/// `{AGENT_BIN} serve --acp`; `--backend` overrides the whole command at
/// runtime.
///
/// `option_env!` yields an `Option<&str>` at compile time; `unwrap_or` is
/// not yet stable in `const` contexts, so we match instead.
pub const AGENT_BIN: &str = match option_env!("HWCLOUD_BINARY_NAME") {
    Some(name) => name,
    None => "hwcloud",
};

/// Channel the permission handler waits on for the UI's answer.
type PermissionReply = oneshot::Sender<Option<PermissionOptionId>>;

/// State shared between the permission request handler (on the dispatch
/// task) and the `Action` consumer that delivers the UI's answer.
struct PermissionBridge {
    /// Resolves with the user's chosen option id (`None` = cancel) once the
    /// UI sends an `AnswerPermission` action.
    pending: Mutex<Option<PermissionReply>>,
}

/// The boxed, pinned future returned by `send_request(PromptRequest).block_task()`.
/// Held in `in_flight` so it can be raced against incoming actions (notably
/// `Cancel`) instead of blocking the action loop.
type PromptFut = std::pin::Pin<Box<dyn std::future::Future<Output = Result<PromptResponse>> + Send>>;

/// Process one UI `Action`. `in_flight` receives the prompt future when a
/// `Prompt` is sent, so the caller can race it against the next action.
/// Returns `false` for `Quit` (caller stops the loop), `true` otherwise.
async fn handle_action(
    action: Action,
    cx: &ConnectionTo<Agent>,
    events_tx: &mpsc::UnboundedSender<UiEvent>,
    bridge: &Arc<PermissionBridge>,
    in_flight: &mut Option<PromptFut>,
) -> bool {
    match action {
        Action::Prompt { session_id: sid, text } => {
            let req = PromptRequest::new(
                sid,
                vec![ContentBlock::Text(TextContent::new(text))],
            );
            // Box the prompt future so it can be stored in `in_flight` and
            // raced against subsequent actions (cancel/permission/quit).
            *in_flight = Some(Box::pin(cx.send_request(req).block_task()));
        }
        Action::Cancel { session_id: sid } => {
            // `session/cancel` is a notification (fire-and-forget). Safe to
            // send while a prompt is in flight: it writes to the transport
            // directly, independent of the blocking prompt future.
            if let Err(e) = cx.send_notification(CancelNotification::new(sid)) {
                let _ = events_tx.unbounded_send(UiEvent::Error(format!("cancel: {e}")));
            }
        }
        Action::AnswerPermission { option_id } => {
            // Deliver the UI's decision to the waiting request handler. If
            // the turn was cancelled out from under us, `pending` is already
            // drained.
            let mut guard = bridge.pending.lock().await;
            if let Some(tx) = guard.take() {
                let _ = tx.send(option_id);
            }
        }
        Action::Quit => {
            // Dropping `cx` tears down the transport; the ChildGuard kills
            // the backend process group. Signal the caller to stop.
            return false;
        }
    }
    true
}

/// Run the full ACP client lifecycle. Drains `actions` until the UI quits
/// or the connection closes. Every notable state change is reported on
/// `events_tx`.
///
/// `command` is the full backend launch string as the SDK's
/// `AcpAgent::from_str` expects it — a shell-style command line such as
/// `hwcloud serve --acp`, `npx -y @zed-industries/claude-code-acp`, or
/// `ENV=val opencode acp`. The caller (main.rs) is responsible for
/// assembling it from `--backend` or the compile-time default
/// (`{AGENT_BIN} serve --acp`); this function does no further concatenation.
pub async fn run(
    events_tx: mpsc::UnboundedSender<UiEvent>,
    mut actions: mpsc::UnboundedReceiver<Action>,
    command: String,
) -> Result<()> {
    // Parse the spawn command up front so a bad command fails fast with a
    // clear message rather than a protocol-level error later.
    let agent = match AcpAgent::from_str(&command) {
        Ok(a) => a,
        Err(e) => {
            let _ = events_tx.unbounded_send(UiEvent::AgentSpawned {
                ok: false,
                detail: format!("parse command `{command}`: {e}"),
            });
            // Drain remaining actions so the UI's `Quit` is honored.
            while let Some(a) = actions.next().await {
                if matches!(a, Action::Quit) {
                    break;
                }
            }
            return Ok(());
        }
    };

    let bridge = Arc::new(PermissionBridge {
        pending: Mutex::new(None),
    });

    // The notification handler captures a clone of the event sender.
    let notif_tx = events_tx.clone();
    // The permission handler captures both the event sender (to show the
    // modal) and the bridge (to await the answer).
    let perm_tx = events_tx.clone();
    let perm_bridge = bridge.clone();
    // A clone retained for the post-connection `ConnectionClosed` send,
    // since `events_tx` itself is moved into the `connect_with` closure.
    let close_tx = events_tx.clone();

    let connection_fut = Client
        .builder()
        .on_receive_notification(
            async move |notification: SessionNotification, _cx| {
                translate_update(notification, &notif_tx);
                Ok(())
            },
            on_receive_notification!(),
        )
        .on_receive_request(
            async move |request: RequestPermissionRequest, responder, _cx| {
                handle_permission_request(request, responder, &perm_bridge, &perm_tx).await
            },
            on_receive_request!(),
        )
        .connect_with(agent, |cx: ConnectionTo<_>| async move {
            let _ = events_tx.unbounded_send(UiEvent::AgentSpawned {
                ok: true,
                detail: command.clone(),
            });

            // 1. Initialize.
            let init = match cx
                .send_request(InitializeRequest::new(ProtocolVersion::V1))
                .block_task()
                .await
            {
                Ok(r) => r,
                Err(e) => {
                    let _ = events_tx.unbounded_send(UiEvent::Error(format!("initialize: {e}")));
                    return Err(e);
                }
            };

            // 2. New session.
            let cwd = std::env::current_dir().unwrap_or_else(|_| PathBuf::from("/"));
            let new_session = match cx
                .send_request(NewSessionRequest::new(cwd))
                .block_task()
                .await
            {
                Ok(r) => r,
                Err(e) => {
                    let _ = events_tx.unbounded_send(UiEvent::Error(format!("session/new: {e}")));
                    return Err(e);
                }
            };
            let session_id = new_session.session_id;

            let agent_label = init
                .agent_info
                .as_ref()
                .map(|i| i.title.clone().unwrap_or_else(|| i.name.clone()))
                .unwrap_or_else(|| command_first_token(&command).to_string());
            let _ = events_tx.unbounded_send(UiEvent::SessionReady {
                session_id: session_id.clone(),
                agent_label,
            });

            // 3. Drain the action channel for the lifetime of the session.
            //
            // A prompt is an in-flight `send_request().block_task()` future.
            // We must NOT block the action loop on it: `session/cancel` and
            // permission answers arrive while the prompt is running, and
            // blocking would queue them until the prompt returns — making
            // cancel useless. So we hold the prompt future in `in_flight`
            // and `select!` between it and the next action each iteration.
            let mut in_flight: Option<PromptFut> = None;
            loop {
                if let Some(mut prompt_fut) = in_flight.take() {
                    // Prompt running: race it against the next action so a
                    // Cancel/AnswerPermission/Quit lands immediately.
                    //
                    // CRITICAL: `prompt_fut` must NOT be dropped while an
                    // action is being handled. Dropping a `SentRequest`
                    // future auto-sends `$/cancel_request` to the agent
                    // (SentRequestCancellation::drop), which cancels the
                    // in-flight prompt — so answering a permission request
                    // would cancel the very turn it's part of. We race the
                    // future by mutable reference so the action branch can
                    // stash the still-owned future back into `in_flight`.
                    tokio::select! {
                        biased;
                        res = &mut prompt_fut => {
                            match res {
                                Ok(resp) => {
                                    let _ = events_tx.unbounded_send(
                                        UiEvent::Stopped(stop_from(resp.stop_reason)),
                                    );
                                }
                                Err(e) => {
                                    let _ = events_tx
                                        .unbounded_send(UiEvent::Error(format!("prompt: {e}")));
                                    let _ = events_tx
                                        .unbounded_send(UiEvent::Stopped(StopView::Unknown));
                                }
                            }
                            continue;
                        }
                        maybe_action = actions.next() => {
                            let Some(action) = maybe_action else { break; };
                            // Stash the still-running prompt back BEFORE
                            // handling the action, so the future is not
                            // dropped at the end of this block.
                            // `handle_action` only overwrites `in_flight`
                            // for the Prompt variant (a new prompt); for
                            // Cancel/AnswerPermission/Quit it leaves the
                            // stashed future in place.
                            in_flight = Some(prompt_fut);
                            handle_action(
                                action, &cx, &events_tx, &bridge, &mut in_flight,
                            ).await;
                            continue;
                        }
                    }
                }

                // No prompt in flight: just wait for the next action.
                match actions.next().await {
                    None => break,
                    Some(action) => {
                        if !handle_action(action, &cx, &events_tx, &bridge, &mut in_flight).await {
                            break; // Quit
                        }
                    }
                }
            }

            Ok(())
        });

    let result = connection_fut.await;
    let detail = match &result {
        Ok(()) => "closed".to_string(),
        Err(e) => e.to_string(),
    };
    let _ = close_tx.unbounded_send(UiEvent::ConnectionClosed { detail });
    result
}

/// Translate a `SessionNotification` into zero or more `UiEvent`s.
///
/// Every variant the Go server emits (acp/server.go maps all 9 StreamEvent
/// types onto these) is handled here.
fn translate_update(notification: SessionNotification, tx: &mpsc::UnboundedSender<UiEvent>) {
    let SessionNotification { update, .. } = notification;
    match update {
        SessionUpdate::AgentMessageChunk(ContentChunk { content, .. }) => {
            if let ContentBlock::Text(t) = content {
                let _ = tx.unbounded_send(UiEvent::AgentMessageChunk { text: t.text });
            }
        }
        SessionUpdate::AgentThoughtChunk(ContentChunk { content, .. }) => {
            if let ContentBlock::Text(t) = content {
                let _ = tx.unbounded_send(UiEvent::AgentThoughtChunk { text: t.text });
            }
        }
        SessionUpdate::UserMessageChunk(_) => {
            // The agent echoes the user's own message; we already have it
            // in the transcript, so ignore.
        }
        SessionUpdate::ToolCall(call) => {
            let _ = tx.unbounded_send(UiEvent::ToolCall(tool_call_view(&call.title, &call)));
        }
        SessionUpdate::ToolCallUpdate(update) => {
            // A `ToolCallUpdate` may omit the title (it's a delta, not a
            // full call). Reconstruct a `ToolCall` from the update fields so
            // we can reuse the view builder; missing fields default.
            let title = update.fields.title.clone().unwrap_or_default();
            let synthesized = agent_client_protocol::schema::v1::ToolCall::new(
                update.tool_call_id.clone(),
                title,
            )
            .kind(update.fields.kind.unwrap_or_default())
            .status(update.fields.status.unwrap_or_default())
            .content(update.fields.content.clone().unwrap_or_default());
            let _ = tx.unbounded_send(UiEvent::ToolCallUpdate(tool_call_view(
                &synthesized.title,
                &synthesized,
            )));
        }
        SessionUpdate::UsageUpdate(u) => {
            let _ = tx.unbounded_send(UiEvent::Usage(UsageView {
                used: u.used,
                size: u.size,
                cost: u.cost,
            }));
        }
        SessionUpdate::Plan(plan) => {
            // Stable v1: the agent sends a *full replacement* of its plan on
            // every update. Translate to a `PlanView` and hand it to the UI,
            // which stores it whole and re-renders the sidebar.
            let steps: Vec<PlanStepView> = plan
                .entries
                .into_iter()
                .map(|e| PlanStepView {
                    content: e.content,
                    priority: match e.priority {
                        PlanEntryPriority::High => PlanStepPriority::High,
                        PlanEntryPriority::Medium => PlanStepPriority::Medium,
                        PlanEntryPriority::Low => PlanStepPriority::Low,
                        _ => PlanStepPriority::Medium,
                    },
                    status: match e.status {
                        PlanEntryStatus::Pending => PlanStepStatus::Pending,
                        PlanEntryStatus::InProgress => PlanStepStatus::InProgress,
                        PlanEntryStatus::Completed => PlanStepStatus::Completed,
                        _ => PlanStepStatus::Pending,
                    },
                })
                .collect();
            let _ = tx.unbounded_send(UiEvent::Plan(Box::new(PlanView { steps })));
        }
        SessionUpdate::AvailableCommandsUpdate(_)
        | SessionUpdate::CurrentModeUpdate(_)
        | SessionUpdate::ConfigOptionUpdate(_)
        | SessionUpdate::SessionInfoUpdate(_) => {
            // Not surfaced in the first version; safe to ignore.
        }
        // `PlanUpdate`/`PlanRemoved` are behind unstable features; the
        // non-exhaustive match covers any future variant as a no-op.
        _ => {}
    }
}

/// Build a `ToolCallView` from a full `ToolCall`.
fn tool_call_view(
    fallback_title: &str,
    call: &agent_client_protocol::schema::v1::ToolCall,
) -> ToolCallView {
    use agent_client_protocol::schema::v1::{ToolCallStatus, ToolKind};
    let status = match call.status {
        ToolCallStatus::Pending => "pending",
        ToolCallStatus::InProgress => "in_progress",
        ToolCallStatus::Completed => "completed",
        ToolCallStatus::Failed => "failed",
        _ => "unknown",
    };
    let kind = match call.kind {
        ToolKind::Read => "read",
        ToolKind::Edit => "edit",
        ToolKind::Delete => "delete",
        ToolKind::Move => "move",
        ToolKind::Search => "search",
        ToolKind::Execute => "execute",
        ToolKind::Think => "think",
        ToolKind::Fetch => "fetch",
        ToolKind::SwitchMode => "switch_mode",
        ToolKind::Other => "other",
        _ => "other",
    };
    ToolCallView {
        id: call.tool_call_id.to_string(),
        title: if call.title.is_empty() {
            fallback_title.to_string()
        } else {
            call.title.clone()
        },
        status: status.to_string(),
        kind: kind.to_string(),
        summary: summarize_tool_content(&call.content),
    }
}

/// Reduce a tool call's content blocks to a short text summary for the
/// status line. Picks the first text block; falls back to `None`.
fn summarize_tool_content(
    content: &[agent_client_protocol::schema::v1::ToolCallContent],
) -> Option<String> {
    for item in content {
        if let ToolCallContent::Content(c) = item {
            if let ContentBlock::Text(t) = &c.content {
                return Some(t.text.clone());
            }
        }
    }
    None
}

/// Convert a protocol `StopReason` into the UI's `StopView`.
fn stop_from(reason: StopReason) -> StopView {
    match reason {
        StopReason::EndTurn => StopView::EndTurn,
        StopReason::MaxTokens => StopView::MaxTokens,
        StopReason::MaxTurnRequests => StopView::MaxTurnRequests,
        StopReason::Refusal => StopView::Refusal,
        StopReason::Cancelled => StopView::Cancelled,
        _ => StopView::Unknown,
    }
}

/// Extract the first real command token from a launch string, skipping any
/// leading `ENV=val` assignments (the SDK's `from_args` accepts these). Used
/// only as a fallback label when the backend's `agent_info` is absent — e.g.
/// `ENV=val opencode acp` → `opencode`, `npx -y @zed/claude-code-acp` → `npx`.
/// A `Path`-style binary yields its file name: `/usr/local/bin/foo` → `foo`.
fn command_first_token(command: &str) -> &str {
    for part in command.split_whitespace() {
        // Skip environment-variable assignments that precede the command.
        if let Some(eq) = part.find('=') {
            let name = &part[..eq];
            if !name.is_empty()
                && name.chars().next().is_some_and(|c| c.is_ascii_alphabetic() || c == '_')
                && name.chars().all(|c| c.is_ascii_alphanumeric() || c == '_')
            {
                continue;
            }
        }
        // Strip a leading path so `/usr/local/bin/hwcloud` → `hwcloud`.
        return part
            .rsplit('/')
            .next()
            .filter(|s| !s.is_empty())
            .unwrap_or(part);
    }
    "agent"
}

/// The `session/requestPermission` handler.
///
/// The dispatch loop blocks on this future until it resolves, which is
/// exactly what we want: the agent waits for the permission response before
/// continuing the turn, so we park here until the UI answers (or the
/// responder is dropped — e.g. the connection closed — which we treat as
/// cancellation).
async fn handle_permission_request(
    request: RequestPermissionRequest,
    responder: agent_client_protocol::Responder<RequestPermissionResponse>,
    bridge: &PermissionBridge,
    events_tx: &mpsc::UnboundedSender<UiEvent>,
) -> Result<()> {
    let RequestPermissionRequest {
        session_id,
        tool_call,
        options,
        ..
    } = request;

    // Remember the offered option ids so we can validate the answer.
    let offered: Vec<PermissionOptionId> = options.iter().map(|o| o.option_id.clone()).collect();

    let (tx, rx) = oneshot::channel();
    {
        let mut guard = bridge.pending.lock().await;
        // Replace any stale pending request (defensive; shouldn't happen).
        *guard = Some(tx);
    }

    // Tell the UI to render the modal.
    let _ = events_tx.unbounded_send(UiEvent::RequestPermission(PendingPermission {
        session_id,
        tool_call,
        options,
    }));

    // Wait for the UI's answer. Validate the chosen id against the offered
    // set before building the outcome; anything else (including a dropped
    // sender when the connection closes) cancels the request.
    let outcome = match rx.await {
        Ok(Some(id)) if offered.contains(&id) => {
            RequestPermissionOutcome::Selected(SelectedPermissionOutcome::new(id))
        }
        _ => RequestPermissionOutcome::Cancelled,
    };

    responder.respond(RequestPermissionResponse::new(outcome))
}
