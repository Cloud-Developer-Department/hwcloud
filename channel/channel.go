// Package channel defines the interface for IM platform adapters.
//
// A Channel connects to an IM platform (Feishu, WeChat Work, DingTalk, etc.),
// receives incoming messages, normalizes them to IncomingMessage, and forwards
// them to a MessageHandler. Replies are sent back through ReplyFunc.
//
// Sub-packages (channel/feishu, channel/wecom, etc.) provide concrete
// implementations for each platform. The CLI layer wires channels to the
// Agent — Channel itself knows nothing about LLM, Memory, or sessions.
package channel

import (
	"context"
	"strings"
	"time"
)

// Channel is an IM platform adapter.
type Channel interface {
	// Name returns a human-readable label (e.g. "feishu", "wecom").
	Name() string

	// Start connects to the IM platform and begins forwarding incoming
	// messages to handler. It blocks until ctx is cancelled or the
	// connection is permanently lost.
	//
	// The handler is called in the same goroutine as the platform event
	// loop. Long-running work (e.g. agent.Run) MUST be launched in a
	// separate goroutine to avoid blocking the transport.
	Start(ctx context.Context, handler MessageHandler) error

	// Stop shuts down the channel. After Stop returns, no further calls
	// to the handler will be made (the platform event loop has exited).
	//
	// Contract caveat: Stop guarantees the EVENT LOOP stops; it does not
	// necessarily terminate the Start goroutine. Some platform SDKs (e.g.
	// the larksuite WebSocket client) block Start forever and ignore
	// both ctx cancellation and close — for those, Start's termination
	// is driven by its context, and Stop only tears the connection down.
	// Implementations document their specific behavior.
	Stop() error
}

// MessageHandler receives normalized incoming messages. Every call happens
// in the transport goroutine — the handler is responsible for spawning its
// own goroutines for heavyweight work.
type MessageHandler func(ctx context.Context, msg IncomingMessage, reply ReplyFunc)

// ReplyFunc sends a response back to the original chat and returns the
// platform-assigned message ID. If ReplyMessage.UpdateID is set and Card
// is non-nil, the channel updates the existing card rather than sending
// a new message.
type ReplyFunc func(ctx context.Context, msg ReplyMessage) (string, error)

// ReplyMessage carries the content to send back to an IM user.
// If Card is set the channel renders it as a platform-specific card;
// otherwise the message is sent as plain text.
//
// When UpdateID is set and Card is non-nil, the channel patches the
// existing card identified by UpdateID instead of creating a new message.
// Text is ignored when UpdateID is set.
type ReplyMessage struct {
	Text     string
	Card     *Card
	UpdateID string // update existing card instead of creating new message
}

// FoldMode controls how a card body is rendered on platforms that
// support collapsible content (e.g. Feishu collapsible_panel).
type FoldMode int

const (
	// FoldNone renders the body as plain markdown — always visible,
	// no fold affordance. This is the zero value and the most common
	// case (e.g. the run card's answer section).
	FoldNone FoldMode = iota
	// FoldCollapsed wraps the body in a collapsible panel that starts
	// collapsed (hidden behind a clickable bar).
	FoldCollapsed
	// FoldExpanded wraps the body in a collapsible panel that starts
	// expanded (visible, but the user can click to collapse).
	FoldExpanded
)

// Card is a platform-neutral structured message. Each channel translates
// it into the platform's native card format (e.g. Feishu interactive card,
// WeChat Work template card).
type Card struct {
	Header     CardHeader
	Content    string // markdown body
	Footer     string // optional note at the bottom
	Color      CardColor
	Fold       FoldMode        // how the body wraps: none, collapsed panel, or expanded panel
	Panels     []Card          // nested collapsed sub-panels (when non-empty, Content is ignored)
	Approval   *CardApproval   // optional approval section rendered at the bottom
	ModeSwitch *CardModeSwitch // optional mode-switch section rendered at the bottom
}

// Per-chat execution modes.
const (
	ModeManual = "manual"
	ModeAuto   = "auto"
)

// IsValidMode reports whether mode is a recognized execution mode.
func IsValidMode(mode string) bool {
	return mode == ModeManual || mode == ModeAuto
}

// CardModeSwitch carries the info needed to render a mode-switch button
// row inside a card. When set, BuildCard appends a separator and two
// action buttons (Manual / Auto) with the current mode highlighted.
// Button values carry {"type":"mode_switch","mode":"manual|auto"} so the
// card action callback can route mode-switch clicks separately.
type CardModeSwitch struct {
	CurrentMode string // ModeManual | ModeAuto
}

// CardApproval carries the info needed to render an approval button row
// inside a card. When set, BuildCard appends a separator, a context line
// (tool name + args), and three action buttons (always allow / allow / deny)
// to the card body. The ApprovalID is embedded in every button's value so
// the card action callback can correlate clicks back to the pending approval.
type CardApproval struct {
	ToolName   string
	Args       string
	ApprovalID string
}

// CardHeader is the title area of a card.
type CardHeader struct {
	Title    string
	Subtitle string
	// TitleMarkdown renders the title with lark_md (Feishu markdown)
	// instead of plain_text, allowing **bold** etc. Used by tool-call
	// panel titles to bold the first word.
	TitleMarkdown bool
}

// CardColor controls the header accent colour.
type CardColor string

const (
	CardColorBlue   CardColor = "blue"
	CardColorGreen  CardColor = "green"
	CardColorRed    CardColor = "red"
	CardColorYellow CardColor = "yellow"
	CardColorOrange CardColor = "orange"
	CardColorPurple CardColor = "purple"
	CardColorGrey   CardColor = "grey"
)

// IncomingMessage is a platform-neutral message received from an IM channel.
type IncomingMessage struct {
	ID        string    // platform message ID
	ChatID    string    // conversation identifier (group chat or private chat)
	ChatType  string    // "group" or "private"
	UserID    string    // sender identifier
	UserName  string    // sender display name
	Text      string    // plain text content (mentions stripped)
	Mentions  []string  // IDs mentioned in the message
	Timestamp time.Time // when the message was sent
	Raw       any       // original platform event, nil if not needed
}

// CodeBlock wraps content in a markdown code fence long enough to safely
// enclose any backtick run appearing inside content. Per CommonMark spec,
// a fence of N backticks cannot be closed by a run of fewer than N
// backticks, so inner ``` markers are rendered as literal text instead
// of prematurely terminating the block. This prevents markdown constructs
// inside tool output (e.g. tables in a SKILL.md) from leaking out and
// tripping platform card limits (Feishu ErrCode 11310).
func CodeBlock(content string) string {
	maxRun, run := 0, 0
	for _, c := range content {
		if c == '`' {
			run++
			if run > maxRun {
				maxRun = run
			}
		} else {
			run = 0
		}
	}
	n := 3
	if maxRun >= n {
		n = maxRun + 1
	}
	fence := strings.Repeat("`", n)
	return fence + "\n" + content + "\n" + fence
}
