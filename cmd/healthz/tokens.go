package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/qbite-tech/healthz/internal/config"
	"github.com/qbite-tech/healthz/internal/monitor"
)

func runTokenCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		printTokenUsage(stderr)
		return 2
	}
	switch args[0] {
	case "create":
		return runTokenCreate(args[1:], stdout, stderr)
	case "list":
		return runTokenList(args[1:], stdout, stderr)
	case "revoke":
		return runTokenRevoke(args[1:], stdout, stderr)
	case "help", "-h", "--help":
		printTokenUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "unknown token command %q\n\n", args[0])
		printTokenUsage(stderr)
		return 2
	}
}

func runTokenCreate(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("token create", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "config.json", "path to the JSON configuration file")
	name := flags.String("name", "", "human-readable token owner or device name")
	email := flags.String("email", "", "optional owner email")
	expiresIn := flags.String("expires-in", "", "optional lifetime such as 24h, 30d, or 2w")
	expiresAtText := flags.String("expires-at", "", "optional RFC3339 expiry timestamp")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "token create does not accept positional arguments")
		return 2
	}
	now := time.Now()
	expiresAt, err := resolveTokenExpiry(*expiresIn, *expiresAtText, now)
	if err != nil {
		fmt.Fprintf(stderr, "invalid token expiry: %v\n", err)
		return 2
	}
	store, err := openTokenStore(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "could not open token database: %v\n", err)
		return 1
	}
	defer store.Close()
	created, err := store.CreateAPIToken(context.Background(), *name, *email, expiresAt, now)
	if err != nil {
		fmt.Fprintf(stderr, "could not create token: %v\n", err)
		return 1
	}
	fmt.Fprintln(stdout, created.Plaintext)
	fmt.Fprintf(stderr, "created API token %s for %s", created.ID, created.Name)
	if created.Email != "" {
		fmt.Fprintf(stderr, " <%s>", created.Email)
	}
	if created.ExpiresAt == nil {
		fmt.Fprintln(stderr, "; it does not expire")
	} else {
		fmt.Fprintf(stderr, "; expires %s\n", created.ExpiresAt.Format(time.RFC3339))
	}
	fmt.Fprintln(stderr, "the token is shown once; store it securely")
	return 0
}

func runTokenList(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("token list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "config.json", "path to the JSON configuration file")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "token list does not accept positional arguments")
		return 2
	}
	store, err := openTokenStore(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "could not open token database: %v\n", err)
		return 1
	}
	defer store.Close()
	tokens, err := store.ListAPITokens(context.Background())
	if err != nil {
		fmt.Fprintf(stderr, "could not list tokens: %v\n", err)
		return 1
	}
	writer := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "ID\tNAME\tEMAIL\tCREATED\tEXPIRES\tSTATUS\tLAST USED")
	now := time.Now()
	for _, token := range tokens {
		status := "active"
		if token.RevokedAt != nil {
			status = "revoked"
		} else if token.ExpiresAt != nil && !token.ExpiresAt.After(now) {
			status = "expired"
		}
		fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", token.ID, token.Name,
			displayValue(token.Email), token.CreatedAt.Format(time.RFC3339), displayTime(token.ExpiresAt, "never"),
			status, displayTime(token.LastUsedAt, "never"))
	}
	if err := writer.Flush(); err != nil {
		fmt.Fprintf(stderr, "could not write token list: %v\n", err)
		return 1
	}
	return 0
}

func runTokenRevoke(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("token revoke", flag.ContinueOnError)
	flags.SetOutput(stderr)
	configPath := flags.String("config", "config.json", "path to the JSON configuration file")
	id := flags.String("id", "", "public token ID")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 || strings.TrimSpace(*id) == "" {
		fmt.Fprintln(stderr, "token revoke requires -id and does not accept positional arguments")
		return 2
	}
	store, err := openTokenStore(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "could not open token database: %v\n", err)
		return 1
	}
	defer store.Close()
	if err := store.RevokeAPIToken(context.Background(), *id, time.Now()); err != nil {
		if errors.Is(err, monitor.ErrAPITokenNotFound) {
			fmt.Fprintf(stderr, "token %s was not found or is already revoked\n", *id)
		} else {
			fmt.Fprintf(stderr, "could not revoke token: %v\n", err)
		}
		return 1
	}
	fmt.Fprintf(stdout, "revoked API token %s\n", strings.TrimSpace(*id))
	return 0
}

func openTokenStore(configPath string) (*monitor.StatusStore, error) {
	cfg, err := config.Load(configPath)
	if err != nil {
		return nil, err
	}
	return monitor.OpenTokenStore(cfg.Storage.Path)
}

func resolveTokenExpiry(expiresIn, expiresAtText string, now time.Time) (*time.Time, error) {
	if expiresIn != "" && expiresAtText != "" {
		return nil, errors.New("use either -expires-in or -expires-at, not both")
	}
	if expiresIn != "" {
		duration, err := parseTokenLifetime(expiresIn)
		if err != nil {
			return nil, err
		}
		expiresAt := now.Add(duration).UTC()
		if !expiresAt.After(now) {
			return nil, errors.New("lifetime is too large")
		}
		return &expiresAt, nil
	}
	if expiresAtText != "" {
		expiresAt, err := time.Parse(time.RFC3339, expiresAtText)
		if err != nil {
			return nil, errors.New("-expires-at must use RFC3339, for example 2026-12-31T23:59:59Z")
		}
		if !expiresAt.After(now) {
			return nil, errors.New("expiry must be in the future")
		}
		expiresAt = expiresAt.UTC()
		return &expiresAt, nil
	}
	return nil, nil
}

func parseTokenLifetime(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, errors.New("lifetime cannot be empty")
	}
	for suffix, unit := range map[string]time.Duration{"d": 24 * time.Hour, "w": 7 * 24 * time.Hour} {
		if strings.HasSuffix(value, suffix) {
			count, err := strconv.ParseUint(strings.TrimSuffix(value, suffix), 10, 32)
			if err != nil || count == 0 || count > uint64((time.Duration(1<<63-1))/unit) {
				return 0, fmt.Errorf("invalid lifetime %q", value)
			}
			return time.Duration(count) * unit, nil
		}
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("invalid lifetime %q; use a positive duration such as 24h, 30d, or 2w", value)
	}
	return duration, nil
}

func displayTime(value *time.Time, fallback string) string {
	if value == nil {
		return fallback
	}
	return value.Format(time.RFC3339)
}

func displayValue(value string) string {
	if value == "" {
		return "-"
	}
	return value
}

func printTokenUsage(writer io.Writer) {
	fmt.Fprintln(writer, `Usage:
  healthz token create -config PATH -name NAME [-email ADDRESS] [-expires-in DURATION | -expires-at RFC3339]
  healthz token list   -config PATH
  healthz token revoke -config PATH -id ID`)
}
