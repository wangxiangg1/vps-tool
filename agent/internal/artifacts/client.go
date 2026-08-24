package artifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const maxArtifactBytes int64 = 32 * 1024 * 1024

type Client struct {
	baseURL    string
	nodeID     string
	httpClient *http.Client
	mu         sync.RWMutex
	credential string
}

func NewClient(wssURL, nodeID, credential string) (*Client, error) {
	u, err := url.Parse(wssURL)
	if err != nil || u.Host == "" || (u.Scheme != "wss" && u.Scheme != "ws") {
		return nil, fmt.Errorf("invalid WSS URL for artifact client")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("WSS URL contains unsupported credentials or query")
	}
	u.Scheme = map[string]string{"wss": "https", "ws": "http"}[u.Scheme]
	u.Path = "/api/agent/xui-backups"
	u.RawPath = ""
	return &Client{
		baseURL:    strings.TrimRight(u.String(), "/"),
		nodeID:     nodeID,
		credential: credential,
		httpClient: &http.Client{},
	}, nil
}

func (c *Client) SetCredential(value string) {
	c.mu.Lock()
	c.credential = value
	c.mu.Unlock()
}

func (c *Client) request(ctx context.Context, method, backupID string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+"/"+url.PathEscape(backupID), body)
	if err != nil {
		return nil, err
	}
	c.mu.RLock()
	credential := c.credential
	c.mu.RUnlock()
	if credential == "" {
		return nil, fmt.Errorf("agent credential is not available")
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	req.Header.Set("X-VPS-Node-ID", c.nodeID)
	req.Header.Set("Content-Type", "application/octet-stream")
	return req, nil
}

func (c *Client) Upload(ctx context.Context, backupID, path string) (int64, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, "", fmt.Errorf("open backup: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return 0, "", fmt.Errorf("stat backup: %w", err)
	}
	if info.Size() <= 0 || info.Size() > maxArtifactBytes {
		return 0, "", fmt.Errorf("backup size is outside the allowed range")
	}
	req, err := c.request(ctx, http.MethodPut, backupID, io.LimitReader(file, maxArtifactBytes+1))
	if err != nil {
		return 0, "", err
	}
	req.ContentLength = info.Size()
	response, err := c.httpClient.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("upload backup: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return 0, "", fmt.Errorf("upload backup returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}
	digest, err := fileSHA256(path)
	if err != nil {
		return 0, "", err
	}
	return info.Size(), digest, nil
}

func (c *Client) Download(ctx context.Context, backupID, path string) (int64, string, error) {
	req, err := c.request(ctx, http.MethodGet, backupID, nil)
	if err != nil {
		return 0, "", err
	}
	response, err := c.httpClient.Do(req)
	if err != nil {
		return 0, "", fmt.Errorf("download backup: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return 0, "", fmt.Errorf("download backup returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return 0, "", err
	}
	temporary := path + ".part"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return 0, "", err
	}
	count, copyErr := io.Copy(file, io.LimitReader(response.Body, maxArtifactBytes+1))
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.Remove(temporary)
		if copyErr != nil {
			return 0, "", copyErr
		}
		return 0, "", closeErr
	}
	if count <= 0 || count > maxArtifactBytes {
		_ = os.Remove(temporary)
		return 0, "", fmt.Errorf("downloaded backup size is outside the allowed range")
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return 0, "", err
	}
	digest, err := fileSHA256(path)
	if err != nil {
		return 0, "", err
	}
	if expected := response.Header.Get("X-VPS-Backup-SHA256"); expected != "" && !strings.EqualFold(expected, digest) {
		_ = os.Remove(path)
		return 0, "", fmt.Errorf("downloaded backup checksum mismatch")
	}
	return count, digest, nil
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, maxArtifactBytes+1)); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}
