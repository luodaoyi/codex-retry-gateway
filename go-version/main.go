package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

func main() {
	if err := run(os.Args); err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
}

func run(argv []string) error {
	command, options, err := parseCLI(argv)
	if err != nil {
		return err
	}

	switch command {
	case "serve":
		return runServe(options)
	case "start":
		return runStart(options)
	case "stop":
		return runStop(options)
	case "install":
		return runInstall(options)
	case "restore":
		return runRestore(options)
	case "launch-ui":
		return runLaunchUI(options)
	case "help", "--help", "-h":
		printHelp()
		return nil
	default:
		return fmt.Errorf("unknown command: %s", command)
	}
}

type cliOptions struct {
	Args map[string]string
	Bool map[string]bool
	Rest []string
}

func parseCLI(argv []string) (string, cliOptions, error) {
	command := "launch-ui"
	start := 1
	if len(argv) > 1 && !strings.HasPrefix(argv[1], "-") {
		command = argv[1]
		start = 2
	}

	options := cliOptions{
		Args: map[string]string{},
		Bool: map[string]bool{},
	}
	booleanFlags := map[string]bool{
		"restart-if-running": true,
		"no-open":            true,
		"quiet":              true,
	}

	for index := start; index < len(argv); index++ {
		current := argv[index]
		if !strings.HasPrefix(current, "--") {
			options.Rest = append(options.Rest, current)
			continue
		}

		name := strings.TrimPrefix(current, "--")
		if booleanFlags[name] {
			options.Bool[name] = true
			continue
		}

		if index+1 >= len(argv) {
			return "", cliOptions{}, fmt.Errorf("missing value for --%s", name)
		}
		options.Args[name] = argv[index+1]
		index++
	}

	return command, options, nil
}

func printHelp() {
	fmt.Println(`codex-retry-gateway

Usage:
  codex-retry-gateway [launch-ui] [--codex-config-path PATH] [--state-root PATH] [--listen-host HOST] [--listen-port PORT] [--no-open]
  codex-retry-gateway install [--codex-config-path PATH] [--state-root PATH] [--listen-host HOST] [--listen-port PORT]
  codex-retry-gateway start [--state-root PATH] [--config-path PATH] [--log-path PATH] [--restart-if-running]
  codex-retry-gateway stop [--state-root PATH] [--quiet]
  codex-retry-gateway restore [--state-root PATH] [--codex-config-path PATH]
  codex-retry-gateway serve [--config PATH] [--log PATH]
`)
}

func requiredArg(options cliOptions, key string, fallback string) string {
	if value := strings.TrimSpace(options.Args[key]); value != "" {
		return value
	}
	return fallback
}

func intArg(options cliOptions, key string, fallback int) (int, error) {
	if value := strings.TrimSpace(options.Args[key]); value != "" {
		parsed, err := parsePositiveInt(value)
		if err != nil {
			return 0, fmt.Errorf("invalid --%s: %w", key, err)
		}
		return parsed, nil
	}
	return fallback, nil
}

func parsePositiveInt(raw string) (int, error) {
	value, err := parseInt(raw)
	if err != nil {
		return 0, err
	}
	if value <= 0 {
		return 0, errors.New("must be greater than 0")
	}
	return value, nil
}
