package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
)

const (
	resourceSummaryAPI = "/billing/meter/get-user-resource-summary"
	resourceFreeAPI    = "/billing/meter/get-user-resource-free-packages"
	resourcePaidAPI    = "/billing/meter/get-user-resource-paid-packages"
)

// Provider package identifiers are protocol data, not locally invented plans.
var freeResourceCodes = []string{
	"TCACA_code_001_PqouKr6QWV", "TCACA_code_008_cfWoLwvjU4", "TCACA_code_035_ArVxJcGDsm",
	"TCACA_code_006_DbXS0lrypC", "TCACA_code_039_KRcQj7wUat", "TCACA_code_040_mi9rCYg46x",
	"TCACA_code_007_nzdH5h4Nl0", "TCACA_code_028_NtpWi0jzXs", "TCACA_code_037_WxOD3MpI2o",
	"TCACA_code_029_6wCGEWquYy", "TCACA_code_030_BjSt89qTvr",
}

var paidResourceCodes = []string{
	"TCACA_code_002_AkiJS3ZHF5", "TCACA_code_005_maRGyrHhw1", "TCACA_code_003_FAnt7lcmRT",
	"TCACA_code_023_4xbGhMrE6q", "TCACA_code_026_BaESVICNoi", "TCACA_code_027_0FCGVA6vSa",
	"TCACA_code_043_6oIkxT7Z0S", "TCACA_code_009_0XmEQc2xOf", "TCACA_code_038_OhvqZtiPKr",
	"TCACA_code_036_lupO5WgNdG",
}

type resourceDecimal struct{ value *big.Rat }

func (d *resourceDecimal) UnmarshalJSON(raw []byte) error {
	if string(raw) == "null" {
		d.value = nil
		return nil
	}
	s := string(raw)
	if len(s) > 128 {
		return errReadSchema
	}
	if strings.HasPrefix(s, `"`) {
		if json.Unmarshal(raw, &s) != nil {
			return errReadSchema
		}
	}
	// Accept decimal numbers/strings (including exponents), never fractions,
	// hexadecimal, NaN or infinity. Rat avoids rounding each package early.
	for _, r := range s {
		if !strings.ContainsRune("0123456789.eE+-", r) {
			return errReadSchema
		}
	}
	if _, err := strconv.ParseFloat(s, 64); err != nil {
		return errReadSchema
	}
	value, ok := new(big.Rat).SetString(s)
	if !ok {
		return errReadSchema
	}
	if value.Sign() < 0 {
		value.SetInt64(0)
	}
	d.value = value
	return nil
}

func resourceValue(precise, legacy resourceDecimal) *big.Rat {
	if precise.value != nil {
		return new(big.Rat).Set(precise.value)
	}
	if legacy.value != nil {
		return new(big.Rat).Set(legacy.value)
	}
	return new(big.Rat)
}

type resourcePackage struct {
	ID               string          `json:"ResourceId"`
	Code             string          `json:"PackageCode"`
	CapacityType     int             `json:"CapacityType"`
	Total            resourceDecimal `json:"CycleCapacitySizePrecise"`
	Left             resourceDecimal `json:"CycleCapacityRemainPrecise"`
	LegacyTotal      resourceDecimal `json:"CycleCapacitySize"`
	LegacyLeft       resourceDecimal `json:"CycleCapacityRemain"`
	CapacitySize     resourceDecimal `json:"CapacitySize"`
	CapacityRemain   resourceDecimal `json:"CapacityRemain"`
	DeductionEndTime int64           `json:"DeductionEndTime"`
	Slices           []struct {
		Total resourceDecimal `json:"SlicePeriodCapacitySizePrecise"`
		Left  resourceDecimal `json:"SlicePeriodCapacityRemainPrecise"`
	} `json:"SlicePeriodUsageDetails"`
}

func (p resourcePackage) amounts() (*big.Rat, *big.Rat) {
	total, left := resourceValue(p.Total, p.LegacyTotal), resourceValue(p.Left, p.LegacyLeft)
	if p.Total.value == nil && p.LegacyTotal.value == nil {
		total, left = resourceValue(p.CapacitySize, resourceDecimal{}), resourceValue(p.CapacityRemain, resourceDecimal{})
	}
	if p.CapacityType == 4 && len(p.Slices) != 0 {
		total, left = resourceValue(p.Slices[0].Total, resourceDecimal{}), resourceValue(p.Slices[0].Left, resourceDecimal{})
	}
	if left.Cmp(total) > 0 {
		left.Set(total)
	}
	return total, left
}

type balanceSummary struct {
	Packages []struct {
		Code  string          `json:"PackageCode"`
		Total resourceDecimal `json:"CycleTotalCapacity"`
		Left  resourceDecimal `json:"CycleRemainCapacity"`
	} `json:"Packages"`
	Subscription string          `json:"SubscriptionPackageCode"`
	Paid         json.RawMessage `json:"IsPaidUser"`
}

func isFreeResourceCode(code string) bool {
	for _, candidate := range freeResourceCodes {
		if code == candidate {
			return true
		}
	}
	return false
}

func (s *balanceSummary) freeOnly() bool {
	if s == nil || len(s.Packages) == 0 || (s.Subscription != "" && !isFreeResourceCode(s.Subscription)) {
		return false
	}
	switch string(s.Paid) {
	case "false", "0", `""`, "null", "":
	default:
		return false
	}
	for _, p := range s.Packages {
		if !isFreeResourceCode(p.Code) {
			return false
		}
	}
	return true
}

func (c *Client) UserResourceDetailsContext(ctx context.Context, a *auth.Auth) (usage ResourceUsage, err error) {
	a = c.growthAuthSnapshot(a, a.Snapshot())
	trace := &balanceTrace{}
	ctx = context.WithValue(ctx, balanceTraceKey{}, trace)
	defer func() {
		if err != nil {
			noteBalanceFailure(ctx, "final", "result", err)
			trace.logFailure(a.UID)
		}
	}()
	// The new package contract is researched for personal billing only.
	if a.EnterpriseID != "" {
		return c.legacyResourceDetails(ctx, a)
	}
	usage, err = c.personalResourceDetails(ctx, a)
	if err != nil && legacyReadAllowed(ctx, err) {
		noteBalanceFailure(ctx, "personal", "before_legacy_fallback", err)
		return c.legacyResourceDetails(ctx, a)
	}
	return usage, err
}

func (c *Client) resourceJSON(ctx context.Context, a *auth.Auth, path string, body any) (json.RawMessage, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.billingBase(a)+path, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	c.billingHeaders(req, a)
	return c.doJSON(req, a)
}

func (c *Client) personalResourceDetails(ctx context.Context, a *auth.Auth) (ResourceUsage, error) {
	data, summaryErr := c.resourceJSON(ctx, a, resourceSummaryAPI, map[string]any{})
	var summary *balanceSummary
	if summaryErr == nil {
		var parsed balanceSummary
		if parseErr := json.Unmarshal(data, &parsed); parseErr != nil {
			noteBalanceFailure(ctx, "summary", "decode", parseErr)
		} else if parsed.Packages != nil {
			summary = &parsed
		} else {
			noteBalanceFailure(ctx, "summary", "packages_missing_or_null", errReadSchema)
		}
	} else {
		var e *Error
		if ctx.Err() != nil || errors.Is(summaryErr, ErrNoProxyNode) || legacyReadAllowed(ctx, summaryErr) || (errors.As(summaryErr, &e) && e.Status < 500) {
			return ResourceUsage{}, summaryErr
		}
	}
	// Keep all requests on the existing account route; no retry or new polling
	// loop is introduced. CN free-only accounts need no paid-package request.
	packages, err := c.resourcePages(ctx, a, resourceFreeAPI)
	if err != nil {
		return ResourceUsage{}, err
	}
	if a.Region() == auth.RegionGlobal || !summary.freeOnly() {
		paid, err := c.resourcePages(ctx, a, resourcePaidAPI)
		if err != nil {
			return ResourceUsage{}, err
		}
		packages = append(packages, paid...)
	}
	total, left := new(big.Rat), new(big.Rat)
	if summary != nil && len(summary.Packages) != 0 {
		for _, p := range summary.Packages {
			if p.Total.value == nil || p.Left.value == nil {
				summary = nil
				break
			}
			total.Add(total, p.Total.value)
			left.Add(left, p.Left.value)
		}
	} else {
		summary = nil
	}
	if summary == nil {
		total.SetInt64(0)
		left.SetInt64(0)
		seen := make(map[string]bool)
		var unique []resourcePackage
		for _, p := range packages {
			if p.ID != "" {
				if seen[p.ID] {
					continue
				}
				seen[p.ID] = true
			}
			unique = append(unique, p)
		}
		best := -1
		for i, p := range unique {
			if subscriptionRank(p.Code, a.Region()) >= 0 && (best < 0 || preferSubscription(p, unique[best], a.Region())) {
				best = i
			}
		}
		for i, p := range unique {
			if subscriptionRank(p.Code, a.Region()) >= 0 && i != best {
				continue
			}
			t, l := p.amounts()
			total.Add(total, t)
			left.Add(left, l)
		}
	}
	if left.Cmp(total) > 0 {
		left.Set(total)
	}
	used := new(big.Rat).Sub(total, left)
	t, l, u := roundedResource(total), roundedResource(left), roundedResource(used)
	return ResourceUsage{Remaining: l, RemainingPrecise: left.FloatString(6), CapacitySize: t, CapacityRemain: l, CapacityUsed: u, CycleCapacitySize: t, CycleCapacityRemain: l, CycleCapacityUsed: u}, nil
}

func (c *Client) resourcePages(ctx context.Context, a *auth.Auth, path string) ([]resourcePackage, error) {
	var all []resourcePackage
	for page := 1; page <= 10; page++ {
		codes := freeResourceCodes
		body := map[string]any{"PageNumber": page, "PageSize": 200, "Status": []int{0, 3}}
		if path == resourcePaidAPI {
			codes = paidResourceCodes
			body["NeedRenewInfo"] = true
		} else {
			day := time.Now().In(time.FixedZone("billing", 8*3600)).Format("2006-01-02")
			body["SlicePeriodStartTime"] = day + " 00:00:00"
			body["SlicePeriodEndTime"] = day + " 23:59:59"
		}
		body["PackageCodes"] = codes
		data, err := c.resourceJSON(ctx, a, path, body)
		if err != nil {
			return nil, err
		}
		var response struct {
			Accounts []resourcePackage `json:"Accounts"`
			Total    *int              `json:"TotalCount"`
		}
		if parseErr := json.Unmarshal(data, &response); parseErr != nil {
			noteBalanceFailure(ctx, balanceStage(path), "decode", parseErr)
			return nil, fmt.Errorf("resource packages: %w", errReadSchema)
		}
		if response.Accounts == nil {
			noteBalanceFailure(ctx, balanceStage(path), "accounts_missing_or_null", errReadSchema)
			return nil, fmt.Errorf("resource packages: %w", errReadSchema)
		}
		all = append(all, response.Accounts...)
		if response.Total != nil {
			if *response.Total < 0 || (len(response.Accounts) == 0 && len(all) < *response.Total) {
				noteBalanceFailure(ctx, balanceStage(path), "incomplete_page", errReadSchema)
				return nil, fmt.Errorf("incomplete resource package page")
			}
			if len(all) >= *response.Total {
				return all, nil
			}
		} else if len(response.Accounts) < 200 {
			return all, nil
		}
	}
	noteBalanceFailure(ctx, balanceStage(path), "pagination_limit", errReadSchema)
	return nil, fmt.Errorf("resource packages exceed pagination limit")
}

func roundedResource(v *big.Rat) int64 {
	n := new(big.Rat).Add(v, big.NewRat(1, 2))
	integer := new(big.Int).Quo(n.Num(), n.Denom())
	if !integer.IsInt64() {
		return int64(^uint64(0) >> 1)
	}
	return integer.Int64()
}

func subscriptionRank(code, region string) int {
	switch code {
	case "TCACA_code_043_6oIkxT7Z0S":
		return 6
	case "TCACA_code_027_0FCGVA6vSa":
		return 5
	case "TCACA_code_026_BaESVICNoi":
		return 4
	case "TCACA_code_002_AkiJS3ZHF5", "TCACA_code_005_maRGyrHhw1", "TCACA_code_003_FAnt7lcmRT":
		return 3
	case "TCACA_code_023_4xbGhMrE6q":
		return 2
	case "TCACA_code_039_KRcQj7wUat", "TCACA_code_040_mi9rCYg46x":
		return 1
	case "TCACA_code_035_ArVxJcGDsm":
		return 0
	case "TCACA_code_008_cfWoLwvjU4":
		if region == auth.RegionCN {
			return 0
		}
	}
	return -1
}

func preferSubscription(a, b resourcePackage, region string) bool {
	if x, y := subscriptionRank(a.Code, region), subscriptionRank(b.Code, region); x != y {
		return x > y
	}
	at, al := a.amounts()
	bt, bl := b.amounts()
	if (al.Sign() > 0) != (bl.Sign() > 0) {
		return al.Sign() > 0
	}
	if cmp := at.Cmp(bt); cmp != 0 {
		return cmp > 0
	}
	return a.DeductionEndTime < b.DeductionEndTime
}
