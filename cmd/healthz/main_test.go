package main

import (
	"bytes"
	"flag"
	"strings"
	"testing"
)

func TestMainHelpIncludesTokenCommands(t *testing.T) {
	var output bytes.Buffer
	flags := flag.NewFlagSet("healthz", flag.ContinueOnError)
	flags.SetOutput(&output)
	flags.Bool("check-config", false, "validate configuration and exit")
	flags.String("config", "config.json", "path to the JSON configuration file")
	printMainUsage(&output, "healthz", flags)

	for _, expected := range []string{
		"healthz token create",
		"healthz token list",
		"healthz token revoke",
		"-check-config",
		"-config string",
	} {
		if !strings.Contains(output.String(), expected) {
			t.Errorf("help output does not contain %q:\n%s", expected, output.String())
		}
	}
}
