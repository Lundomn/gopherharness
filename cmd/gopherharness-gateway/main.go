package main

import (
	"context"
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
	"github.com/Lundomn/gopherharness/internal/gatewayapp"
	"github.com/Lundomn/gopherharness/internal/provider"
	"github.com/Lundomn/gopherharness/internal/runtime"
	"github.com/Lundomn/gopherharness/internal/scheduler"
)

func main() { os.Exit(run()) }

func run() int {
	fs := flag.NewFlagSet("gopherharness-gateway", flag.ContinueOnError)
	root := fs.String("cwd", ".", "workspace directory; remote tools are read-only")
	configPath := fs.String("config", "", "model configuration path")
	listen := fs.String("listen", "127.0.0.1:8080", "HTTP address; use a TLS reverse proxy for public callbacks")
	busy := fs.String("busy", "append", "append, inject, or interrupt")
	foreground := fs.Int("user-slots", 2, "maximum concurrent foreground turns")
	background := fs.Int("system-slots", 1, "maximum concurrent background turns")
	maxSteps := fs.Int("max-steps", 20, "maximum tool steps per turn")
	modelTimeout := fs.Int("model-timeout", 120, "model request timeout in seconds")
	shutdown := fs.Duration("shutdown-timeout", 30*time.Second, "graceful shutdown deadline")
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
		Agent: runtime.Options{Root: abs, Config: cfg, Provider: client, MaxSteps: *maxSteps},
		Feishu: feishu.Config{AppID: os.Getenv("FEISHU_APP_ID"), AppSecret: os.Getenv("FEISHU_APP_SECRET"),
			VerificationToken: os.Getenv("FEISHU_VERIFICATION_TOKEN"), EncryptKey: os.Getenv("FEISHU_ENCRYPT_KEY"),
			BotOpenID: os.Getenv("FEISHU_BOT_OPEN_ID"), AllowedSenders: strings.Split(os.Getenv("FEISHU_ALLOWED_SENDERS"), ","), Busy: scheduler.Policy(*busy)},
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
	listener, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Print(err)
		return 1
	}
	server := &http.Server{Handler: application.Handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 32 << 10}
	served := make(chan error, 1)
	go func() { served <- server.Serve(listener) }()
	fmt.Printf("GopherHarness gateway listening on %s (read-only tools)\n", listener.Addr())
	select {
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
