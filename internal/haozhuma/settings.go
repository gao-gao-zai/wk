package haozhuma

import (
	"context"
	"errors"
	"strings"
)

// Settings 保留旧 sms.haozhuma 配置路径；豪猪项目 ID 和 E码云关键词独立。
type Settings struct {
	Credentials      *PlatformCredentials `json:"credentials,omitempty"`
	Provider         string               `json:"provider"`
	User             string               `json:"user"`
	Pass             string               `json:"pass"`
	Token            string               `json:"token"`
	Sid              string               `json:"sid"`
	Author           string               `json:"author"`
	UID              string               `json:"uid"`
	UIDs             []string             `json:"uids"`
	ISP              string               `json:"isp"`
	H5Session        string               `json:"h5_session"`
	Mode             string               `json:"mode"`
	EcodeBase        string               `json:"ecode_base"`
	EcodeKeyword     string               `json:"ecode_keyword"`
	EcodeCardType    string               `json:"ecode_card_type"`
	EcodeProvince    string               `json:"ecode_province"`
	Fallback         string               `json:"fallback"`
	FallbackBase     string               `json:"fallback_base"`
	FallbackCardType string               `json:"fallback_card_type"`
	FallbackProvince string               `json:"fallback_province"`
}

// PlatformCredentials 分开保存两家的凭据；旧 user/pass/token 仅作为当前平台的兼容投影。
type PlatformCredentials struct {
	Haozhuma Credentials `json:"haozhuma"`
	Ecode    Credentials `json:"ecode"`
}

type Credentials struct {
	User  string `json:"user,omitempty"`
	Pass  string `json:"pass,omitempty"`
	Token string `json:"token,omitempty"`
}

// Normalized 迁移旧配置并生成当前平台投影，不修改调用方或其他客户端的配置。
func (s Settings) Normalized() Settings {
	s.Provider = strings.TrimSpace(s.Provider)
	if s.Provider == "" {
		s.Provider = "haozhuma"
	}
	if s.Credentials == nil {
		s.Credentials = &PlatformCredentials{}
		if s.Provider == "ecode" {
			s.Credentials.Ecode.Token = s.Token
		} else {
			s.Credentials.Haozhuma = Credentials{User: s.User, Pass: s.Pass, Token: s.Token}
			s.Credentials.Ecode.Token = s.Fallback
			// 旧的兜底参数迁入独立 E码云配置，之后切换主平台也复用它们。
			if s.FallbackBase != "" {
				s.EcodeBase = s.FallbackBase
			}
			if s.FallbackCardType != "" {
				s.EcodeCardType = s.FallbackCardType
			}
			if s.FallbackProvince != "" {
				s.EcodeProvince = s.FallbackProvince
			}
		}
	} else {
		copy := *s.Credentials
		s.Credentials = &copy
	}
	active := s.Credentials.Haozhuma
	if s.Provider == "ecode" {
		active = s.Credentials.Ecode
	}
	s.User, s.Pass, s.Token = active.User, active.Pass, active.Token
	if s.Fallback != "" {
		s.Fallback = s.Credentials.Ecode.Token
	}
	s.FallbackBase, s.FallbackCardType, s.FallbackProvince = s.EcodeBase, s.EcodeCardType, s.EcodeProvince
	return s
}

func (s Settings) Project() string {
	if s.Provider == "ecode" && s.EcodeKeyword != "" {
		return s.EcodeKeyword
	}
	return s.Sid
}

func (s Settings) Validate() error {
	if s.Provider != "" && s.Provider != "haozhuma" && s.Provider != "ecode" {
		return errors.New("接码平台只能是 haozhuma 或 ecode")
	}
	if !validMode(s.Mode) {
		return errors.New("无效的取号模式")
	}
	for _, base := range []string{s.EcodeBase, s.FallbackBase} {
		if base != "" && !validEndpoint(base) {
			return errors.New("E码云 API 地址必须是无凭据、无查询参数的 HTTP(S) 地址")
		}
	}
	for _, card := range []string{s.EcodeCardType, s.FallbackCardType} {
		if card != "" && card != "全部" && card != "实卡" && card != "虚卡" {
			return errors.New("卡类型只能是全部、实卡或虚卡")
		}
	}
	if s.Fallback != "" && strings.TrimSpace(s.EcodeKeyword) == "" {
		return errors.New("启用 E码云兜底前请填写独立的短信关键词")
	}
	if s.Provider == "ecode" && s.EcodeKeyword == "" && strings.Trim(s.Sid, "0123456789") == "" {
		return errors.New("请填写 E码云短信关键词，不能使用豪猪数字项目 ID")
	}
	return nil
}

func samePrimary(a, b Settings) bool {
	return a.Provider == b.Provider && a.User == b.User && a.Pass == b.Pass && a.Token == b.Token &&
		a.EcodeBase == b.EcodeBase
}

func sameFallback(a, b Settings) bool {
	return a.Fallback == b.Fallback && a.FallbackBase == b.FallbackBase
}

// Build 先构造并验证新客户端，绝不修改 previous。未改变的凭据不重复请求供应商。
func Build(ctx context.Context, s Settings, haoBase string, previous *Client, verify bool) (*Client, error) {
	s = s.Normalized()
	s.User, s.Token, s.Sid = strings.TrimSpace(s.User), strings.TrimSpace(s.Token), strings.TrimSpace(s.Sid)
	s.EcodeKeyword, s.EcodeBase = strings.TrimSpace(s.EcodeKeyword), strings.TrimSpace(s.EcodeBase)
	s.Fallback, s.FallbackBase, s.Mode = strings.TrimSpace(s.Fallback), strings.TrimSpace(s.FallbackBase), strings.TrimSpace(s.Mode)
	s.Provider = strings.TrimSpace(s.Provider)
	if s.Provider == "" {
		s.Provider = "haozhuma"
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	var old *Settings
	if previous != nil {
		old = previous.settings
	}
	same := old != nil && samePrimary(*old, s)
	var c *Client
	if s.Provider == "ecode" {
		if strings.TrimSpace(s.Token) == "" {
			return nil, errors.New("请填写 E码云 token")
		}
		c = NewEcode(s.EcodeBase, s.Token)
		c.Keyword = s.EcodeKeyword
		if c.Keyword == "" {
			c.Keyword = s.Sid
		}
		c.CardType, c.Province = s.EcodeCardType, s.EcodeProvince
	} else {
		if s.User == "" && s.Token == "" {
			return nil, errors.New("豪猪账号与 token 至少填写一个")
		}
		c = NewWithCredentials(s.Token, s.User, s.Pass)
		if haoBase != "" {
			c.Base = haoBase
		}
		if same {
			c.Token = previous.tokenValue()
		}
		if s.User != "" && (!same || c.Token == "") {
			if err := c.Relogin(); err != nil {
				if s.Token == "" {
					return nil, err
				}
				if verify {
					if _, err := c.summaryDirect(ctx); err != nil {
						return nil, err
					}
				}
			}
		}
		c.Author, c.ISP = s.Author, s.ISP
		c.SetUID(s.UID)
		c.SetUIDs(s.UIDs)
	}
	if verify && !same && !(s.Provider != "ecode" && s.User != "") {
		if _, err := c.summaryDirect(ctx); err != nil {
			return nil, err
		}
	}
	if s.Provider != "ecode" && s.Fallback != "" {
		fb := NewEcode(s.FallbackBase, s.Fallback)
		fb.Keyword, fb.CardType, fb.Province = s.EcodeKeyword, s.FallbackCardType, s.FallbackProvince
		if verify && (old == nil || !sameFallback(*old, s)) {
			if _, err := fb.summaryDirect(ctx); err != nil {
				return nil, err
			}
		}
		if err := c.SetFallback(fb); err != nil {
			return nil, err
		}
	}
	if err := c.SetMode(s.Mode); err != nil {
		return nil, err
	}
	c.settings = &s
	return c, nil
}

func (c *Client) Project() string {
	if c.settings != nil {
		return c.settings.Project()
	}
	return ""
}
