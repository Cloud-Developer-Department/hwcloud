// Standalone ACP client — connects to any ACP agent via stdio.
//
//	go build -o build/acp-client ./examples/acp/client/
//	./build/acp-client -prompt "calculate 3.14 * 2"
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	openacp "github.com/Cloud-Developer-Department/hwcloud/acp/sdk"
)

func main() {
	serverBin := flag.String("server", "./build/acp-server", "path to ACP server binary")
	prompt := flag.String("prompt", "calculate 12 + 34", "prompt to send")
	flag.Parse()

	client := openacp.NewClient("acp-client", "1.0.0")
	session, err := client.ConnectStdio(context.Background(), nil, *serverBin)
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: connect to %s: %v\n", *serverBin, err)
		os.Exit(1)
	}
	defer session.Close()

	initResp, err := session.Initialize(context.Background(), openacp.InitializeRequest{
		ProtocolVersion: 1,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: initialize: %v\n", err)
		os.Exit(1)
	}
	agent := "unknown"
	if initResp.AgentInfo != nil {
		agent = initResp.AgentInfo.Name + "/" + initResp.AgentInfo.Version
	}
	fmt.Printf("connected to %s proto=%d\n\n", agent, initResp.ProtocolVersion)

	newResp, err := session.NewSession(context.Background(), openacp.NewSessionRequest{Cwd: "/tmp"})
	if err != nil {
		fmt.Fprintf(os.Stderr, "FATAL: new session: %v\n", err)
		os.Exit(1)
	}
	sid := newResp.SessionID

	session.SetEventHandler(&eventPrinter{})

	fmt.Printf("> %s\n\n", *prompt)
	_, err = session.Prompt(context.Background(), openacp.PromptRequest{
		SessionID: sid,
		Prompt:    []openacp.ContentBlock{{Type: "text", Text: *prompt}},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "ERROR: prompt: %v\n", err)
		os.Exit(1)
	}

	session.CloseSession(context.Background())
}

type eventPrinter struct{}

func (p *eventPrinter) OnAgentMessage(text string) { fmt.Print(text) }
func (p *eventPrinter) OnAgentThought(text string) { fmt.Printf("[thought] %s\n", text) }
func (p *eventPrinter) OnToolCall(tc openacp.ToolCallUpdate) {
	switch tc.Status {
	case "in_progress":
		fmt.Printf("[tool] %s(%v)\n", tc.Title, tc.RawInput)
	case "completed":
		fmt.Printf("[tool_result] %v\n", tc.RawOutput)
	case "failed":
		fmt.Printf("[tool_failed] %v\n", tc.RawOutput)
	}
}
func (p *eventPrinter) OnPlan(plan openacp.Plan)                                  {}
func (p *eventPrinter) OnAvailableCommandsUpdate(cmds []openacp.AvailableCommand) {}
func (p *eventPrinter) OnModeUpdate(modeID openacp.SessionModeId)                 {}
func (p *eventPrinter) OnConfigOptionUpdate(opts []openacp.SessionConfigOption)   {}
func (p *eventPrinter) OnUsageUpdate(used, total int, cost *openacp.Cost)         {}
func (p *eventPrinter) OnSessionInfo(title string, metadata map[string]any)       {}
func (p *eventPrinter) OnUserMessage(text string)                                 {}
