package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/runeQ/healthz/internal/alert"
	"github.com/runeQ/healthz/internal/check"
	"github.com/runeQ/healthz/internal/config"
	"github.com/runeQ/healthz/internal/monitor"
)

// version is set by release builds with -ldflags "-X main.version=vX.Y.Z".
var version = "dev"

// buildVersion reports the release version, or the module version when the
// binary was built with `go install module@version`.
func buildVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "token" {
		os.Exit(runTokenCommand(os.Args[2:], os.Stdout, os.Stderr))
	}

	configPath := flag.String("config", "config.json", "path to the JSON configuration file")
	checkConfig := flag.Bool("check-config", false, "validate configuration and exit")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Usage = func() {
		printMainUsage(flag.CommandLine.Output(), os.Args[0], flag.CommandLine)
	}
	flag.Parse()
	if *showVersion {
		fmt.Println("healthz", buildVersion())
		return
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{}))
	logger.Info("starting healthz", "version", buildVersion())
	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("configuration is invalid", "error", err)
		os.Exit(1)
	}
	if *checkConfig {
		fmt.Println("configuration is valid")
		return
	}
	bootstrapToken := os.Getenv(cfg.Server.AuthTokenEnv)
	if err := monitor.ValidateBootstrapToken(bootstrapToken); err != nil {
		logger.Error("API token environment variable is invalid", "variable", cfg.Server.AuthTokenEnv, "error", err)
		os.Exit(1)
	}
	if cfg.SMTP.Enabled && cfg.SMTP.Username != "" {
		if _, exists := os.LookupEnv(cfg.SMTP.PasswordEnv); !exists {
			logger.Error("SMTP password environment variable is not set", "variable", cfg.SMTP.PasswordEnv)
			os.Exit(1)
		}
	}
	if !cfg.SMTP.Enabled {
		logger.Warn("SMTP is disabled; alerts will be logged but no email will be delivered")
	}
	for _, variable := range cfg.RequiredEnv() {
		if value, exists := os.LookupEnv(variable); !exists || value == "" {
			logger.Error("required environment variable is not set", "variable", variable)
			os.Exit(1)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	status, err := monitor.OpenStatusStore(cfg.Storage.Path, cfg.Endpoints, cfg.Storage.HistoryRetentionDays)
	if err != nil {
		logger.Error("could not initialize persistent storage", "path", cfg.Storage.Path, "error", err)
		os.Exit(1)
	}
	defer func() {
		if err := status.Close(); err != nil {
			logger.Error("could not close persistent storage", "error", err)
		}
	}()
	hasDatabaseToken, err := status.HasUsableAPITokens(context.Background(), time.Now())
	if err != nil {
		logger.Error("could not inspect API tokens", "error", err)
		os.Exit(1)
	}
	if bootstrapToken == "" && !hasDatabaseToken {
		logger.Error("no usable API authentication token is configured",
			"bootstrap_variable", cfg.Server.AuthTokenEnv,
			"hint", "set the bootstrap variable or create a token with `healthz token create`")
		os.Exit(1)
	}
	pingKeys, err := monitor.NewPingKeys(cfg.Endpoints, os.LookupEnv)
	if err != nil {
		logger.Error("ping endpoint key is invalid", "error", err)
		os.Exit(1)
	}
	logger.Info("persistent storage ready", "path", cfg.Storage.Path, "history_retention_days", cfg.Storage.HistoryRetentionDays)
	emailSender := alert.NewEmailSender(cfg.SMTP)
	runner := monitor.NewRunner(cfg, check.NewRouter(status), emailSender, status, logger)
	routes := http.NewServeMux()
	routes.Handle("/ping/", status.PingHandler(pingKeys, runner.Wake, logger))
	routes.Handle("/", status.Handler(bootstrapToken, logger))
	server := &http.Server{
		Addr:              cfg.Server.Listen,
		Handler:           routes,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	serverErrors := make(chan error, 1)
	go func() {
		logger.Info("status server listening", "address", cfg.Server.Listen)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			serverErrors <- err
		}
		close(serverErrors)
	}()
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		runner.Run(ctx)
	}()
	go status.RunMaintenance(ctx, logger)
	if cfg.Digest.Enabled {
		go monitor.NewDigestRunner(cfg.Digest, status, emailSender, logger).Run(ctx)
	}

	select {
	case <-ctx.Done():
		logger.Info("shutting down")
	case err := <-serverErrors:
		if err != nil {
			logger.Error("status server failed", "error", err)
			stop()
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		logger.Error("status server shutdown failed", "error", err)
	}
	select {
	case <-monitorDone:
	case <-shutdownCtx.Done():
		logger.Warn("monitor workers did not stop before shutdown timeout")
	}
}

func printMainUsage(writer io.Writer, program string, flags *flag.FlagSet) {
	fmt.Fprintf(writer, `Usage:
  %s [options]
  %s token create -config PATH -name NAME [-email ADDRESS] [-expires-in DURATION | -expires-at RFC3339]
  %s token list   -config PATH
  %s token revoke -config PATH -id ID

Options:
`, program, program, program, program)
	flags.PrintDefaults()
	fmt.Fprintf(writer, `
Token commands:
  create  Issue a personal API token and display it once.
  list    List token metadata and status; secrets are never displayed.
  revoke  Immediately revoke a token by its public ID.

Run "%s token <command> -help" for command-specific options.
`, program)
}
