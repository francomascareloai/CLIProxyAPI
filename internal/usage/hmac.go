package usage

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	log "github.com/sirupsen/logrus"
)

const (
	cliproxyDirName       = ".cliproxy"
	usageHMACKeyFileName  = "usage_hmac_key"
	usageStatsFileName    = "usage_stats.json"
	usageHMACKeyBytesSize = 32
)

var (
	usageHMACKeyOnce    sync.Once
	usageHMACKey        []byte
	usageHMACKeyErr     error
	usageHMACKeyLogOnce sync.Once
)

func cliproxyDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("usage: resolve home dir: %w", err)
	}
	if strings.TrimSpace(home) == "" {
		return "", errors.New("usage: resolve home dir: empty")
	}
	return filepath.Join(home, cliproxyDirName), nil
}

func ensureCliproxyDir() (string, error) {
	dir, err := cliproxyDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("usage: create cliproxy dir %s: %w", dir, err)
	}
	return dir, nil
}

func usageHMACKeyPath() (string, error) {
	dir, err := ensureCliproxyDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, usageHMACKeyFileName), nil
}

func usageStatsPath() (string, error) {
	dir, err := ensureCliproxyDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, usageStatsFileName), nil
}

func ensureUsageHMACKey() ([]byte, error) {
	usageHMACKeyOnce.Do(func() {
		keyPath, err := usageHMACKeyPath()
		if err != nil {
			usageHMACKeyErr = err
			return
		}

		data, err := os.ReadFile(keyPath)
		if err != nil {
			if !os.IsNotExist(err) {
				usageHMACKeyErr = fmt.Errorf("usage: read hmac key file %s: %w", keyPath, err)
				return
			}

			// File is missing: create a new secret, but publish it atomically without
			// overwriting an existing file (multi-process safe).
			key := make([]byte, usageHMACKeyBytesSize)
			if _, err := rand.Read(key); err != nil {
				usageHMACKeyErr = fmt.Errorf("usage: generate hmac key: %w", err)
				return
			}
			if err := writeUsageHMACKeyAtomic(keyPath, key, 0o600); err != nil {
				usageHMACKeyErr = err
				return
			}

			data, err = os.ReadFile(keyPath)
			if err != nil {
				usageHMACKeyErr = fmt.Errorf("usage: read hmac key file %s: %w", keyPath, err)
				return
			}
		}

		key, err := parseUsageHMACKey(data)
		if err != nil {
			usageHMACKeyErr = fmt.Errorf("usage: invalid hmac key file %s: %w", keyPath, err)
			return
		}
		_ = os.Chmod(keyPath, 0o600)
		usageHMACKey = key
	})

	if usageHMACKeyErr != nil {
		return nil, usageHMACKeyErr
	}
	if len(usageHMACKey) != usageHMACKeyBytesSize {
		return nil, fmt.Errorf("usage: unexpected hmac key length %d", len(usageHMACKey))
	}
	return usageHMACKey, nil
}

func writeUsageHMACKeyAtomic(keyPath string, key []byte, perm os.FileMode) error {
	keyPath = strings.TrimSpace(keyPath)
	if keyPath == "" {
		return errors.New("usage: invalid hmac key path")
	}
	if len(key) != usageHMACKeyBytesSize {
		return fmt.Errorf("usage: unexpected hmac key length %d", len(key))
	}

	dir := filepath.Dir(keyPath)
	if strings.TrimSpace(dir) == "" {
		return errors.New("usage: invalid hmac key directory")
	}

	f, err := os.CreateTemp(dir, usageHMACKeyFileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("usage: create hmac key tmp in %s: %w", dir, err)
	}
	tmpPath := f.Name()
	cleanup := func() {
		_ = f.Close()
		_ = os.Remove(tmpPath)
	}

	_ = os.Chmod(tmpPath, perm)
	if n, err := f.Write(key); err != nil {
		cleanup()
		return fmt.Errorf("usage: write hmac key tmp %s: %w", tmpPath, err)
	} else if n != len(key) {
		cleanup()
		return fmt.Errorf("usage: short write hmac key tmp %s: %d/%d", tmpPath, n, len(key))
	}
	if err := f.Sync(); err != nil {
		cleanup()
		return fmt.Errorf("usage: fsync hmac key tmp %s: %w", tmpPath, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("usage: close hmac key tmp %s: %w", tmpPath, err)
	}

	// Publish atomically without replacing an existing key (multi-process safe).
	if runtime.GOOS == "windows" {
		if err := os.Rename(tmpPath, keyPath); err != nil {
			// Windows can return different errors depending on filesystem / antivirus hooks.
			// If the destination exists, consider this a successful publish and use it.
			if _, statErr := os.Stat(keyPath); statErr == nil {
				_ = os.Remove(tmpPath)
				_ = os.Chmod(keyPath, perm)
				return nil
			}
			_ = os.Remove(tmpPath)
			return fmt.Errorf("usage: rename hmac key tmp %s -> %s: %w", tmpPath, keyPath, err)
		}
	} else {
		if err := os.Link(tmpPath, keyPath); err != nil {
			if os.IsExist(err) {
				_ = os.Remove(tmpPath)
				_ = os.Chmod(keyPath, perm)
				return nil
			}
			_ = os.Remove(tmpPath)
			return fmt.Errorf("usage: link hmac key tmp %s -> %s: %w", tmpPath, keyPath, err)
		}
		_ = os.Remove(tmpPath)
	}

	_ = os.Chmod(keyPath, perm)

	// Best-effort fsync the directory to ensure the rename/link is durable.
	d, err := os.Open(dir)
	if err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func parseUsageHMACKey(data []byte) ([]byte, error) {
	switch len(data) {
	case usageHMACKeyBytesSize:
		key := make([]byte, usageHMACKeyBytesSize)
		copy(key, data)
		return key, nil
	case usageHMACKeyBytesSize + 1:
		if data[usageHMACKeyBytesSize] == '\n' {
			key := make([]byte, usageHMACKeyBytesSize)
			copy(key, data[:usageHMACKeyBytesSize])
			return key, nil
		}
	case usageHMACKeyBytesSize + 2:
		if data[usageHMACKeyBytesSize] == '\r' && data[usageHMACKeyBytesSize+1] == '\n' {
			key := make([]byte, usageHMACKeyBytesSize)
			copy(key, data[:usageHMACKeyBytesSize])
			return key, nil
		}
	}

	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, errors.New("empty")
	}
	if len(trimmed) == usageHMACKeyBytesSize*2 {
		decoded, err := hex.DecodeString(trimmed)
		if err == nil && len(decoded) == usageHMACKeyBytesSize {
			return decoded, nil
		}
	}
	decoded, err := base64.StdEncoding.DecodeString(trimmed)
	if err == nil && len(decoded) == usageHMACKeyBytesSize {
		return decoded, nil
	}
	return nil, fmt.Errorf("unsupported format (len=%d)", len(trimmed))
}

// HashClientKey returns a stable client identifier derived from apiKey, without storing
// the API key in plaintext.
//
// Output format: api:hmac256:<hex>
func HashClientKey(apiKey string) string {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return ""
	}
	key, err := ensureUsageHMACKey()
	if err != nil {
		usageHMACKeyLogOnce.Do(func() {
			log.Warnf("usage: hmac key unavailable, client ids will be bucketed: %v", err)
		})
		return "api:hmac256:unavailable"
	}

	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(apiKey))
	sum := mac.Sum(nil)
	return "api:hmac256:" + hex.EncodeToString(sum)
}
