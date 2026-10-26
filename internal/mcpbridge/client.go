// Package mcpbridge adapts official MCP sessions to the local tool registry.
package mcpbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	registry          *tools.Registry
	servers           map[string]*serverConnection
	connections       []*serverConnection
	ctx               context.Context
	cancel            context.CancelFunc
	mu                sync.Mutex
	closed, published bool
	calls             sync.WaitGroup
	admin             sync.WaitGroup
	adminGate         chan struct{}
	once              sync.Once
	done              chan struct{}
	err               error
}
type serverConnection struct {
	config    Server
	session   *mcp.ClientSession
	transport *ownedTransport
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	sealed    bool
	calls     sync.WaitGroup
	once      sync.Once
	done      chan struct{}
	err       error
}

// Retain the real SDK connection so transport-specific interfaces remain intact.
type ownedTransport struct {
	base    mcp.Transport
	conn    mcp.Connection
	command *exec.Cmd
}

func (t *ownedTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	conn, err := t.base.Connect(ctx)
	if err == nil {
		t.conn = conn
	}
	return conn, err
}

// DELETE precedes transport cancellation in the SDK. Bound only this shutdown
// request; a global HTTP timeout would incorrectly terminate long SSE streams.
type closingHTTPTransport struct{ base http.RoundTripper }

func (t closingHTTPTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodDelete {
		return t.base.RoundTrip(req)
	}
	ctx, cancel := context.WithTimeout(req.Context(), 2*time.Second)
	resp, err := t.base.RoundTrip(req.Clone(ctx))
	if err != nil {
		cancel()
		return nil, err
	}
	resp.Body = &cancelBody{ReadCloser: resp.Body, cancel: cancel}
	return resp, nil
}

type cancelBody struct {
	io.ReadCloser
	cancel context.CancelFunc
}

func (b *cancelBody) Close() error { defer b.cancel(); return b.ReadCloser.Close() }
func mcpHTTPClient() *http.Client {
	return &http.Client{Transport: closingHTTPTransport{base: http.DefaultTransport}}
}

func Connect(ctx context.Context, registry *tools.Registry, servers []Server) (*Manager, error) {
	if registry == nil {
		return nil, errors.New("MCP registry required")
	}
	life, cancel := context.WithCancel(context.Background())
	m := &Manager{registry: registry, servers: map[string]*serverConnection{}, ctx: life, cancel: cancel, adminGate: make(chan struct{}, 1), done: make(chan struct{})}
	fail := func(err error) (*Manager, error) { _ = m.Close(); return nil, err }
	updates := map[string][]tools.Definition{}
	for _, cfg := range servers {
		if cfg.Name == "" || strings.ContainsAny(cfg.Name, " \t\r\n") || m.servers[cfg.Name] != nil {
			return fail(errors.New("MCP server names must be nonempty and unique without whitespace"))
		}
		p, defs, err := m.open(ctx, cfg)
		if err != nil {
			return fail(err)
		}
		m.servers[cfg.Name] = p
		updates["mcp_"+cfg.Name] = defs
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if err := registry.ReplaceNamespaces(updates); err != nil {
		return fail(err)
	}
	m.published = true
	return m, nil
}

func (m *Manager) open(ctx context.Context, cfg Server) (*serverConnection, []tools.Definition, error) {
	life, cancel := context.WithCancel(m.ctx)
	p := &serverConnection{config: cfg, ctx: life, cancel: cancel, done: make(chan struct{})}
	m.mu.Lock()
	// Retain ownership only while cleanup is still running; repeated explicit
	// reconnects must not retain every closed SDK session for the process life.
	owned := m.connections[:0]
	for _, previous := range m.connections {
		select {
		case <-previous.done:
		default:
			owned = append(owned, previous)
		}
	}
	m.connections = owned
	m.connections = append(m.connections, p)
	m.mu.Unlock()
	var transport mcp.Transport
	var command *exec.Cmd
	switch cfg.Transport {
	case "stdio":
		if cfg.Command == "" {
			return p, nil, errors.New("MCP command required")
		}
		command = exec.Command(cfg.Command, cfg.Args...)
		command.Env = workspace.ShellEnv()
		ownProcessGroup(command)
		transport = &mcp.CommandTransport{Command: command, TerminateDuration: 3 * time.Second}
	case "sse":
		transport = &mcp.SSEClientTransport{Endpoint: cfg.URL, HTTPClient: mcpHTTPClient()}
	case "streamableHttp":
		transport = &mcp.StreamableClientTransport{Endpoint: cfg.URL, HTTPClient: mcpHTTPClient(), MaxRetries: -1}
	default:
		return p, nil, fmt.Errorf("unknown MCP transport %q", cfg.Transport)
	}
	p.transport = &ownedTransport{base: transport, command: command}
	client := mcp.NewClient(&mcp.Implementation{Name: "gopherharness", Version: "0.1.0"}, nil)
	session, err := client.Connect(ctx, p.transport, nil)
	if err != nil {
		return p, nil, err
	}
	p.session = session
	defs, err := m.discover(ctx, p)
	return p, defs, err
}
func (m *Manager) discover(ctx context.Context, p *serverConnection) ([]tools.Definition, error) {
	var definitions []tools.Definition
	cursor := ""
	seen := map[string]bool{}
	for page := 0; page < 100; page++ {
		list, err := p.session.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for _, remote := range list.Tools {
			if remote == nil || strings.TrimSpace(remote.Name) == "" {
				return nil, errors.New("MCP tool name required")
			}
			schema, err := json.Marshal(remote.InputSchema)
			if err != nil {
				return nil, err
			}
			if len(schema) > 1<<20 || len(definitions) >= 10000 {
				return nil, errors.New("MCP tool catalog exceeds configured size limit")
			}
			name := "mcp_" + p.config.Name + "_" + remote.Name
			definitions = append(definitions, tools.Definition{Spec: tools.Spec{Name: name, Risky: true, Description: remote.Description, Schema: string(schema)}, Handler: m.handler(p, remote.Name)})
		}
		if list.NextCursor == "" {
			return definitions, nil
		}
		if seen[list.NextCursor] || list.NextCursor == cursor {
			return nil, errors.New("MCP pagination did not advance")
		}
		seen[list.NextCursor] = true
		cursor = list.NextCursor
	}
	return nil, errors.New("MCP catalog exceeded 100 pages")
}
func (m *Manager) handler(p *serverConnection, remote string) tools.Handler {
	return func(ctx context.Context, _ tools.Host, args map[string]any) (string, []string, error) {
		m.mu.Lock()
		if m.closed {
			m.mu.Unlock()
			return "", nil, errors.New("MCP manager closed")
		}
		p.mu.Lock()
		if p.sealed {
			p.mu.Unlock()
			m.mu.Unlock()
			return "", nil, errors.New("MCP connection revoked; refresh before a new call")
		}
		p.calls.Add(1)
		m.calls.Add(1)
		p.mu.Unlock()
		m.mu.Unlock()
		defer p.calls.Done()
		defer m.calls.Done()
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		stop := context.AfterFunc(p.ctx, cancel)
		defer stop()
		result, err := p.session.CallTool(ctx, &mcp.CallToolParams{Name: remote, Arguments: args})
		if err != nil {
			return "", nil, fmt.Errorf("MCP call ended without a confirmed result; remote side effects may have occurred, reconcile before repeating a write: %w", err)
		}
		if result == nil {
			return "", nil, errors.New("MCP returned no result; reconcile before repeating a write")
		}
		var text strings.Builder
		for _, content := range result.Content {
			var part string
			if block, ok := content.(*mcp.TextContent); ok {
				part = block.Text
			} else {
				data, _ := json.Marshal(content)
				part = string(data)
			}
			// Bound retained model-facing text even when an external tool is verbose.
			remaining := (1 << 20) - text.Len()
			if len(part)+1 > remaining {
				if remaining > 64 {
					text.WriteString(part[:remaining-64])
				}
				text.WriteString("\n[MCP output truncated]\n")
				break
			}
			text.WriteString(part)
			text.WriteByte('\n')
		}
		if result.IsError {
			return "", nil, fmt.Errorf("MCP tool failed: %s", text.String())
		}
		return text.String(), nil, nil
	}
}

// adminContext serializes trusted refresh/reconnect requests, includes waiting
// for the gate in the 30s bound, and is cancelled when the owner closes.
func (m *Manager) adminContext(parent context.Context) (context.Context, func(), error) {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, nil, errors.New("MCP manager closed")
	}
	m.admin.Add(1)
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	stop := context.AfterFunc(m.ctx, cancel)
	release := func() { stop(); cancel(); m.admin.Done() }
	select {
	case m.adminGate <- struct{}{}:
		return ctx, func() { <-m.adminGate; release() }, nil
	case <-ctx.Done():
		release()
		return nil, nil, ctx.Err()
	}
}

// Refresh is called by a trusted host after a directory-change notification or
// an operator action. It is not an LLM tool, and never expands the allowlist.
// Failure retains the previous complete snapshot instead of a partial catalog.
func (m *Manager) Refresh(parent context.Context, name string) error {
	ctx, done, err := m.adminContext(parent)
	if err != nil {
		return err
	}
	defer done()
	p := m.servers[name]
	if p == nil {
		return errors.New("unknown MCP server")
	}
	p.mu.Lock()
	sealed := p.sealed
	p.mu.Unlock()
	if sealed {
		return errors.New("MCP connection unavailable; explicit reconnect required")
	}
	defs, err := m.discover(ctx, p)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || ctx.Err() != nil {
		return errors.New("MCP refresh cancelled before publication")
	}
	return m.registry.ReplaceNamespace("mcp_"+name, defs)
}

// Reconnect revokes new admissions, cancels/joins the old connection, then opens
// a bounded number of new connections. It NEVER replays interrupted tool calls.
// Any old uncertain write must be reconciled before the caller issues a new one.
func (m *Manager) Reconnect(parent context.Context, name string, attempts int) error {
	if attempts < 1 || attempts > 3 {
		return errors.New("MCP reconnect attempts must be in [1,3]")
	}
	ctx, done, err := m.adminContext(parent)
	if err != nil {
		return err
	}
	defer done()
	old := m.servers[name]
	if old == nil {
		return errors.New("unknown MCP server")
	}
	if err = m.registry.ReplaceNamespace("mcp_"+name, nil); err != nil {
		return err
	}
	if err = old.close(ctx); err != nil {
		select {
		case <-old.done:
			// A crashed server can have a nonzero exit status after every
			// resource is joined. That must not permanently block replacement.
		default:
			return fmt.Errorf("old MCP connection still owns resources: %w", err)
		}
	}
	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		p, defs, err := m.open(ctx, old.config)
		if err == nil {
			m.mu.Lock()
			if m.closed || ctx.Err() != nil {
				err = errors.New("MCP reconnect cancelled before publication")
			} else {
				err = m.registry.ReplaceNamespace("mcp_"+name, defs)
				if err == nil {
					m.servers[name] = p
				}
			}
			m.mu.Unlock()
			if err == nil {
				return nil
			}
		}
		last = err
		_ = p.close(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if attempt+1 < attempts {
			timer := time.NewTimer(time.Duration(1<<attempt) * 100 * time.Millisecond)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			}
		}
	}
	return fmt.Errorf("MCP reconnect exhausted without replaying calls: %w", last)
}
func (p *serverConnection) close(ctx context.Context) error {
	p.once.Do(func() {
		p.mu.Lock()
		p.sealed = true
		p.cancel()
		p.mu.Unlock()
		go func() {
			callsDone := make(chan struct{})
			go func() { p.calls.Wait(); close(callsDone) }()
			timer := time.NewTimer(250 * time.Millisecond)
			select {
			case <-callsDone:
				timer.Stop()
			case <-timer.C:
			}
			var wg sync.WaitGroup
			var mu sync.Mutex
			join := func(err error) { mu.Lock(); p.err = errors.Join(p.err, err); mu.Unlock() }
			if p.transport != nil && p.transport.conn != nil {
				wg.Add(1)
				go func() {
					defer wg.Done()
					var reap *time.Timer
					if p.transport.command != nil {
						reap = time.AfterFunc(2*time.Second, func() { killOwnedProcessGroup(p.transport.command) })
					}
					err := p.transport.conn.Close()
					if reap != nil {
						reap.Stop()
						killOwnedProcessGroup(p.transport.command)
					}
					join(err)
				}()
			}
			if p.session != nil {
				wg.Add(1)
				go func() { defer wg.Done(); join(p.session.Close()) }()
			}
			<-callsDone
			wg.Wait()
			close(p.done)
		}()
	})
	select {
	case <-p.done:
		return p.err
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (m *Manager) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return m.CloseContext(ctx)
}

// A waiter's timeout does not release the owner: later CloseContext joins the
// same cleanup, including SDK workers, stdio processes and cancelled calls.
func (m *Manager) CloseContext(ctx context.Context) error {
	m.once.Do(func() {
		m.mu.Lock()
		m.closed = true
		m.cancel()
		m.mu.Unlock()
		go func() {
			m.admin.Wait()
			if m.published {
				updates := map[string][]tools.Definition{}
				for name := range m.servers {
					updates["mcp_"+name] = nil
				}
				m.err = m.registry.ReplaceNamespaces(updates)
			}
			var wg sync.WaitGroup
			var mu sync.Mutex
			for _, p := range m.connections {
				wg.Add(1)
				go func() {
					defer wg.Done()
					err := p.close(context.Background())
					mu.Lock()
					m.err = errors.Join(m.err, err)
					mu.Unlock()
				}()
			}
			m.calls.Wait()
			wg.Wait()
			close(m.done)
		}()
	})
	select {
	case <-m.done:
		return m.err
	case <-ctx.Done():
		return ctx.Err()
	}
}
