package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

type authTokens struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id"`
}

type authState struct {
	AuthMode     string     `json:"auth_mode"`
	OpenAIAPIKey string     `json:"OPENAI_API_KEY"`
	Tokens       authTokens `json:"tokens"`
	LastRefresh  string     `json:"last_refresh"`
}

func loadAuthState(authPath string) (*authState, error) {
	content, err := os.ReadFile(authPath)
	if err != nil {
		return nil, err
	}
	var state authState
	if err := json.Unmarshal(content, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func resolveBearerToken(incoming string, stateRoot string, codexConfigPath string) (string, error) {
	if value := strings.TrimSpace(incoming); value != "" {
		return value, nil
	}

	authCandidates := []string{}
	if codexConfigPath != "" {
		authCandidates = append(authCandidates, filepath.Join(filepath.Dir(codexConfigPath), "auth.json"))
	}
	if stateRoot != "" {
		authCandidates = append(authCandidates, filepath.Join(stateRoot, "auth.json"))
	}
	authCandidates = append(authCandidates, defaultAuthPath())

	seen := map[string]bool{}
	for _, authPath := range authCandidates {
		authPath = strings.TrimSpace(authPath)
		if authPath == "" || seen[authPath] {
			continue
		}
		seen[authPath] = true
		state, err := loadAuthState(authPath)
		if err != nil {
			continue
		}
		if value := strings.TrimSpace(state.OpenAIAPIKey); value != "" {
			return "Bearer " + value, nil
		}
		if value := strings.TrimSpace(state.Tokens.AccessToken); value != "" {
			return "Bearer " + value, nil
		}
	}
	return "", errors.New("authorization token not available")
}
