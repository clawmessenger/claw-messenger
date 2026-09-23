package rongcloud

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// installConfig is the JSON shape stored in channel_installation.config JSONB.
type installConfig struct {
	AppKey             string `json:"app_key"`
	AppSecretEncrypted string `json:"app_secret_encrypted"`
	SystemNodeID       string `json:"system_node_id"`
}

// credentials holds the decrypted RongCloud API credentials.
type credentials struct {
	AppKey       string
	AppSecret    string
	SystemNodeID string
}

// Decrypter decrypts ciphertext produced by the secretbox encryption layer.
type Decrypter func(ciphertext []byte) (plaintext []byte, err error)

// decodeCredentials unmarshals the installation config and decrypts the app secret.
func decodeCredentials(raw json.RawMessage, decrypt Decrypter) (credentials, error) {
	var ic installConfig
	if err := json.Unmarshal(raw, &ic); err != nil {
		return credentials{}, fmt.Errorf("rongcloud: unmarshal install config: %w", err)
	}
	if strings.TrimSpace(ic.AppKey) == "" {
		return credentials{}, errors.New("rongcloud: app_key is required")
	}
	if strings.TrimSpace(ic.AppSecretEncrypted) == "" {
		return credentials{}, errors.New("rongcloud: app_secret_encrypted is required")
	}
	if strings.TrimSpace(ic.SystemNodeID) == "" {
		return credentials{}, errors.New("rongcloud: system_node_id is required")
	}
	secret, err := decryptSecret(ic.AppSecretEncrypted, decrypt)
	if err != nil {
		return credentials{}, fmt.Errorf("rongcloud: decrypt app secret: %w", err)
	}
	return credentials{
		AppKey:       strings.TrimSpace(ic.AppKey),
		AppSecret:    secret,
		SystemNodeID: strings.TrimSpace(ic.SystemNodeID),
	}, nil
}

// decryptSecret base64-decodes then decrypts the app secret.
func decryptSecret(enc string, decrypt Decrypter) (string, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(enc)
	if err != nil {
		return "", fmt.Errorf("rongcloud: base64 decode app secret: %w", err)
	}
	plaintext, err := decrypt(ciphertext)
	if err != nil {
		return "", fmt.Errorf("rongcloud: decrypt: %w", err)
	}
	return strings.TrimSpace(string(plaintext)), nil
}
