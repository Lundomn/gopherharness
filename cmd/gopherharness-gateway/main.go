package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Lundomn/gopherharness/internal/config"
	"github.com/Lundomn/gopherharness/internal/feishu"
	"github.com/Lundomn/gopherharness/internal/gateway"
	"github.com/Lundomn/gopherharness/internal/gatewayapp"
	"github.com/Lundomn/gopherharness/internal/journal"
	"github.com/Lundomn/gopherharness/internal/provider"
	"github.com/Lundomn/gopherharness/internal/runtime"
	"github.com/Lundomn/gopherharness/internal/scheduler"
)

func main() { os.Exit(run()) }

func run() int {
	fs := flag.NewFlagSet("gopherharness-gateway", flag.ContinueOnError)
	root := fs.String("cwd", ".", "workspace directory")
	transport := fs.String("transport", "websocket", "Feishu websocket (official SDK) or encrypted webhook")
	allowTools := fs.String("allow-tools", "read_file,list_files,search", "explicit remote tool allowlist; dangerous tools still require approval")
	configPath := fs.String("config", "", "model configuration path")
	listen := fs.String("listen", "127.0.0.1:8080", "HTTP address; use a TLS reverse proxy for public callbacks")
	busy := fs.String("busy", "append", "append, inject, or interrupt")
	foreground := fs.Int("user-slots", 4, "maximum concurrent foreground turns")
	background := fs.Int("system-slots", 2, "maximum concurrent background turns")
	maxSteps := fs.Int("max-steps", 20, "maximum tool steps per turn")
	modelTimeout := fs.Int("model-timeout", 120, "model request timeout in seconds")
	shutdown := fs.Duration("shutdown-timeout", 30*time.Second, "graceful shutdown deadline")
	reconcile := fs.String("reconcile", "", "offline journal operation: list, delivered, confirmed_not_sent, abandon, recover_result; stop the running gateway first")
	reconcileKind := fs.String("record-kind", "outbox", "inbox or outbox for reconciliation")
	reconcileID := fs.String("record-id", "", "durable record ID, including channel prefix")
	actor := fs.String("operator", "", "operator identity for reconciliation audit")
	reason := fs.String("reason", "", "reason for reconciliation")
	evidence := fs.String("evidence", "", "external receipt/query or manual verification supporting the decision")
	receipt := fs.String("receipt", "", "confirmed platform message ID for delivered decision")
	resultFile := fs.String("result-file", "", "verified result text file for recover_result")
	if err := fs.Parse(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || *maxSteps < 1 || *modelTimeout < 1 || *shutdown <= 0 {
		log.Print("invalid arguments or non-positive limits")
		return 2
	}
	abs, err := filepath.Abs(*root)
	if err != nil {
		log.Print(err)
		return 1
	}
	if *reconcile != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		j, err := journal.Open(ctx, abs)
		if err != nil {
			log.Print(err)
			return 1
		}
		defer j.Close()
		if *reconcile == "list" {
			var unresolved []journal.Entry
			for _, pair := range [][2]string{{"inbox", "needs_reconcile"}, {"outbox", "unknown"}} {
				entries, err := j.List(ctx, pair[0], pair[1])
				if err != nil {
					log.Print(err)
					return 1
				}
				unresolved = append(unresolved, entries...)
			}
			if err := json.NewEncoder(os.Stdout).Encode(unresolved); err != nil {
				log.Print(err)
				return 1
			}
			return 0
		}
		var result []byte
		if *resultFile != "" {
			result, err = os.ReadFile(*resultFile)
			if err != nil {
				log.Print(err)
				return 1
			}
		}
		err = gateway.Reconcile(ctx, j, *reconcileKind, *reconcileID, journal.Resolution{Actor: *actor, Reason: *reason, Evidence: *evidence, Action: *reconcile, Receipt: *receipt}, string(result))
		if err != nil {
			log.Print(err)
			return 1
		}
		fmt.Println("reconciliation saved; only pending delivery is sent on the next gateway start")
		return 0
	}
	cfg, err := config.Load(abs, *configPath)
	if err != nil {
		log.Print(err)
		return 1
	}
	p, err := cfg.Selected()
	if err != nil || strings.TrimSpace(p.APIKey) == "" {
		log.Print("model provider and API key must be configured")
		return 1
	}
	client, err := provider.New(p, *modelTimeout)
	if err != nil {
		log.Print(err)
		return 1
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	startup, stopStartup := context.WithTimeout(ctx, 10*time.Second)
	defer stopStartup()
	application, err := gatewayapp.New(startup, gatewayapp.Config{
		Agent: runtime.Options{Root: abs, Config: cfg, Provider: client, MaxSteps: *maxSteps, AllowedTools: splitIDs(*allowTools)},
		Feishu: feishu.Config{AppID: os.Getenv("FEISHU_APP_ID"), AppSecret: os.Getenv("FEISHU_APP_SECRET"),
			VerificationToken: os.Getenv("FEISHU_VERIFICATION_TOKEN"), EncryptKey: os.Getenv("FEISHU_ENCRYPT_KEY"),
			BotOpenID: os.Getenv("FEISHU_BOT_OPEN_ID"), AllowedSenders: splitIDs(os.Getenv("FEISHU_ALLOWED_SENDERS")),
			AllowedChats: splitIDs(os.Getenv("FEISHU_ALLOWED_CHATS")), Transport: *transport, Busy: scheduler.Policy(*busy)},
		UserSlots: *foreground, SystemSlots: *background,
		OnError: func(err error) { log.Printf("gateway: %v", err) },
	})
	if err != nil {
		log.Print(err)
		return 1
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), *shutdown)
		defer stop()
		if err := application.Close(cleanup); err != nil {
			log.Printf("shutdown: %v", err)
		}
	}()
	var socketDone <-chan struct{}
	var socket *feishu.Socket
	if *transport == "websocket" {
		socket, err = application.StartSocket(ctx)
		if err != nil {
			log.Print(err)
			return 1
		}
		socketDone = socket.Done()
	}
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Print(err)
		return 1
	}
	server := &http.Server{Handler: application.Handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 32 << 10}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	fmt.Printf("GopherHarness gateway listening on %s; Feishu %s; tools require configured approval\n", listener.Addr(), *transport)
	select {
	case <-socketDone:
		socketErr := socket.Wait(context.Background())
		cancel()
		_ = server.Close()
		log.Printf("Feishu socket stopped: %v", socketErr)
		return 1
	case err = <-served:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Print(err)
			return 1
		}
	case <-ctx.Done():
		cleanup, stop := context.WithTimeout(context.Background(), *shutdown)
		defer stop()
		if err := server.Shutdown(cleanup); err != nil {
			_ = server.Close()
			log.Printf("HTTP shutdown: %v", err)
		}
	}
	return 0
}

func splitIDs(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}
