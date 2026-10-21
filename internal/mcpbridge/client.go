// Package mcpbridge adapts official MCP sessions to the local tool registry.
package mcpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/Lundomn/gopherharness/internal/tools"
	"github.com/Lundomn/gopherharness/internal/workspace"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Server struct {
	Name      string   `json:"name"`
	Transport string   `json:"transport"`
	URL       string   `json:"url"`
	Command   string   `json:"command"`
	Args      []string `json:"args"`
}
type Manager struct {
	sessions []*mcp.ClientSession
	once     sync.Once
	err      error
}

func Connect(ctx context.Context, registry *tools.Registry, servers []Server) (*Manager, error) {
	m := &Manager{}
	var definitions []tools.Definition
	names := map[string]bool{}
	fail := func(err error) (*Manager, error) { _ = m.Close(); return nil, err }
	for _, cfg := range servers {
		if cfg.Name == "" || names[cfg.Name] {
			return fail(errors.New("MCP names must be nonempty and unique"))
		}
		names[cfg.Name] = true
		var transport mcp.Transport
		switch cfg.Transport {
		case "stdio":
			if cfg.Command == "" {
				return fail(errors.New("MCP command required"))
			}
			cmd := exec.Command(cfg.Command, cfg.Args...)
			cmd.Env = workspace.ShellEnv()
			transport = &mcp.CommandTransport{Command: cmd, TerminateDuration: 3 * time.Second}
		case "sse":
			transport = &mcp.SSEClientTransport{Endpoint: cfg.URL, HTTPClient: &http.Client{}}
		case "streamableHttp":
			transport = &mcp.StreamableClientTransport{Endpoint: cfg.URL, HTTPClient: &http.Client{}, MaxRetries: -1}
		default:
			return fail(fmt.Errorf("unknown MCP transport %q", cfg.Transport))
		}
		client := mcp.NewClient(&mcp.Implementation{Name: "gopherharness", Version: "0.1.0"}, nil)
		session, err := client.Connect(ctx, transport, nil)
		if err != nil {
			return fail(err)
		}
		m.sessions = append(m.sessions, session)
		cursor := ""
		for {
			list, err := session.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
			if err != nil {
				return fail(err)
			}
			for _, remote := range list.Tools {
				schema, err := json.Marshal(remote.InputSchema)
				if err != nil {
					return fail(err)
				}
				name := "mcp_" + cfg.Name + "_" + remote.Name
				remoteName := remote.Name
				definitions = append(definitions, tools.Definition{Spec: tools.Spec{Name: name, Risky: true, Description: remote.Description, Schema: string(schema)}, Handler: func(callCtx context.Context, _ tools.Host, args map[string]any) (string, []string, error) {
					callCtx, cancel := context.WithTimeout(callCtx, 30*time.Second)
					defer cancel()
					result, err := session.CallTool(callCtx, &mcp.CallToolParams{Name: remoteName, Arguments: args})
					if err != nil {
						return "", nil, err
					}
					var text strings.Builder
					for _, content := range result.Content {
						if block, ok := content.(*mcp.TextContent); ok {
							text.WriteString(block.Text)
							text.WriteByte('\n')
						} else {
							data, _ := json.Marshal(content)
							text.Write(data)
							text.WriteByte('\n')
						}
					}
					if result.IsError {
						return "", nil, fmt.Errorf("MCP tool failed: %s", text.String())
					}
					return text.String(), nil, nil
				}})
			}
			if list.NextCursor == "" {
				break
			}
			if list.NextCursor == cursor {
				return fail(errors.New("MCP pagination did not advance"))
			}
			cursor = list.NextCursor
		}
	}
	if err := registry.RegisterAll(definitions); err != nil {
		return fail(err)
	}
	return m, nil
}
func (m *Manager) Close() error {
	m.once.Do(func() {
		for i := len(m.sessions) - 1; i >= 0; i-- {
			m.err = errors.Join(m.err, m.sessions[i].Close())
		}
	})
	return m.err
}
