package wecom

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	hwcloud "github.com/Cloud-Developer-Department/hwcloud"
	"github.com/Cloud-Developer-Department/hwcloud/governance"
	opentool "github.com/Cloud-Developer-Department/hwcloud/tool"
)

// wecomApprover implements governance.HumanApprover for the WeCom
// channel. When the policy engine routes a tool call to the human layer,
// Ask sends a button_interaction template card to the WeCom chat and
// blocks until the user clicks a button (or the context is cancelled).
//
// Card action callbacks (button clicks) arrive via the WebSocket event
// loop as aibot_event_callback frames with eventtype=
// "template_card_event". The read loop dispatches these to
// handleCardEvent, which resolves the pending approval and sends the
// updated card via aibot_respond_update_msg (echoing the event
// callback's req_id).
type wecomApprover struct {
	ch     *Channel
	memory governance.ApprovalMemory

	mu      sync.Mutex
	pending map[string]*pendingApproval // approvalID → pending entry

	preApprovalMu sync.Mutex
	preApproval   func() // called before sending the approval card
}

// pendingApproval pairs a result channel with the tool info needed to
// build the resolved card after the user clicks.
type pendingApproval struct {
	result    chan approvalResult
	toolName  string
	args      string
	toolTitle string
	sessionID string
}

// approvalResult is the outcome extracted from a card action click.
type approvalResult struct {
	action string // "allow" | "deny"
	reason string // populated for deny
}

// newApprovalID generates a process-unique approval ID used as the
// card's task_id. Format: approval_<unix-nano>_<random-hex-4>. The
// timestamp guarantees uniqueness across restarts (the WeCom server
// rejects duplicate task_ids with errcode 42014).
func newApprovalID() string {
	var buf [4]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("approval_%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("approval_%d_%s", time.Now().UnixNano(), hex.EncodeToString(buf[:]))
}

// newWecomApprover creates an approver bound to the given channel.
func newWecomApprover(ch *Channel) *wecomApprover {
	return &wecomApprover{
		ch:      ch,
		pending: make(map[string]*pendingApproval),
	}
}

// Approver returns the approver as a governance.HumanApprover, setting
// the approval memory used by "allow_always" persistence.
func (c *Channel) Approver(mem governance.ApprovalMemory) governance.HumanApprover {
	c.approver.memory = mem
	return c.approver
}

// SetPreApprovalHook registers a callback invoked by Ask immediately
// before sending the approval card. The message handler uses this to
// finish the current streaming reply (finish=true), because WeCom
// rejects aibot_send_msg while a stream is open in the same chat
// (errcode 6000 "data version conflict").
func (c *Channel) SetPreApprovalHook(fn func()) {
	c.approver.preApprovalMu.Lock()
	c.approver.preApproval = fn
	c.approver.preApprovalMu.Unlock()
}

// Ask implements governance.HumanApprover. It sends an approval card to
// the WeCom chat and blocks until the user responds (or ctx is
// cancelled).
func (a *wecomApprover) Ask(ctx context.Context, call hwcloud.ToolCall, def hwcloud.FunctionDefinition, session hwcloud.Session) (governance.Decision, error) {
	if a.ch == nil {
		return governance.Decision{Action: governance.Deny, Reason: "approval channel not ready"}, nil
	}

	chatID := chatIDFromSession(session.ID)
	if chatID == "" {
		return governance.Decision{Action: governance.Deny, Reason: "cannot determine chat ID from session"}, nil
	}

	approvalID := newApprovalID()
	toolName := def.Name
	args := call.Function.Arguments
	toolTitle := opentool.ToolTitle(toolName, args)

	// Finish the current streaming reply before sending the approval
	// card — WeCom rejects aibot_send_msg while a stream is open
	// (errcode 6000 "data version conflict").
	a.preApprovalMu.Lock()
	hook := a.preApproval
	a.preApprovalMu.Unlock()
	if hook != nil {
		hook()
	}

	cardJSON, err := buildApprovalCard(approvalID, toolTitle)
	if err != nil {
		return governance.Decision{Action: governance.Deny, Reason: "build approval card: " + err.Error()}, nil
	}

	if err := a.ch.SendTemplateCard(chatID, cardJSON); err != nil {
		return governance.Decision{Action: governance.Deny, Reason: "send approval card: " + err.Error()}, nil
	}

	resultCh := make(chan approvalResult, 1)
	entry := &pendingApproval{
		result:    resultCh,
		toolName:  toolName,
		args:      args,
		toolTitle: toolTitle,
		sessionID: session.ID,
	}

	a.mu.Lock()
	a.pending[approvalID] = entry
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		delete(a.pending, approvalID)
		a.mu.Unlock()
	}()

	select {
	case <-ctx.Done():
		return governance.Decision{Action: governance.Deny, Reason: "approval cancelled: " + ctx.Err().Error()}, nil
	case r := <-resultCh:
		switch r.action {
		case "allow":
			return governance.Decision{Action: governance.Allow, Reason: "approved by user"}, nil
		default:
			return governance.Decision{Action: governance.Deny, Reason: r.reason}, nil
		}
	}
}

// handleCardEvent processes a template_card_event callback. It resolves
// the pending approval and sends the updated card via
// aibot_respond_update_msg (echoing the event callback's req_id). The
// WS event is acked with pong by the caller (handleFrame) before this
// is called.
//
// reqID is the event callback's req_id — echoed in the update frame.
func (a *wecomApprover) handleCardEvent(reqID string, ev *TemplateCardEvent) {
	if ev == nil || ev.TaskID == "" {
		slog.Warn("wecom: template card event missing task_id")
		return
	}

	approvalID := ev.TaskID

	a.mu.Lock()
	entry, ok := a.pending[approvalID]
	if ok {
		delete(a.pending, approvalID)
	}
	a.mu.Unlock()

	if !ok {
		slog.Warn("wecom: approval not found or already resolved", "approval_id", approvalID)
		return
	}

	var r approvalResult
	switch ev.EventKey {
	case "allow_once":
		r = approvalResult{action: "allow"}
	case "allow_always":
		r = approvalResult{action: "allow"}
		a.rememberAllowAlways(entry)
	case "deny":
		r = approvalResult{action: "deny", reason: "rejected by user"}
	default:
		slog.Warn("wecom: unknown approval action", "action", ev.EventKey)
		return
	}

	// Non-blocking send: Ask is waiting on this channel.
	select {
	case entry.result <- r:
	default:
	}

	// Send the updated card via aibot_respond_update_msg.
	resolvedCard, err := buildResolvedCard(approvalID, entry.toolTitle, r.action)
	if err != nil {
		slog.Warn("wecom: build resolved card", "error", err)
		return
	}
	if err := a.ch.UpdateTemplateCard(reqID, resolvedCard); err != nil {
		slog.Warn("wecom: update resolved card", "error", err)
	}
}

// rememberAllowAlways persists the approval decision for the session so
// the same tool+args won't ask again (ACP "Allow Always" semantics).
func (a *wecomApprover) rememberAllowAlways(entry *pendingApproval) {
	if a.memory == nil {
		return
	}
	d := governance.Decision{Action: governance.Allow}
	keys := governance.MemoryKeys(entry.toolName, json.RawMessage(entry.args))
	if len(keys) == 0 {
		keys = []string{governance.ApprovalKey(entry.toolName, json.RawMessage(entry.args))}
	}
	for _, key := range keys {
		if err := a.memory.Remember(context.Background(), entry.sessionID, key, d); err != nil {
			slog.Warn("wecom: approve always persistence failed", "session", entry.sessionID, "error", err)
		}
	}
}

// chatIDFromSession extracts the WeCom chat ID from a session ID.
// Channel session IDs follow the convention "wecom_<chatID>".
func chatIDFromSession(sessionID string) string {
	prefix := "wecom_"
	if strings.HasPrefix(sessionID, prefix) {
		return strings.TrimPrefix(sessionID, prefix)
	}
	return sessionID
}
