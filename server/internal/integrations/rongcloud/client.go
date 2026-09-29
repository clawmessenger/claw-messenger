package rongcloud

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const defaultAPIBase = "https://api.rong-api.com"

// rongcloudAPIClient sends messages via RongCloud Server REST API.
type rongcloudAPIClient struct {
	appKey    string
	appSecret string
	base      string
	client    *http.Client
	logger    *slog.Logger
	// loadCreds, when set, lazily resolves credentials from the active
	// channel installation on first use (and re-resolves after a reset).
	loadCreds func(ctx context.Context) (appKey, appSecret string, err error)
}

func newRongCloudAPIClient(appKey, appSecret, base string, client *http.Client, logger *slog.Logger) *rongcloudAPIClient {
	if base == "" {
		base = defaultAPIBase
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &rongcloudAPIClient{
		appKey:    appKey,
		appSecret: appSecret,
		base:      base,
		client:    client,
		logger:    logger,
	}
}

const nonceChars = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// generateNonce produces a random alphanumeric string of the given length.
func generateNonce(length int) string {
	b := make([]byte, length)
	max := big.NewInt(int64(len(nonceChars)))
	for i := range b {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			b[i] = nonceChars[0]
			continue
		}
		b[i] = nonceChars[n.Int64()]
	}
	return string(b)
}

// computeSignature computes SHA1(AppSecret + Nonce + Timestamp) as lowercase hex.
func computeSignature(appSecret, nonce string, timestamp int64) string {
	return computeSignatureFromString(appSecret, nonce, fmt.Sprintf("%d", timestamp))
}

// computeSignatureFromString computes SHA1(AppSecret + Nonce + Timestamp) from string timestamp.
func computeSignatureFromString(appSecret, nonce, timestamp string) string {
	source := appSecret + nonce + timestamp
	h := sha1.Sum([]byte(source))
	return hex.EncodeToString(h[:])
}

func (c *rongcloudAPIClient) signRequest(req *http.Request, contentType string) {
	nonce := generateNonce(18)
	timestamp := time.Now().UnixMilli()
	signature := computeSignature(c.appSecret, nonce, timestamp)
	req.Header.Set("App-Key", c.appKey)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Nonce", nonce)
	req.Header.Set("Timestamp", fmt.Sprintf("%d", timestamp))
	req.Header.Set("Signature", signature)
}

// ensureCreds populates appKey/appSecret for service-layer clients that were
// constructed without credentials, resolving them lazily from the active
// channel installation so a later (re)configuration is picked up too.
func (c *rongcloudAPIClient) ensureCreds(ctx context.Context) error {
	if c.appKey != "" && c.appSecret != "" || c.loadCreds == nil {
		return nil
	}
	appKey, appSecret, err := c.loadCreds(ctx)
	if err != nil {
		return fmt.Errorf("rongcloud: load installation credentials: %w", err)
	}
	if appKey == "" || appSecret == "" {
		return fmt.Errorf("rongcloud: no active installation with app credentials")
	}
	c.appKey, c.appSecret = appKey, appSecret
	return nil
}

// postForm sends a POST request with form-urlencoded body and returns the parsed JSON response.
func (c *rongcloudAPIClient) postForm(ctx context.Context, path string, form url.Values) (map[string]interface{}, error) {
	if err := c.ensureCreds(ctx); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, fmt.Errorf("rongcloud: create request: %w", err)
	}
	c.signRequest(req, "application/x-www-form-urlencoded")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("rongcloud: send request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("rongcloud: read response: %w", err)
	}
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("rongcloud: parse response: %w", err)
	}
	if code, ok := result["code"].(float64); ok && code != 200 {
		return result, fmt.Errorf("rongcloud: API error code %v: %v", code, result["errorMessage"])
	}
	return result, nil
}

// sendPrivateMessage sends a message to a specific user via RongCloud.
func (c *rongcloudAPIClient) sendPrivateMessage(ctx context.Context, fromUserID, toUserID, objectName, content string) (string, error) {
	form := url.Values{
		"fromUserId":  {fromUserID},
		"toUserId":    {toUserID},
		"objectName":  {objectName},
		"content":     {content},
		"isPersisted": {"1"},
		"isCounted":   {"1"},
	}
	result, err := c.postForm(ctx, "/message/private/publish.json", form)
	if err != nil {
		return "", err
	}
	if msgUID, ok := result["msgUID"].(string); ok {
		return msgUID, nil
	}
	return "", nil
}

// sendCommandResult sends a command_result message back to the requesting user.
func (c *rongcloudAPIClient) sendCommandResult(ctx context.Context, fromUserID, toUserID, requestID string, payload map[string]interface{}) error {
	resultContent := CommandResultContent{
		RequestID: requestID,
		MsgType:   "command_result",
		Payload:   payload,
	}
	contentBytes, err := json.Marshal(resultContent)
	if err != nil {
		return fmt.Errorf("rongcloud: marshal command result: %w", err)
	}
	_, err = c.sendPrivateMessage(ctx, fromUserID, toUserID, objectNameCommand, string(contentBytes))
	return err
}
