package server

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"workbuddy2api/internal/haozhuma"
)

func (a *AutoEnroller) CurrentSMSClient() *haozhuma.Client {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.hzm
}
func (a *AutoEnroller) SMSConfigured() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.hzm != nil && a.sid != ""
}

func (a *AutoEnroller) HasHaozhuma() bool {
	c := a.CurrentSMSClient()
	return c != nil && c.ValidateMode("haozhuma") == nil
}
func (a *AutoEnroller) FallbackMounted() bool {
	c := a.CurrentSMSClient()
	return c != nil && c.Fallback() != nil
}
func (a *AutoEnroller) ProviderMode() string {
	c := a.CurrentSMSClient()
	if c == nil {
		return "auto"
	}
	return c.Mode()
}

// ApplySMSUpdate 在同一运行态锁下校验、写盘和发布客户端。
func (a *AutoEnroller) ApplySMSUpdate(replacement *haozhuma.Client, mode *string, persist func() error) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if replacement != nil || a.clientPreparing {
		if err := a.smsBusyLocked(); err != nil {
			return err
		}
	}
	target := a.hzm
	if replacement != nil {
		target = replacement
	}
	if mode != nil {
		if target == nil {
			return errors.New("请先配置接码平台凭据")
		}
		if err := target.ValidateMode(strings.TrimSpace(*mode)); err != nil {
			return err
		}
	}
	if err := persist(); err != nil {
		return err
	}
	if mode != nil {
		_ = target.SetMode(*mode)
	}
	if replacement != nil {
		a.hzm = replacement
		if project := replacement.Project(); project != "" {
			a.sid = project
		}
	}
	return nil
}

func smsSettingsFromDoc(doc map[string]any) haozhuma.Settings {
	var c struct {
		SMS struct {
			Haozhuma haozhuma.Settings `json:"haozhuma"`
		} `json:"sms"`
	}
	raw, _ := json.Marshal(doc)
	_ = json.Unmarshal(raw, &c)
	return c.SMS.Haozhuma
}
func (h *Handler) currentSMSSettings() haozhuma.Settings {
	raw, err := os.ReadFile(h.cfg.ConfigPath)
	if err != nil {
		return haozhuma.Settings{}
	}
	var doc map[string]any
	_ = json.Unmarshal(raw, &doc)
	return smsSettingsFromDoc(doc)
}
func supplierPatchPresent(p *smsPatch) bool {
	if p == nil {
		return false
	}
	s := p.Haozhuma
	return s.Profile != nil || s.User != nil || s.Pass != nil || s.Token != nil || s.Provider != nil ||
		s.EcodeBase != nil || s.EcodeKeyword != nil || s.EcodeCardType != nil ||
		s.EcodeProvince != nil || s.Fallback != nil || s.FallbackBase != nil ||
		s.FallbackCardType != nil || s.FallbackProvince != nil || s.FallbackEnabled != nil
}

// 在更改 provider 前保存旧平台凭据，避免把旧 token 误归到新平台。
func normalizeSMSDoc(doc map[string]any) {
	sms, _ := doc["sms"].(map[string]any)
	if sms == nil {
		sms = map[string]any{}
	}
	hz, _ := sms["haozhuma"].(map[string]any)
	if hz == nil {
		hz = map[string]any{}
	}
	s := smsSettingsFromDoc(doc).Normalized()
	hz["credentials"] = s.Credentials
	hz["user"], hz["pass"], hz["token"] = s.User, s.Pass, s.Token
	hz["ecode_base"], hz["ecode_card_type"], hz["ecode_province"] = s.EcodeBase, s.EcodeCardType, s.EcodeProvince
	hz["fallback_base"], hz["fallback_card_type"], hz["fallback_province"] = s.EcodeBase, s.EcodeCardType, s.EcodeProvince
	hz["fallback"] = s.Fallback
	sms["haozhuma"], doc["sms"] = hz, sms
}

func mergeSMSCredentials(doc map[string]any, p *smsPatch) {
	if p == nil {
		return
	}
	sms, _ := doc["sms"].(map[string]any)
	if sms == nil {
		sms = map[string]any{}
	}
	hz, _ := sms["haozhuma"].(map[string]any)
	if hz == nil {
		hz = map[string]any{}
	}
	s := smsSettingsFromDoc(doc)
	if s.Credentials == nil {
		s = s.Normalized()
	}
	creds := *s.Credentials
	active := &creds.Haozhuma
	target := s.Provider
	if p.Haozhuma.Profile != nil {
		target = *p.Haozhuma.Profile
	}
	if target == "ecode" {
		active = &creds.Ecode
	}
	for key, value := range map[string]*string{"user": p.Haozhuma.User, "pass": p.Haozhuma.Pass, "token": p.Haozhuma.Token} {
		if value != nil {
			v := *value
			if key != "pass" {
				v = strings.TrimSpace(v)
			}
			switch key {
			case "user":
				active.User = v
			case "pass":
				active.Pass = v
			case "token":
				active.Token = v
			}
		}
	}
	if active.User != "" && target != "ecode" &&
		(p.Haozhuma.User != nil || p.Haozhuma.Pass != nil || p.Haozhuma.Token != nil) {
		active.Token = ""
	}
	if p.Haozhuma.Fallback != nil && strings.TrimSpace(*p.Haozhuma.Fallback) != "" {
		creds.Ecode.Token = strings.TrimSpace(*p.Haozhuma.Fallback)
	}
	// 兼容旧前端的兜底参数补丁；E码云统一使用一套独立参数。
	for key, value := range map[string]*string{"ecode_base": p.Haozhuma.FallbackBase, "ecode_card_type": p.Haozhuma.FallbackCardType, "ecode_province": p.Haozhuma.FallbackProvince} {
		if value != nil {
			hz[key] = strings.TrimSpace(*value)
		}
	}
	if p.Haozhuma.FallbackEnabled != nil {
		if *p.Haozhuma.FallbackEnabled {
			if creds.Ecode.Token == "" {
				hz["fallback"] = ""
			} else {
				hz["fallback"] = creds.Ecode.Token
			}
		} else {
			hz["fallback"] = ""
		}
	}
	hz["credentials"] = &creds
	sms["haozhuma"] = hz
	doc["sms"] = sms
	normalizeSMSDoc(doc)
}
