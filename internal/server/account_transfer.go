package server

import (
	"context"
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
const accountCredentialBytes = 64 << 10
const accountTransferPlainBytes = 10 << 20

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
	Bundle         accountTransferBundle `json:"bundle"`
	Credential     json.RawMessage       `json:"credential,omitempty"`
	CredentialJSON string                `json:"credential_json,omitempty"`
	Password       string                `json:"password"`
	UIDs           []string              `json:"uids"`
	GroupMode      string                `json:"group_mode,omitempty"`
	Groups         []string              `json:"groups,omitempty"`
	Nickname       string                `json:"nickname,omitempty"`
	RunGrowthTasks bool                  `json:"run_growth_tasks,omitempty"`
}
type accountImportRow struct {
	UID            string   `json:"uid"`
	Nickname       string   `json:"nickname"`
	Region         string   `json:"region"`
	Exists         bool     `json:"exists"`
	SourceDisabled bool     `json:"source_disabled"`
	Groups         []string `json:"groups"`
	Issue          string   `json:"issue,omitempty"`
	DeviceAction   string   `json:"device_action"`
	DevicePlatform string   `json:"device_platform"`
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
	if len(raw) > accountTransferPlainBytes {
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
	if !transferPasswordOK(password) || b.Format != accountTransferFormat || b.Version != 1 || len(b.Salt) != 16 || len(b.Nonce) != 12 || len(b.Ciphertext) < 16 || len(b.Ciphertext) > accountTransferPlainBytes+16 {
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
	if json.Unmarshal(raw, &doc) != nil {
		return doc, errors.New("迁移文件内容无效")
	}
	return doc, validateTransferDocument(doc)
}

// Both export and import use the same limits. Group memberships are bounded by
// the document size, rather than an import-only per-account group limit.
func validateTransferDocument(doc accountTransferDocument) error {
	if len(doc.Accounts) == 0 || len(doc.Accounts) > accountTransferLimit {
		return errors.New("迁移文件账号数量无效")
	}
	seen := map[string]bool{}
	for _, item := range doc.Accounts {
		if len(item.Credential) > accountCredentialBytes {
			return errors.New("单个账号凭据超过 64 KiB，请缩小后重试")
		}
		a, err := auth.Parse(item.Credential)
		if err != nil || a == nil || !validTransferUID(a.UID) {
			return errors.New("迁移文件包含无效账号")
		}
		if seen[a.UID] {
			return errors.New("迁移文件包含重复 UID")
		}
		seen[a.UID] = true
	}
	return nil
}

func openAccountImport(req accountImportRequest) (accountTransferDocument, error) {
	if req.CredentialJSON != "" {
		if len(req.Credential) != 0 {
			return accountTransferDocument{}, errors.New("请只提供一种原始凭据格式")
		}
		req.Credential = json.RawMessage(req.CredentialJSON)
	}
	if len(req.Credential) == 0 {
		return openAccounts(req.Bundle, req.Password)
	}
	b := req.Bundle
	if b.Format != "" || b.Version != 0 || len(b.Salt) != 0 || len(b.Nonce) != 0 || len(b.Ciphertext) != 0 {
		return accountTransferDocument{}, errors.New("请只提供账号凭据或加密迁移文件中的一种")
	}
	var obj map[string]json.RawMessage
	var token, account map[string]json.RawMessage
	if len(req.Credential) > accountCredentialBytes || json.Unmarshal(req.Credential, &obj) != nil ||
		json.Unmarshal(obj["auth"], &token) != nil || len(token) == 0 ||
		json.Unmarshal(obj["account"], &account) != nil || len(account) == 0 {
		return accountTransferDocument{}, errors.New("账号文件须为 auth + account JSON，且不超过 64 KiB")
	}
	doc := accountTransferDocument{Accounts: []transferredAccount{{Credential: req.Credential, Groups: []string{"default"}}}}
	return doc, validateTransferDocument(doc)
}
func (h *Handler) beginAccountTransfer(w http.ResponseWriter) bool {
	w.Header().Set("Cache-Control", "no-store")
	if h.cfg.Pool == nil {
		transferError(w, 503, "账号池未启用")
		return false
	}
	if h.cfg.Pool.SharedError() != nil {
		transferError(w, 503, "账号池存储不可用，请恢复后重新预览")
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
		a.DeviceIdentity()
		c := a.Snapshot()
		c.DeviceProfile = a.Device()
		credential := map[string]any{"uid": c.UID, "nickname": c.Nickname, "accessToken": c.AccessToken, "refreshToken": c.RefreshToken, "domain": c.Domain, "enterpriseId": c.EnterpriseID, "expiresAt": c.ExpiresAt}
		c.AddAuthorityMetadata(credential, credential)
		c.AddDeviceMetadata(credential)
		raw, _ := json.Marshal(credential)
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
	if h.cfg.Pool.SharedError() != nil {
		transferError(w, 503, "账号池存储不可用，未生成迁移文件")
		return
	}
	if err := validateTransferDocument(doc); err != nil {
		transferError(w, 400, err.Error())
		return
	}
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
	compatible := map[string]bool{}
	for _, g := range h.cfg.Groups.ListDetailed() {
		compatible[g.Name] = g.Region == "all" || g.Region == a.Region()
	}
	for _, name := range source {
		if compatible[name] && !seen[name] {
			out = append(out, name)
			seen[name] = true
		}
	}
	if len(out) == 0 && compatible["default"] {
		out = []string{"default"}
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
	doc, err := openAccountImport(req)
	if err != nil {
		transferError(w, 400, err.Error())
		return
	}
	if err := normalizeImportOptions(&req, &doc); err != nil {
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
		row := accountImportRow{UID: a.UID, Nickname: a.Nickname, Region: a.Region(), Exists: h.importAccountExists(a.UID), SourceDisabled: item.Disabled, DeviceAction: "preserve", DevicePlatform: a.DeviceProfile.Platform}
		row.Groups, row.Issue = h.resolveImportGroups(a, item.Groups, req)
		if a.DeviceProfile.Version == 0 {
			row.DeviceAction, row.DevicePlatform = "generate_mac", "macos"
		}
		if row.Exists {
			row.DeviceAction = "skip"
		}
		rows = append(rows, row)
	}
	if h.cfg.Pool.SharedError() != nil {
		transferError(w, 503, "账号池存储不可用，请恢复后重新预览")
		return
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
	// Only change region for new, compatible accounts from another region.
	for _, row := range rows {
		if row.Exists || row.Issue != "" {
			continue
		}
		selected := false
		for _, uid := range uids {
			selected = selected || uid == row.UID
		}
		if selected {
			if err := h.promoteMixedRegionIfNeeded(row.Region); err != nil {
				transferError(w, 500, "无法保存混合区域配置，尚未导入账号")
				return
			}
		}
	}
	results := []map[string]any{}
	success := 0
	skipped := 0
	stop := false
	imported := map[string]*auth.Auth{}
	for _, uid := range uids {
		if stop || r.Context().Err() != nil {
			results = append(results, map[string]any{"uid": uid, "ok": false, "detail": "导入已中止，此账号未处理，请重新预览"})
			continue
		}
		exists := h.importAccountExists(uid)
		if h.cfg.Pool.SharedError() != nil {
			results = append(results, map[string]any{"uid": uid, "ok": false, "detail": "账号池存储不可用，此账号未导入"})
			stop = true
			continue
		}
		if exists {
			results = append(results, map[string]any{"uid": uid, "ok": false, "skipped": true, "detail": "目标已有该账号或授权文件，未覆盖"})
			skipped++
			continue
		}
		item := items[uid]
		a, _ := auth.Parse(item.Credential)
		path := filepath.Join(h.cfg.AuthDir, "workbuddy-"+uid+".json")
		targetGroups, groupIssue := h.resolveImportGroups(a, item.Groups, req)
		if groupIssue != "" {
			results = append(results, map[string]any{"uid": uid, "ok": false, "detail": groupIssue})
			continue
		}
		// The marker is in the same atomic file as the credential, so a crash
		// before Pool.Flush cannot activate an imported account on restart.
		a, credentialRaw, err := prepareImportedCredential(item.Credential)
		if err != nil {
			results = append(results, map[string]any{"uid": uid, "ok": false, "detail": err.Error()})
			continue
		}
		written := false
		if err := auth.WithCredentialWrite(r.Context(), path, func() error {
			if err := writeNewAccountFile(path, credentialRaw); err != nil {
				return err
			}
			written = true
			return nil
		}); err != nil {
			detail := "写入授权文件失败，未导入"
			if written {
				detail = "授权存储提交未确认，已保留停用凭据；请恢复存储后核对账号及授权文件"
				stop = true
			}
			results = append(results, map[string]any{"uid": uid, "ok": false, "pending": written, "detail": detail})
			continue
		}
		a.FilePath = path
		a.ImportDisabled = true
		added, addErr := h.cfg.Pool.AddDisabledChecked(a, "imported; awaiting administrator enable")
		if !added {
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
			cleanupErr := auth.WithCredentialWrite(cleanupCtx, path, func() error {
				if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
					return err
				}
				return nil
			})
			cleanupCancel()
			if addErr != nil || cleanupErr != nil {
				detail := "账号池存储不可用，未加入账号池"
				if cleanupErr != nil {
					detail += "；停用凭据清理未确认，请核对授权文件"
				}
				results = append(results, map[string]any{"uid": uid, "ok": false, "pending": cleanupErr != nil, "detail": detail})
				stop = true
				continue
			}
			results = append(results, map[string]any{"uid": uid, "ok": false, "skipped": true, "detail": "账号已存在，未覆盖"})
			skipped++
			continue
		}
		if addErr != nil {
			// A failed shared commit can have an ambiguous outcome. Never remove
			// the only credential for an account that may have been committed.
			results = append(results, map[string]any{"uid": uid, "ok": false, "pending": true, "detail": "账号池提交未确认，已保留停用凭据；请恢复存储后核对账号及授权文件"})
			stop = true
			continue
		}
		detail := "已导入并停用，核对后可手动启用"
		if h.cfg.Groups != nil {
			if err := h.setAccountGroupsChecked(uid, targetGroups); err != nil {
				results = append(results, map[string]any{"uid": uid, "ok": false, "pending": true, "detail": "账号已导入并停用，但分组保存失败，请核对分组后再启用"})
				stop = true
				continue
			}
		}
		success++
		imported[uid] = a
		if h.cfg.Collection != nil {
			h.cfg.Collection.Forget(uid)
		}
		results = append(results, map[string]any{"uid": uid, "ok": true, "detail": detail})
	}
	if err := h.cfg.Pool.FlushWithError(); err != nil {
		stop = true
		for _, row := range results {
			if row["ok"] == true {
				row["ok"], row["pending"] = false, true
				row["detail"] = "账号池持久化未确认，凭据保持停用标记；请恢复存储后核对账号及授权文件"
			}
		}
		success = 0
	}
	// Import success is committed before any optional upstream work can start.
	// Growth failures never roll back credentials or make quarantined accounts routable.
	if req.RunGrowthTasks {
		for _, row := range results {
			if row["ok"] != true {
				continue
			}
			uid, _ := row["uid"].(string)
			a := imported[uid]
			if a == nil {
				continue
			}
			if a.Region() != auth.RegionCN {
				row["growth_status"] = "unsupported_region"
				row["growth_detail"] = "海外版账号不支持成长任务，未排队"
				continue
			}
			if h.cfg.Upstream == nil {
				row["growth_status"] = "enqueue_failed"
				row["growth_detail"] = "成长任务服务未配置；账号已成功导入并保持停用"
				continue
			}
			job := h.growthJobs.newJob(uid, "account")
			row["growth_job_id"] = job.ID
			if !h.growthSubmitAccount(job, a) {
				job.finish("failed", "成长任务未能加入执行队列；可能已在队列或队列暂不可用，请稍后核对")
				row["growth_status"] = "enqueue_failed"
				row["growth_detail"] = "成长任务未入队；账号已成功导入并保持停用"
				continue
			}
			row["growth_status"] = "queued"
			row["growth_detail"] = "已排队检查并完成未完成成长任务；账号保持停用，关闭页面后继续执行"
		}
	}
	invalidateDynamicModelsCache()
	writeJSON(w, 200, map[string]any{"ok": success == len(uids), "success": success, "skipped": skipped, "stop": stop, "results": results})
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
