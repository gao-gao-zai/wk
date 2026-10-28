package server

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
)

const accountTransferLimit = 1000
const accountTransferBytes = 16 << 20
const accountTransferFormat = "workbuddy2api-accounts"
const accountTransferRounds = 600000

type accountTransferBundle struct {
	Format     string `json:"format"`
	Version    int    `json:"version"`
	Salt       []byte `json:"salt"`
	Nonce      []byte `json:"nonce"`
	Ciphertext []byte `json:"ciphertext"`
}
type transferredAccount struct {
	Credential json.RawMessage `json:"credential"`
	Groups     []string        `json:"groups"`
	Disabled   bool            `json:"disabled"`
}
type accountTransferDocument struct {
	CreatedAt time.Time            `json:"created_at"`
	Accounts  []transferredAccount `json:"accounts"`
}
type accountImportRequest struct {
	Bundle   accountTransferBundle `json:"bundle"`
	Password string                `json:"password"`
	UIDs     []string              `json:"uids"`
}
type accountImportRow struct {
	UID            string   `json:"uid"`
	Nickname       string   `json:"nickname"`
	Region         string   `json:"region"`
	Exists         bool     `json:"exists"`
	SourceDisabled bool     `json:"source_disabled"`
	Groups         []string `json:"groups"`
	Issue          string   `json:"issue,omitempty"`
}

func transferError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, status, map[string]string{"error": message})
}
func decodeTransfer(w http.ResponseWriter, r *http.Request, dst any) bool {
	w.Header().Set("Cache-Control", "no-store")
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, accountTransferBytes))
	if dec.Decode(dst) != nil || dec.Decode(new(any)) != io.EOF {
		transferError(w, 400, "迁移请求无效或超过 16 MiB")
		return false
	}
	return true
}
func transferPasswordOK(password string) bool { return len(password) >= 12 && len(password) <= 1024 }
func validTransferUID(uid string) bool {
	return uid != "" && len(uid) <= 200 && uid != "." && uid != ".." && strings.TrimSpace(uid) == uid && !strings.ContainsAny(uid, "/\\") && filepath.Base(uid) == uid
}
func transferUIDs(uids []string) ([]string, error) {
	if len(uids) == 0 || len(uids) > accountTransferLimit {
		return nil, errors.New("请选择 1–1000 个账号")
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(uids))
	for _, uid := range uids {
		if !validTransferUID(uid) {
			return nil, errors.New("账号 UID 无效")
		}
		if !seen[uid] {
			seen[uid] = true
			out = append(out, uid)
		}
	}
	return out, nil
}
func accountTransferCipher(password string, salt []byte) (cipher.AEAD, error) {
	key, err := pbkdf2.Key(sha256.New, password, salt, accountTransferRounds, 32)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func sealAccounts(doc accountTransferDocument, password string) (accountTransferBundle, error) {
	b := accountTransferBundle{Format: accountTransferFormat, Version: 1, Salt: make([]byte, 16), Nonce: make([]byte, 12)}
	if _, err := rand.Read(b.Salt); err != nil {
		return b, err
	}
	if _, err := rand.Read(b.Nonce); err != nil {
		return b, err
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return b, err
	}
	if len(raw) > 10<<20 {
		return b, errors.New("迁移内容过大")
	}
	aead, err := accountTransferCipher(password, b.Salt)
	if err != nil {
		return b, err
	}
	b.Ciphertext = aead.Seal(nil, b.Nonce, raw, []byte(accountTransferFormat+"/v1"))
	return b, nil
}
func openAccounts(b accountTransferBundle, password string) (accountTransferDocument, error) {
	var doc accountTransferDocument
	if !transferPasswordOK(password) || b.Format != accountTransferFormat || b.Version != 1 || len(b.Salt) != 16 || len(b.Nonce) != 12 || len(b.Ciphertext) < 16 || len(b.Ciphertext) > 10<<20 {
		return doc, errors.New("迁移文件格式或密码无效")
	}
	aead, err := accountTransferCipher(password, b.Salt)
	if err != nil {
		return doc, errors.New("无法读取迁移文件")
	}
	raw, err := aead.Open(nil, b.Nonce, b.Ciphertext, []byte(accountTransferFormat+"/v1"))
	if err != nil {
		return doc, errors.New("密码错误或迁移文件已损坏")
	}
	if json.Unmarshal(raw, &doc) != nil || len(doc.Accounts) == 0 || len(doc.Accounts) > accountTransferLimit {
		return doc, errors.New("迁移文件账号数量无效")
	}
	seen := map[string]bool{}
	for _, item := range doc.Accounts {
		a, err := auth.Parse(item.Credential)
		if err != nil || a == nil || !validTransferUID(a.UID) || len(item.Credential) > 64<<10 || len(item.Groups) > 100 {
			return doc, errors.New("迁移文件包含无效账号")
		}
		if seen[a.UID] {
			return doc, errors.New("迁移文件包含重复 UID")
		}
		seen[a.UID] = true
	}
	return doc, nil
}
func (h *Handler) beginAccountTransfer(w http.ResponseWriter) bool {
	w.Header().Set("Cache-Control", "no-store")
	if h.cfg.Pool == nil {
		transferError(w, 503, "账号池未启用")
		return false
	}
	if !h.accountTransferMu.TryLock() {
		transferError(w, 429, "另一项账号迁移正在处理，请稍后重试")
		return false
	}
	return true
}
func (h *Handler) exportAccounts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		UIDs     []string `json:"uids"`
		Password string   `json:"password"`
	}
	if !decodeTransfer(w, r, &req) {
		return
	}
	uids, err := transferUIDs(req.UIDs)
	if err != nil {
		transferError(w, 400, err.Error())
		return
	}
	if !transferPasswordOK(req.Password) {
		transferError(w, 400, "迁移密码需要 12–1024 字节")
		return
	}
	if !h.beginAccountTransfer(w) {
		return
	}
	defer h.accountTransferMu.Unlock()
	doc := accountTransferDocument{CreatedAt: time.Now().UTC()}
	h.accountAdminMu.Lock()
	for _, uid := range uids {
		a := h.cfg.Pool.AuthByUID(uid)
		st, ok := h.cfg.Pool.Status(uid)
		if a == nil || !ok {
			h.accountAdminMu.Unlock()
			transferError(w, 409, "选中的账号已变化，请刷新列表后重试")
			return
		}
		c := a.Snapshot()
		raw, _ := json.Marshal(map[string]any{"uid": c.UID, "nickname": c.Nickname, "accessToken": c.AccessToken, "refreshToken": c.RefreshToken, "domain": c.Domain, "enterpriseId": c.EnterpriseID, "expiresAt": c.ExpiresAt})
		gs := []string{"default"}
		if h.cfg.Groups != nil {
			gs = h.cfg.Groups.AccountGroups(uid)
			if len(gs) == 0 {
				gs = []string{"default"}
			}
		}
		doc.Accounts = append(doc.Accounts, transferredAccount{Credential: raw, Groups: gs, Disabled: st.Disabled})
	}
	h.accountAdminMu.Unlock()
	bundle, err := sealAccounts(doc, req.Password)
	if err != nil {
		transferError(w, 500, "生成迁移文件失败，可能包含过多账号")
		return
	}
	writeJSON(w, 200, bundle)
}
func (h *Handler) importGroups(a *auth.Auth, source []string) []string {
	if h.cfg.Groups == nil {
		return []string{"default"}
	}
	out := []string{}
	seen := map[string]bool{}
	for _, name := range source {
		for _, g := range h.cfg.Groups.ListDetailed() {
			if g.Name == name && (g.Region == "all" || g.Region == a.Region()) && !seen[name] {
				out = append(out, name)
				seen[name] = true
			}
		}
	}
	if len(out) == 0 {
		for _, g := range h.cfg.Groups.ListDetailed() {
			if g.Name == "default" && (g.Region == "all" || g.Region == a.Region()) {
				out = []string{"default"}
			}
		}
	}
	return out
}
func (h *Handler) importAccountExists(uid string) bool {
	if h.cfg.Pool.AuthByUID(uid) != nil {
		return true
	}
	_, err := os.Lstat(filepath.Join(h.cfg.AuthDir, "workbuddy-"+uid+".json"))
	return !os.IsNotExist(err)
}
func (h *Handler) previewAccountImport(w http.ResponseWriter, r *http.Request) {
	h.handleAccountImport(w, r, true)
}
func (h *Handler) importAccounts(w http.ResponseWriter, r *http.Request) {
	h.handleAccountImport(w, r, false)
}
func (h *Handler) handleAccountImport(w http.ResponseWriter, r *http.Request, preview bool) {
	var req accountImportRequest
	if !decodeTransfer(w, r, &req) {
		return
	}
	if !h.beginAccountTransfer(w) {
		return
	}
	defer h.accountTransferMu.Unlock()
	if h.cfg.AuthDir == "" {
		transferError(w, 503, "未配置账号存储目录")
		return
	}
	doc, err := openAccounts(req.Bundle, req.Password)
	if err != nil {
		transferError(w, 400, err.Error())
		return
	}
	h.accountAdminMu.Lock()
	defer h.accountAdminMu.Unlock()
	rows := []accountImportRow{}
	items := map[string]transferredAccount{}
	for _, item := range doc.Accounts {
		a, _ := auth.Parse(item.Credential)
		items[a.UID] = item
		row := accountImportRow{UID: a.UID, Nickname: a.Nickname, Region: a.Region(), Exists: h.importAccountExists(a.UID), SourceDisabled: item.Disabled, Groups: h.importGroups(a, item.Groups)}
		if len(row.Groups) == 0 {
			row.Issue = "无兼容目标分组，请先配置 default 区域或来源同名分组"
		}
		rows = append(rows, row)
	}
	if preview {
		writeJSON(w, 200, map[string]any{"accounts": rows, "count": len(rows), "created_at": doc.CreatedAt})
		return
	}
	uids, err := transferUIDs(req.UIDs)
	if err != nil {
		transferError(w, 400, err.Error())
		return
	}
	for _, uid := range uids {
		if _, ok := items[uid]; !ok {
			transferError(w, 400, "选择的账号不在迁移文件中")
			return
		}
	}
	if err := os.MkdirAll(h.cfg.AuthDir, 0700); err != nil {
		transferError(w, 500, "无法访问账号存储目录")
		return
	}
	// Persist mixed-region support before creating any credentials.
	if err := h.promoteMixedRegion(); err != nil {
		transferError(w, 500, "无法保存混合区域配置，尚未导入账号")
		return
	}
	results := []map[string]any{}
	success := 0
	skipped := 0
	for _, uid := range uids {
		if r.Context().Err() != nil {
			break
		}
		if h.importAccountExists(uid) {
			results = append(results, map[string]any{"uid": uid, "ok": false, "skipped": true, "detail": "目标已有该账号或授权文件，未覆盖"})
			skipped++
			continue
		}
		item := items[uid]
		a, _ := auth.Parse(item.Credential)
		path := filepath.Join(h.cfg.AuthDir, "workbuddy-"+uid+".json")
		if len(h.importGroups(a, item.Groups)) == 0 {
			results = append(results, map[string]any{"uid": uid, "ok": false, "detail": "目标没有区域兼容的分组，未导入"})
			continue
		}
		// The marker is in the same atomic file as the credential, so a crash
		// before Pool.Flush cannot activate an imported account on restart.
		var credentialDoc map[string]any
		json.Unmarshal(item.Credential, &credentialDoc)
		credentialDoc["_import_disabled"] = true
		credentialRaw, _ := json.Marshal(credentialDoc)
		if err := writeNewAccountFile(path, credentialRaw); err != nil {
			results = append(results, map[string]any{"uid": uid, "ok": false, "detail": "写入授权文件失败，未导入"})
			continue
		}
		a.FilePath = path
		a.ImportDisabled = true
		if !h.cfg.Pool.AddDisabled(a, "imported; awaiting administrator enable") {
			os.Remove(path)
			results = append(results, map[string]any{"uid": uid, "ok": false, "skipped": true, "detail": "账号已存在，未覆盖"})
			skipped++
			continue
		}
		detail := "已导入并停用，核对后可手动启用"
		if h.cfg.Groups != nil {
			if err := h.setAccountGroupsChecked(uid, h.importGroups(a, item.Groups)); err != nil {
				detail += "；分组保存失败，请手动设置"
			}
		}
		success++
		if h.cfg.Collection != nil {
			h.cfg.Collection.Forget(uid)
		}
		results = append(results, map[string]any{"uid": uid, "ok": true, "detail": detail})
	}
	h.cfg.Pool.Flush()
	invalidateDynamicModelsCache()
	writeJSON(w, 200, map[string]any{"ok": success == len(uids), "success": success, "skipped": skipped, "results": results})
}

// Link a fully synced temporary file into place without overwriting any target.
func writeNewAccountFile(path string, raw []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".account-import-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(raw); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Link(f.Name(), path)
}
