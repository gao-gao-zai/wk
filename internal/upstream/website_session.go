package upstream

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"
	"workbuddy2api/internal/auth"
)

type websiteSession struct {
	jar         http.CookieJar
	created     time.Time
	unsupported bool
}

func (c *Client) websiteEnabled() bool { return c.WebAuthMode == "auto" || c.WebAuthMode == "cookie" }

func websiteBusinessPath(path string) bool {
	return strings.HasPrefix(path, "/activity/growth/") || strings.HasPrefix(path, "/v2/activity/growth/") || path == "/billing/meter/claim-gift" || path == "/billing/meter/claim-compensation"
}

func (c *Client) websiteBase(a *auth.Auth) string {
	if a.Region() == auth.RegionGlobal {
		return c.BillingBaseGlob
	}
	return c.webBase()
}

func (c *Client) websiteKey(a *auth.Auth) string {
	s := a.Snapshot()
	raw, _ := json.Marshal([]any{s.UID, s.EnterpriseID, s.Authority, s.Domain, s.AccessToken, s.RefreshToken, c.chatBase(a), c.websiteBase(a)})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func (c *Client) storeWebsite(key string, session websiteSession) {
	c.webMu.Lock()
	defer c.webMu.Unlock()
	if c.webSessions == nil {
		c.webSessions = make(map[string]websiteSession)
	}
	if _, found := c.webSessions[key]; !found && len(c.webSessions) >= 4096 {
		var oldest string
		var at time.Time
		for candidate, entry := range c.webSessions {
			if at.IsZero() || entry.created.Before(at) {
				oldest, at = candidate, entry.created
			}
		}
		delete(c.webSessions, oldest)
	}
	c.webSessions[key] = session
}

func hasWebsiteCookie(jar http.CookieJar, base string) bool {
	if jar == nil {
		return false
	}
	u, err := url.Parse(base)
	if err != nil {
		return false
	}
	for _, cookie := range jar.Cookies(u) {
		if (cookie.Name == "session" || cookie.Name == "session_2") && cookie.Value != "" {
			return true
		}
	}
	return false
}

func (c *Client) cachedWebsite(key, base string) (websiteSession, bool) {
	c.webMu.Lock()
	entry, ok := c.webSessions[key]
	c.webMu.Unlock()
	if !ok {
		return entry, false
	}
	if entry.unsupported {
		return entry, time.Since(entry.created) < 5*time.Minute
	}
	return entry, time.Since(entry.created) < 24*time.Hour && hasWebsiteCookie(entry.jar, base)
}

func (c *Client) websiteSessionFor(ctx context.Context, a *auth.Auth, page string) (websiteSession, error) {
	if scope := c.growthRouteFor(a); scope != nil {
		if err := scope.check(); err != nil {
			return websiteSession{}, err
		}
		scope.webMu.Lock()
		defer scope.webMu.Unlock()
		if err := scope.check(); err != nil {
			return websiteSession{}, err
		}
		key, base := c.websiteKey(a), c.websiteBase(a)
		if entry, ok := scope.webSessions[key]; ok {
			if (entry.unsupported && time.Since(entry.created) < 5*time.Minute) || (!entry.unsupported && time.Since(entry.created) < 24*time.Hour && hasWebsiteCookie(entry.jar, base)) {
				return entry, nil
			}
		}
		// Task login is owned by this exact private route and cancellation
		// scope. It cannot join an ordinary account's shared login flight.
		entry, err := c.openWebsite(ctx, a, page)
		if err == nil {
			err = scope.check()
		}
		if err != nil && c.WebAuthMode == "auto" && scope.check() == nil && legacyReadAllowed(ctx, err) {
			entry, err = websiteSession{created: time.Now(), unsupported: true}, nil
		}
		if err == nil {
			if scope.webSessions == nil {
				scope.webSessions = map[string]websiteSession{}
			}
			scope.webSessions[key] = entry
		}
		return entry, err
	}
	key, base := c.websiteKey(a), c.websiteBase(a)
	if entry, ok := c.cachedWebsite(key, base); ok {
		return entry, nil
	}
	result := c.webLogins.DoChan(key, func() (any, error) {
		if entry, ok := c.cachedWebsite(key, base); ok {
			return entry, nil
		}
		// Shared authorization has its own finite budget. Canceling one
		// caller must not cancel login for the other waiting requests.
		flightCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
		defer cancel()
		entry, err := c.openWebsite(flightCtx, a, page)
		if err != nil && c.WebAuthMode == "auto" && legacyReadAllowed(flightCtx, err) {
			entry, err = websiteSession{created: time.Now(), unsupported: true}, nil
		}
		if err == nil {
			c.storeWebsite(key, entry)
		}
		return entry, err
	})
	select {
	case <-ctx.Done():
		return websiteSession{}, ctx.Err()
	case r := <-result:
		if r.Err != nil {
			return websiteSession{}, r.Err
		}
		return r.Val.(websiteSession), nil
	}
}

func (c *Client) openWebsite(ctx context.Context, a *auth.Auth, page string) (websiteSession, error) {
	entry := websiteSession{created: time.Now()}
	s := a.Snapshot()
	if s.UID == "" || s.RefreshToken == "" {
		return entry, fmt.Errorf("website session credentials unavailable: %w", errReadSchema)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatBase(a)+"/v2/plugin/device/auth/code", nil)
	if err != nil {
		return entry, err
	}
	req.Header.Set("Content-Type", "application/json")
	// 登录码请求由官方后台服务主进程用 Node 内置 fetch 发：账号头照发，其余是
	// undici 自动补的（UA 是 "node"），不是请求框架那一套。
	applyNodeFetchHeaders(req)
	accountHeaders(req, a)
	req.Header.Set("X-Refresh-Token", s.RefreshToken)
	client, err := c.proxyClientForContext(ctx, a)
	if err != nil {
		return entry, err
	}
	codeClient := *client
	codeClient.Jar = nil
	// The refresh token is confined to the intended API endpoint even if it
	// unexpectedly responds with a redirect to a different service.
	codeClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	data, err := doJSONWithClient(&codeClient, req)
	if err != nil {
		return entry, &refreshStageError{stage: "website authorization", cause: err}
	}
	var code struct {
		Value string `json:"deviceCode"`
	}
	if json.Unmarshal(data, &code) != nil || code.Value == "" {
		return entry, fmt.Errorf("website authorization response: %w", errReadSchema)
	}
	jar, _ := cookiejar.New(&cookiejar.Options{PublicSuffixList: publicsuffix.List})
	base, err := url.Parse(c.websiteBase(a))
	if err != nil {
		return entry, err
	}
	navigation, err := url.Parse(c.websiteBase(a) + "/console/client-login")
	if err != nil {
		return entry, err
	}
	query := navigation.Query()
	query.Set("code", code.Value)
	query.Set("target", webLoginTarget(page))
	navigation.RawQuery = query.Encode()
	nav, err := http.NewRequestWithContext(ctx, http.MethodGet, navigation.String(), nil)
	if err != nil {
		return entry, err
	}
	applyWebNavigationHeaders(nav, a)
	browser := *client
	browser.Jar = jar
	browser.CheckRedirect = func(next *http.Request, via []*http.Request) error {
		if len(via) >= 8 || next.URL.Scheme != base.Scheme || !strings.EqualFold(next.URL.Host, base.Host) {
			return fmt.Errorf("website navigation outside configured origin")
		}
		next.Header.Del("Referer")
		next.Header.Del("Authorization")
		next.Header.Del("X-Refresh-Token")
		return nil
	}
	resp, navErr := doUpstream(&browser, nav)
	if resp != nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 256<<10))
		resp.Body.Close()
	}
	if ctx.Err() != nil {
		return entry, ctx.Err()
	}
	if hasWebsiteCookie(jar, base.String()) {
		entry.jar = jar
		return entry, nil
	}
	if navErr != nil {
		return entry, fmt.Errorf("website login navigation failed")
	}
	if resp != nil && resp.StatusCode >= 400 {
		return entry, &Error{Kind: Classify(resp.StatusCode, ""), Status: resp.StatusCode, Msg: "website login rejected"}
	}
	return entry, fmt.Errorf("website session cookie missing: %w", errReadSchema)
}

// handled=false means auto mode established that this API is unavailable,
// before any business action was sent. Transport/authentication errors never
// silently select a different authentication scheme.
func (c *Client) websiteJSON(ctx context.Context, a *auth.Auth, method, path string, body []byte, beacon bool, page string) (json.RawMessage, bool, error) {
	if !c.websiteEnabled() {
		return nil, false, nil
	}
	a = c.growthAuthSnapshot(a, a.Snapshot())
	uri, err := url.ParseRequestURI(path)
	if err != nil {
		return nil, true, err
	}
	if page == "" {
		page = originOf(c.websiteBase(a)) + webPageFor(uri.Path)
	}
	for attempt := 0; attempt < 2; attempt++ {
		entry, err := c.websiteSessionFor(ctx, a, page)
		if err != nil {
			return nil, true, err
		}
		if entry.unsupported {
			return nil, false, nil
		}
		requestPath := uri.EscapedPath()
		requestBody := body
		switch uri.Path {
		case "/v2/activity/growth/tasks/accept":
			requestPath = "/activity/growth/tasks/accept"
		case buddyFirstPath, "/billing/meter/claim-gift", "/billing/meter/claim-compensation":
			requestBody = nil
		case travelClaimPath:
			requestBody = []byte("{}")
		}
		if uri.RawQuery != "" {
			requestPath += "?" + uri.RawQuery
		}
		req, err := http.NewRequestWithContext(ctx, method, c.websiteBase(a)+requestPath, bytes.NewReader(requestBody))
		if err != nil {
			return nil, true, err
		}
		c.applyWebHeaders(req, a, uri.Path, page)
		if len(requestBody) != 0 {
			req.Header.Set("Content-Type", "application/json")
		}
		if beacon {
			req.Header.Set("Content-Type", "text/plain;charset=UTF-8")
		}
		client, err := c.proxyClientForContext(ctx, a)
		if err != nil {
			return nil, true, err
		}
		business := *client
		business.Jar = entry.jar
		business.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		data, err := doJSONWithClient(&business, req)
		var upstreamErr *Error
		if attempt == 0 && errors.As(err, &upstreamErr) && upstreamErr.Status == 401 && ctx.Err() == nil {
			// Invalidate only the generation that received 401. Another request
			// may already have installed a newer session while this was in flight.
			key := c.websiteKey(a)
			if scope := c.growthRouteFor(a); scope != nil {
				scope.webMu.Lock()
				if current := scope.webSessions[key]; current.jar == entry.jar {
					delete(scope.webSessions, key)
				}
				scope.webMu.Unlock()
			} else {
				c.webMu.Lock()
				if current := c.webSessions[key]; current.jar == entry.jar {
					delete(c.webSessions, key)
				}
				c.webMu.Unlock()
			}
			continue
		}
		return data, true, err
	}
	return nil, true, fmt.Errorf("website authentication failed")
}
