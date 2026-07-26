package web

// Self-contained APNs sender for BentoLife push notifications. Reuses the
// provisioning the user already set up for dotfiles/apns/apns-notify:
// ~/.config/bento-apns/config.json (team/key/bundle + registered device
// tokens) and the AuthKey_*.p8. Ported so a background agent-deck service can
// push directly without shelling out to another repo's binary.

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type apnsConfig struct {
	TeamID       string   `json:"team_id"`
	KeyID        string   `json:"key_id"`
	KeyPath      string   `json:"key_path"`
	BundleID     string   `json:"bundle_id"`
	Production   bool     `json:"production"`
	DeviceTokens []string `json:"device_tokens"`
}

func apnsExpandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

func apnsConfigPath() string {
	if p := os.Getenv("BENTO_APNS_CONFIG"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "bento-apns", "config.json")
}

func loadAPNsConfig() (*apnsConfig, error) {
	data, err := os.ReadFile(apnsConfigPath())
	if err != nil {
		return nil, err
	}
	var c apnsConfig
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse apns config: %w", err)
	}
	return &c, nil
}

func signAPNsJWT(c *apnsConfig) (string, error) {
	keyPEM, err := os.ReadFile(apnsExpandHome(c.KeyPath))
	if err != nil {
		return "", fmt.Errorf("read key %s: %w", c.KeyPath, err)
	}
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return "", fmt.Errorf("key %s is not PEM", c.KeyPath)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("parse PKCS8 key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return "", fmt.Errorf("key is not an ECDSA private key")
	}
	b64 := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	header := b64([]byte(fmt.Sprintf(`{"alg":"ES256","kid":%q}`, c.KeyID)))
	claims := b64([]byte(fmt.Sprintf(`{"iss":%q,"iat":%d}`, c.TeamID, time.Now().Unix())))
	signingInput := header + "." + claims
	digest := sha256.Sum256([]byte(signingInput))
	r, s, err := ecdsa.Sign(rand.Reader, key, digest[:])
	if err != nil {
		return "", fmt.Errorf("sign: %w", err)
	}
	sig := make([]byte, 64) // ES256 = R||S, each left-padded to 32 bytes
	r.FillBytes(sig[:32])
	s.FillBytes(sig[32:])
	return signingInput + "." + b64(sig), nil
}

func apnsHost(c *apnsConfig) string {
	if c.Production {
		return "https://api.push.apple.com"
	}
	return "https://api.sandbox.push.apple.com"
}

var apnsClient = &http.Client{Timeout: 10 * time.Second}

// sendAPNsPush sends an alert push (title/body) with arbitrary top-level custom
// keys (data) to every registered device token. The custom keys ride alongside
// "aps" so the app reads them from userInfo (session_id, audio_file, kind).
// Returns the number of tokens accepted.
func sendAPNsPush(c *apnsConfig, title, body string, data map[string]any) (int, error) {
	if len(c.DeviceTokens) == 0 {
		return 0, fmt.Errorf("no device tokens registered")
	}
	jwt, err := signAPNsJWT(c)
	if err != nil {
		return 0, err
	}

	root := map[string]any{
		"aps": map[string]any{
			"alert":              map[string]string{"title": title, "body": body},
			"sound":              "default",
			"interruption-level": "time-sensitive",
			"category":           "VOICE_SUMMARY",
			// Wake the app on receipt (when it's alive via keep-alive) so it can
			// play the ducking attention tone, not just show the banner.
			"content-available": 1,
		},
	}
	for k, v := range data {
		root[k] = v
	}
	payload, _ := json.Marshal(root)

	ok := 0
	var failures []string
	for _, token := range c.DeviceTokens {
		req, _ := http.NewRequest(http.MethodPost, apnsHost(c)+"/3/device/"+token, bytes.NewReader(payload))
		req.Header.Set("authorization", "bearer "+jwt)
		req.Header.Set("apns-topic", c.BundleID)
		req.Header.Set("apns-push-type", "alert")
		req.Header.Set("apns-priority", "10")
		resp, err := apnsClient.Do(req)
		if err != nil {
			failures = append(failures, err.Error())
			continue
		}
		rb, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			ok++
		} else {
			failures = append(failures, fmt.Sprintf("HTTP %d %s", resp.StatusCode, strings.TrimSpace(string(rb))))
		}
	}
	if len(failures) > 0 {
		return ok, fmt.Errorf("%d/%d failed: %s", len(failures), len(c.DeviceTokens), strings.Join(failures, "; "))
	}
	return ok, nil
}
