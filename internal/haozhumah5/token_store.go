package haozhumah5

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const tokenStoreVersion = "workbuddy2api/haozhuma-web-token/v1"

type tokenStore struct {
	path string
	aead cipher.AEAD
}

// NewPersistent loads the saved value without contacting the provider. An
// explicitly saved empty value overrides the environment fallback after clear.
// A damaged store disables the session until it is explicitly saved again.
func NewPersistent(fallback, path, secret string) (*Client, error) {
	c := New(fallback)
	if path == "" {
		return c, nil
	}
	if secret == "" {
		c.storageError = "未配置稳定的管理员密钥，Web 登录令牌仅在本次运行期间生效"
		return c, errors.New(c.storageError)
	}
	key := sha256.Sum256([]byte(tokenStoreVersion + "\x00" + secret))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	c.tokenStore = &tokenStore{path: path, aead: aead}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err == nil {
		var data []byte
		data, err = io.ReadAll(io.LimitReader(f, 8193))
		_ = f.Close()
		if err == nil && len(data) >= aead.NonceSize()+aead.Overhead() && len(data) <= 8192 {
			var plain []byte
			plain, err = aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], []byte(tokenStoreVersion))
			if err == nil && validStoredToken(string(plain)) {
				c.session = string(plain)
				c.persisted = c.session != ""
				return c, nil
			}
		}
	}
	c.session = ""
	c.storageError = "已保存的 Web 登录令牌无法读取，请检查存储权限及原管理员密钥，或重新保存令牌"
	return c, errors.New(c.storageError)
}

func validStoredToken(token string) bool {
	return len(token) <= 4096 && token == strings.TrimSpace(token) && !strings.ContainsAny(token, "\r\n")
}

// SaveSession commits encrypted storage before changing the active session.
// Failures preserve both the active value and the last complete store.
func (c *Client) SaveSession(token string) error {
	token = strings.TrimSpace(token)
	if !validStoredToken(token) {
		return errors.New("令牌格式不正确")
	}
	c.saveMu.Lock()
	defer c.saveMu.Unlock()
	if c.tokenStore != nil {
		if err := c.tokenStore.save(token); err != nil {
			c.mu.Lock()
			c.storageError = "Web 登录令牌保存失败，原令牌保持不变，请检查持久目录的空间和写入权限"
			c.mu.Unlock()
			return errors.New("Web 登录令牌保存失败，原令牌保持不变，请检查持久目录的空间和写入权限")
		}
	}
	c.Session(token)
	c.mu.Lock()
	c.persisted = c.tokenStore != nil && token != ""
	if c.tokenStore != nil {
		c.storageError = ""
	}
	c.mu.Unlock()
	return nil
}

func (s *tokenStore) save(token string) error {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	data := s.aead.Seal(nonce, nonce, []byte(token), []byte(tokenStoreVersion))
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".haozhuma-token-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), s.path)
}
