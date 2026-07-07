package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"
)

const (
	installModeProvider = "provider_base_url"
	installModeNative   = "native_openai_oauth"
	managedProviderName = "codex_retry_gateway_openai"
	defaultOpenAIURL    = "https://api.openai.com"
)

type providerContext struct {
	Content         string
	ProviderName    string
	SectionText     string
	SectionIndex    int
	SectionLength   int
	CurrentBaseURL  string
	BaseURLLineText string
}

type installState struct {
	InstalledAt         string `json:"installed_at"`
	InstallMode         string `json:"install_mode"`
	CodexConfigPath     string `json:"codex_config_path"`
	ProviderName        string `json:"provider_name,omitempty"`
	ManagedProviderName string `json:"managed_provider_name,omitempty"`
	OriginalBaseURL     string `json:"original_base_url"`
	GatewayBaseURL      string `json:"gateway_base_url"`
	GatewayConfigPath   string `json:"gateway_config_path"`
	GatewayLogPath      string `json:"gateway_log_path"`
	GatewayPIDPath      string `json:"gateway_pid_path"`
	LatestBackupPath    string `json:"latest_backup_path"`
	StateRoot           string `json:"state_root"`
}

func getCodexProviderContext(codexConfigPath string) (*providerContext, error) {
	contentBytes, err := os.ReadFile(codexConfigPath)
	if err != nil {
		return nil, err
	}
	content := string(contentBytes)
	providerMatch := regexp.MustCompile(`(?m)^\s*model_provider\s*=\s*"([^"]+)"\s*$`).FindStringSubmatchIndex(content)
	if providerMatch == nil {
		return nil, fmt.Errorf("model_provider was not found in %s", codexConfigPath)
	}

	providerName := content[providerMatch[2]:providerMatch[3]]
	sectionText, startIndex, endIndex := extractProviderSection(content, providerName)
	if sectionText == "" {
		return nil, fmt.Errorf("[model_providers.%s] was not found in %s", providerName, codexConfigPath)
	}
	baseURLMatch := regexp.MustCompile(`(?m)^\s*base_url\s*=\s*"([^"]+)"\s*$`).FindStringSubmatch(sectionText)
	if baseURLMatch == nil {
		return nil, fmt.Errorf("base_url was not found in [model_providers.%s]", providerName)
	}

	return &providerContext{
		Content:         content,
		ProviderName:    providerName,
		SectionText:     sectionText,
		SectionIndex:    startIndex,
		SectionLength:   endIndex - startIndex,
		CurrentBaseURL:  baseURLMatch[1],
		BaseURLLineText: baseURLMatch[0],
	}, nil
}

func extractProviderBaseURL(content string, providerName string) string {
	if strings.TrimSpace(content) == "" || strings.TrimSpace(providerName) == "" {
		return ""
	}
	section, _, _ := extractProviderSection(content, providerName)
	if section == "" {
		return ""
	}
	match := regexp.MustCompile(`(?m)^\s*base_url\s*=\s*"([^"]+)"\s*$`).FindStringSubmatch(section)
	if match == nil {
		return ""
	}
	return match[1]
}

func extractProviderBooleanSetting(content string, providerName string, key string) bool {
	if providerName == "" || key == "" {
		return false
	}
	section, _, _ := extractProviderSection(content, providerName)
	if section == "" {
		return false
	}
	match := regexp.MustCompile(`(?mi)^\s*` + regexp.QuoteMeta(key) + `\s*=\s*(true|false)\s*$`).FindStringSubmatch(section)
	return match != nil && strings.EqualFold(match[1], "true")
}

func setCodexProviderBaseURL(codexConfigPath string, providerName string, newBaseURL string) error {
	context, err := getCodexProviderContext(codexConfigPath)
	if err != nil {
		return err
	}
	if context.ProviderName != providerName {
		return fmt.Errorf("model_provider changed unexpectedly: expected %s, actual %s", providerName, context.ProviderName)
	}

	updatedSection := regexp.MustCompile(`(?m)^(\s*base_url\s*=\s*")([^"]*)("\s*)$`).ReplaceAllString(context.SectionText, `${1}`+newBaseURL+`${3}`)
	updatedContent := context.Content[:context.SectionIndex] + updatedSection + context.Content[context.SectionIndex+context.SectionLength:]
	return os.WriteFile(codexConfigPath, []byte(updatedContent), 0o644)
}

func detectInstallMode(codexConfigPath string) (mode string, providerName string, originalBaseURL string, err error) {
	context, contextErr := getCodexProviderContext(codexConfigPath)
	if contextErr == nil {
		return installModeProvider, context.ProviderName, context.CurrentBaseURL, nil
	}
	contentBytes, err := os.ReadFile(codexConfigPath)
	if err != nil {
		return "", "", "", err
	}
	content := string(contentBytes)
	if strings.Contains(contextErr.Error(), "model_provider was not found") {
		return installModeNative, "", defaultOpenAIURL, nil
	}

	// Provider configured but no base_url. Treat it like native OpenAI auth if it still requests OpenAI auth.
	providerMatch := regexp.MustCompile(`(?m)^\s*model_provider\s*=\s*"([^"]+)"\s*$`).FindStringSubmatch(content)
	if providerMatch != nil {
		providerName = providerMatch[1]
		if extractProviderBooleanSetting(content, providerName, "requires_openai_auth") {
			return installModeNative, providerName, defaultOpenAIURL, nil
		}
	}
	return "", "", "", contextErr
}

func ensureGatewayProvider(content string, gatewayBaseURL string) string {
	managedSection := strings.Join([]string{
		fmt.Sprintf(`[model_providers.%s]`, managedProviderName),
		`name = "OpenAI"`,
		fmt.Sprintf(`base_url = "%s"`, gatewayBaseURL),
		`requires_openai_auth = true`,
		`wire_api = "responses"`,
		`request_max_retries = 100`,
		`stream_max_retries = 20`,
		`stream_idle_timeout_ms = 300000`,
		"",
	}, "\n")

	result := content
	if regexp.MustCompile(`(?m)^\s*model_provider\s*=`).MatchString(result) {
		result = regexp.MustCompile(`(?m)^\s*model_provider\s*=\s*"[^"]+"\s*$`).ReplaceAllString(result, fmt.Sprintf(`model_provider = "%s"`, managedProviderName))
	} else {
		result = fmt.Sprintf("model_provider = \"%s\"\n%s", managedProviderName, result)
	}

	sectionText, startIndex, endIndex := extractProviderSection(result, managedProviderName)
	if sectionText != "" {
		result = result[:startIndex] + strings.TrimSuffix(managedSection, "\n") + result[endIndex:]
		if !strings.HasSuffix(result, "\n") {
			result += "\n"
		}
		return result
	}

	if !strings.HasSuffix(result, "\n") {
		result += "\n"
	}
	if !strings.HasSuffix(result, "\n\n") {
		result += "\n"
	}
	return result + managedSection
}

func extractProviderSection(content string, providerName string) (section string, startIndex int, endIndex int) {
	if strings.TrimSpace(content) == "" || strings.TrimSpace(providerName) == "" {
		return "", 0, 0
	}
	lines := strings.SplitAfter(content, "\n")
	header := fmt.Sprintf("[model_providers.%s]", providerName)
	builder := strings.Builder{}
	offset := 0
	found := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if !found {
			if trimmed == header {
				found = true
				startIndex = offset
				builder.WriteString(line)
			}
			offset += len(line)
			continue
		}
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			endIndex = offset
			return builder.String(), startIndex, endIndex
		}
		builder.WriteString(line)
		offset += len(line)
	}
	if !found {
		return "", 0, 0
	}
	endIndex = len(content)
	return builder.String(), startIndex, endIndex
}

func readInstallState(statePath string) (*installState, error) {
	content, err := os.ReadFile(statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var state installState
	if err := json.Unmarshal(content, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func writeInstallState(statePath string, state installState) error {
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return os.WriteFile(statePath, body, 0o644)
}

type installOptions struct {
	CodexConfigPath string
	StateRoot       string
	ListenHost      string
	ListenPort      int
}

func installGateway(options installOptions) (installState, gatewayConfig, error) {
	paths := buildGatewayPaths(options.StateRoot)
	for _, dir := range []string{paths.StateRoot, paths.ConfigDir, paths.LogDir, paths.BackupDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return installState{}, gatewayConfig{}, err
		}
	}

	if _, err := os.Stat(options.CodexConfigPath); err != nil {
		return installState{}, gatewayConfig{}, fmt.Errorf("Codex config file was not found: %s", options.CodexConfigPath)
	}

	mode, providerName, originalBaseURL, err := detectInstallMode(options.CodexConfigPath)
	if err != nil {
		return installState{}, gatewayConfig{}, err
	}

	existingState, err := readInstallState(paths.StatePath)
	if err != nil {
		return installState{}, gatewayConfig{}, err
	}
	gatewayBaseURL := fmt.Sprintf("http://%s:%d", options.ListenHost, options.ListenPort)
	if originalBaseURL == gatewayBaseURL {
		if existingState == nil || strings.TrimSpace(existingState.OriginalBaseURL) == "" {
			return installState{}, gatewayConfig{}, errors.New("A real upstream_base_url could not be determined")
		}
		originalBaseURL = existingState.OriginalBaseURL
	}

	backupPath := filepath.Join(paths.BackupDir, "config-"+time.Now().Format("20060102-150405")+".toml")
	if err := copyFile(options.CodexConfigPath, backupPath); err != nil {
		return installState{}, gatewayConfig{}, err
	}

	existingConfig, _ := loadGatewayConfig(paths.ConfigPath)
	config := defaultGatewayConfig()
	if existingConfig.UpstreamBaseURL != "" {
		config = existingConfig
	}
	config.ListenHost = options.ListenHost
	config.ListenPort = options.ListenPort
	config.UpstreamBaseURL = originalBaseURL
	config.Endpoints = mergeEndpoints(config.Endpoints, defaultEndpoints)
	if config.StreamAction == "" {
		config.StreamAction = defaultStreamAction
	}
	if config.HealthPath == "" {
		config.HealthPath = defaultHealthPath
	}
	if config.RequestBodyLimitBytes <= 0 || config.RequestBodyLimitBytes == legacyRequestBodyLimitBytes {
		config.RequestBodyLimitBytes = defaultRequestBodyLimitBytes
	}

	originalContent, err := os.ReadFile(options.CodexConfigPath)
	if err != nil {
		return installState{}, gatewayConfig{}, err
	}

	if err := writeGatewayConfig(paths.ConfigPath, config); err != nil {
		return installState{}, gatewayConfig{}, err
	}

	switch mode {
	case installModeProvider:
		if err := setCodexProviderBaseURL(options.CodexConfigPath, providerName, gatewayBaseURL); err != nil {
			_ = os.WriteFile(options.CodexConfigPath, originalContent, 0o644)
			return installState{}, gatewayConfig{}, err
		}
	case installModeNative:
		updated := ensureGatewayProvider(string(originalContent), gatewayBaseURL)
		if err := os.WriteFile(options.CodexConfigPath, []byte(updated), 0o644); err != nil {
			_ = os.WriteFile(options.CodexConfigPath, originalContent, 0o644)
			return installState{}, gatewayConfig{}, err
		}
	default:
		return installState{}, gatewayConfig{}, fmt.Errorf("unsupported install mode: %s", mode)
	}

	state := installState{
		InstalledAt:         time.Now().Format(time.RFC3339),
		InstallMode:         mode,
		CodexConfigPath:     options.CodexConfigPath,
		ProviderName:        providerName,
		ManagedProviderName: managedProviderName,
		OriginalBaseURL:     originalBaseURL,
		GatewayBaseURL:      gatewayBaseURL,
		GatewayConfigPath:   paths.ConfigPath,
		GatewayLogPath:      paths.LogPath,
		GatewayPIDPath:      paths.PIDPath,
		LatestBackupPath:    backupPath,
		StateRoot:           paths.StateRoot,
	}
	if err := writeInstallState(paths.StatePath, state); err != nil {
		return installState{}, gatewayConfig{}, err
	}
	return state, config, nil
}

func mergeEndpoints(existing []string, defaults []string) []string {
	result := make([]string, 0, len(existing)+len(defaults))
	for _, item := range append(append([]string{}, existing...), defaults...) {
		item = normalizePath(item)
		if item == "" || slices.Contains(result, item) {
			continue
		}
		result = append(result, item)
	}
	return result
}

func readRuntimeState(paths gatewayPaths) (map[string]any, error) {
	state, err := readInstallState(paths.StatePath)
	if err != nil || state == nil {
		return nil, err
	}

	payload := map[string]any{
		"installed_at":          state.InstalledAt,
		"install_mode":          state.InstallMode,
		"codex_config_path":     state.CodexConfigPath,
		"provider_name":         state.ProviderName,
		"managed_provider_name": state.ManagedProviderName,
		"original_base_url":     state.OriginalBaseURL,
		"gateway_base_url":      state.GatewayBaseURL,
		"gateway_config_path":   state.GatewayConfigPath,
		"gateway_log_path":      state.GatewayLogPath,
		"gateway_pid_path":      state.GatewayPIDPath,
		"latest_backup_path":    state.LatestBackupPath,
		"state_root":            state.StateRoot,
	}

	content, err := os.ReadFile(state.CodexConfigPath)
	if err == nil {
		provider := state.ProviderName
		if provider == "" {
			provider = state.ManagedProviderName
		}
		payload["codex_current_base_url"] = extractProviderBaseURL(string(content), provider)
	}
	return payload, nil
}

func restoreGatewayConfig(stateRoot string, codexConfigPath string) error {
	paths := buildGatewayPaths(stateRoot)
	state, err := readInstallState(paths.StatePath)
	if err != nil {
		return err
	}
	if state == nil {
		return fmt.Errorf("Install state file was not found: %s", paths.StatePath)
	}
	if codexConfigPath == "" {
		codexConfigPath = state.CodexConfigPath
	}
	if strings.TrimSpace(state.LatestBackupPath) == "" {
		return errors.New("A restorable backup file was not found")
	}
	if err := stopGateway(stateRoot, true); err != nil {
		return err
	}
	if err := copyFile(state.LatestBackupPath, codexConfigPath); err != nil {
		return err
	}
	_ = os.Remove(paths.StatePath)
	return nil
}

func stopGateway(stateRoot string, quiet bool) error {
	paths := buildGatewayPaths(stateRoot)
	content, err := os.ReadFile(paths.PIDPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if quiet {
				return nil
			}
			return fmt.Errorf("Gateway is not running")
		}
		return err
	}
	pid, err := parseInt(strings.TrimSpace(string(content)))
	if err != nil {
		_ = os.Remove(paths.PIDPath)
		return err
	}
	process, err := os.FindProcess(pid)
	if err == nil {
		_ = process.Kill()
	}
	_ = os.Remove(paths.PIDPath)
	return nil
}

func startGatewayBinary(stateRoot string, configPath string, logPath string, restartIfRunning bool) (string, error) {
	paths := buildGatewayPaths(stateRoot)
	if configPath == "" {
		configPath = paths.ConfigPath
	}
	if logPath == "" {
		logPath = paths.LogPath
	}

	if _, err := os.Stat(configPath); err != nil {
		return "", fmt.Errorf("Gateway config file was not found: %s", configPath)
	}
	if restartIfRunning {
		_ = stopGateway(stateRoot, true)
	} else if content, err := os.ReadFile(paths.PIDPath); err == nil {
		if pid, parseErr := parseInt(strings.TrimSpace(string(content))); parseErr == nil && isProcessAlive(pid) {
			return fmt.Sprintf("Gateway is already running. PID=%d", pid), nil
		}
		_ = os.Remove(paths.PIDPath)
	}

	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return "", err
	}
	exePath, err := os.Executable()
	if err != nil {
		return "", err
	}
	command := exec.Command(exePath, "serve", "--config", configPath, "--log", logPath)
	command.Stdout = nil
	command.Stderr = nil
	command.Stdin = nil
	command.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := command.Start(); err != nil {
		return "", err
	}
	if err := os.WriteFile(paths.PIDPath, []byte(fmt.Sprintf("%d", command.Process.Pid)), 0o644); err != nil {
		return "", err
	}
	_ = command.Process.Release()

	config, err := loadGatewayConfig(configPath)
	if err != nil {
		return "", err
	}
	if err := waitGatewayHealth(config.ListenHost, config.ListenPort, config.HealthPath, 15*time.Second); err != nil {
		return "", err
	}
	return fmt.Sprintf("Gateway started. PID=%d. Listen=http://%s:%d", command.Process.Pid, config.ListenHost, config.ListenPort), nil
}

func waitGatewayHealth(host string, port int, healthPath string, timeout time.Duration) error {
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	url := fmt.Sprintf("http://%s:%d%s", host, port, normalizePath(healthPath))
	for time.Now().Before(deadline) {
		resp, err := client.Get(url)
		if err == nil {
			body, _ := ioReadAllAndClose(resp.Body)
			if resp.StatusCode == http.StatusOK && bytes.Contains(body, []byte(`"ok":true`)) {
				return nil
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	return fmt.Errorf("gateway did not become healthy: %s", url)
}

func isProcessAlive(pid int) bool {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		err = process.Signal(syscall.Signal(0))
		return err == nil
	}
	err = process.Signal(syscall.Signal(0))
	return err == nil
}

func launchUI(options installOptions, noOpen bool) error {
	paths := buildGatewayPaths(options.StateRoot)
	state, _ := readInstallState(paths.StatePath)
	mode := "install"
	if state != nil && state.CodexConfigPath == options.CodexConfigPath && state.GatewayBaseURL != "" {
		mode = "reuse"
	} else {
		var err error
		installedState, _, installErr := installGateway(options)
		err = installErr
		if err != nil {
			return err
		}
		state = &installedState
	}

	message, err := startGatewayBinary(options.StateRoot, paths.ConfigPath, paths.LogPath, true)
	if err != nil {
		return err
	}
	_ = message

	url := state.GatewayBaseURL + defaultUIPath
	if !noOpen {
		_ = openURL(url)
	}
	fmt.Printf("Codex Retry Gateway UI is ready\nmode=%s\nprovider=%s\ngateway=%s\nupstream=%s\nui=%s\n", mode, state.ProviderName, state.GatewayBaseURL, state.OriginalBaseURL, url)
	return nil
}

func openURL(url string) error {
	switch runtime.GOOS {
	case "windows":
		return exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
	case "darwin":
		return exec.Command("open", url).Start()
	default:
		return exec.Command("xdg-open", url).Start()
	}
}

func copyFile(from string, to string) error {
	body, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	return os.WriteFile(to, body, 0o644)
}

func runInstall(options cliOptions) error {
	listenPort, err := intArg(options, "listen-port", defaultListenPort)
	if err != nil {
		return err
	}
	installState, _, err := installGateway(installOptions{
		CodexConfigPath: requiredArg(options, "codex-config-path", defaultCodexConfigPath()),
		StateRoot:       requiredArg(options, "state-root", defaultStateRoot()),
		ListenHost:      requiredArg(options, "listen-host", defaultListenHost),
		ListenPort:      listenPort,
	})
	if err != nil {
		return err
	}
	if _, err := startGatewayBinary(installState.StateRoot, installState.GatewayConfigPath, installState.GatewayLogPath, true); err != nil {
		return err
	}
	fmt.Printf("Installed Codex Retry Gateway\nprovider=%s\ngateway=%s\nupstream=%s\n", installState.ProviderName, installState.GatewayBaseURL, installState.OriginalBaseURL)
	return nil
}

func runLaunchUI(options cliOptions) error {
	listenPort, err := intArg(options, "listen-port", defaultListenPort)
	if err != nil {
		return err
	}
	return launchUI(installOptions{
		CodexConfigPath: requiredArg(options, "codex-config-path", defaultCodexConfigPath()),
		StateRoot:       requiredArg(options, "state-root", defaultStateRoot()),
		ListenHost:      requiredArg(options, "listen-host", defaultListenHost),
		ListenPort:      listenPort,
	}, options.Bool["no-open"])
}

func runStart(options cliOptions) error {
	stateRoot := requiredArg(options, "state-root", defaultStateRoot())
	message, err := startGatewayBinary(
		stateRoot,
		requiredArg(options, "config-path", buildGatewayPaths(stateRoot).ConfigPath),
		requiredArg(options, "log-path", buildGatewayPaths(stateRoot).LogPath),
		options.Bool["restart-if-running"],
	)
	if err != nil {
		return err
	}
	fmt.Println(message)
	return nil
}

func runStop(options cliOptions) error {
	return stopGateway(requiredArg(options, "state-root", defaultStateRoot()), options.Bool["quiet"])
}

func runRestore(options cliOptions) error {
	if err := restoreGatewayConfig(
		requiredArg(options, "state-root", defaultStateRoot()),
		requiredArg(options, "codex-config-path", defaultCodexConfigPath()),
	); err != nil {
		return err
	}
	fmt.Println("Restored Codex config")
	return nil
}
