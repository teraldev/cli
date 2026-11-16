package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/zalando/go-keyring"
)

const keyringService = "teral-cli"

// ErrNoToken means no token is stored for the workspace.
var ErrNoToken = errors.New("no token stored")

// tokenKey scopes a token to the API it belongs to, so staging and
// production sessions never mix.
func tokenKey(apiURL, workspaceID string) string {
	return apiURL + "|" + workspaceID
}

// StoreToken saves a session token, preferring the OS keychain. It returns
// where the token ended up: "keyring" or "file".
func (c *Config) StoreToken(apiURL, workspaceID, token string) (string, error) {
	key := tokenKey(apiURL, workspaceID)
	if os.Getenv("TERAL_NO_KEYRING") == "" {
		if err := keyring.Set(keyringService, key, token); err == nil {
			return "keyring", nil
		}
	}
	if err := c.updateFileTokens(func(tokens map[string]string) { tokens[key] = token }); err != nil {
		return "", err
	}
	return "file", nil
}

// Token reads a session token stored by [Config.StoreToken].
func (c *Config) Token(apiURL, workspaceID, storage string) (string, error) {
	key := tokenKey(apiURL, workspaceID)
	if storage != "file" {
		token, err := keyring.Get(keyringService, key)
		if err == nil {
			return token, nil
		}
		if !errors.Is(err, keyring.ErrNotFound) && storage == "keyring" {
			return "", fmt.Errorf("read token from the keychain: %w", err)
		}
	}
	tokens, err := c.readFileTokens()
	if err != nil {
		return "", err
	}
	if token, ok := tokens[key]; ok {
		return token, nil
	}
	return "", ErrNoToken
}

// DeleteToken removes a session token from wherever it is stored.
func (c *Config) DeleteToken(apiURL, workspaceID string) error {
	key := tokenKey(apiURL, workspaceID)
	// A missing or locked keychain is fine; the file below may hold the token.
	_ = keyring.Delete(keyringService, key)
	return c.updateFileTokens(func(tokens map[string]string) { delete(tokens, key) })
}

type credentialsFile struct {
	Tokens map[string]string `toml:"tokens"`
}

func (c *Config) credentialsPath() string { return filepath.Join(c.dir, "credentials.toml") }

func (c *Config) readFileTokens() (map[string]string, error) {
	var f credentialsFile
	if _, err := toml.DecodeFile(c.credentialsPath(), &f); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read %s: %w", c.credentialsPath(), err)
	}
	if f.Tokens == nil {
		f.Tokens = map[string]string{}
	}
	return f.Tokens, nil
}

func (c *Config) updateFileTokens(update func(map[string]string)) error {
	tokens, err := c.readFileTokens()
	if err != nil {
		return err
	}
	before := len(tokens)
	update(tokens)
	if len(tokens) == 0 {
		if before == 0 {
			return nil
		}
		return os.Remove(c.credentialsPath())
	}
	if err := os.MkdirAll(c.dir, 0o700); err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# Teral CLI session tokens. Keep this file private.\n")
	if err := toml.NewEncoder(&b).Encode(credentialsFile{Tokens: tokens}); err != nil {
		return err
	}
	return writeFileAtomic(c.credentialsPath(), []byte(b.String()), 0o600)
}
