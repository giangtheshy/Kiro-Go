package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"kiro-go/auth"
	"kiro-go/config"
	"kiro-go/logger"
	"net/http"
	neturl "net/url"
	"strings"
	"sync"
	"time"
)

const (
	kiroRestAPIBase               = "https://codewhisperer.us-east-1.amazonaws.com"
	profileArnUnsupportedCooldown = 24 * time.Hour

	// profileArnMissingCooldown is the negative-cache TTL for the "no available
	// Kiro profile" case (empty profile list across all regions). A 5-minute
	// window stops each request from re-probing 2 regions × 3 retries + a token
	// refresh every time while still allowing fast recovery when a profile is
	// provisioned or an outage clears.
	profileArnMissingCooldown = 5 * time.Minute
)

var profileArnResolutionCooldowns sync.Map

// kiroProfileRegions lists the AWS regions where Kiro / Amazon Q Developer
// provisions profiles. ListAvailableProfiles is regional, so when an account's
// configured region yields no profile we probe these in order. us-east-1
// (N. Virginia) and eu-central-1 (Frankfurt) are the only regions Amazon Q
// Developer is generally available in as of 2026; extend this list as AWS adds
// regions. The SSO/auth region can differ from the profile region — an IdC
// instance in us-east-1 may own a profile in eu-central-1 (cross-region app).
var kiroProfileRegions = []string{"us-east-1", "eu-central-1"}

// regionFromProfileArn extracts the AWS region embedded in a CodeWhisperer
// profile ARN, e.g. "arn:aws:codewhisperer:eu-central-1:123:profile/ABC" →
// "eu-central-1". Returns "" when the ARN is empty or malformed. The profile
// ARN is the authoritative source of an account's data-plane region.
func regionFromProfileArn(profileArn string) string {
	arn := strings.TrimSpace(profileArn)
	if arn == "" {
		return ""
	}
	// arn:aws:codewhisperer:<region>:<account>:profile/<id>
	parts := strings.Split(arn, ":")
	if len(parts) < 4 {
		return ""
	}
	if parts[0] != "arn" || parts[2] != "codewhisperer" {
		return ""
	}
	return strings.TrimSpace(parts[3])
}

// kiroRegion returns the AWS region the account's Kiro profile lives in,
// defaulting to us-east-1 when unset. AWS provisions Kiro / Q Developer
// profiles per region, so a profile such as KiroProfile-eu-central-1 only
// resolves against its own regional endpoint. Every data-plane call must
// therefore target the account's region rather than a hardcoded one.
//
// Resolution order:
//  1. the region embedded in the profile ARN (authoritative once resolved),
//  2. the account's effective API region (account.apiRegion > region > global),
//  3. us-east-1.
func kiroRegion(account *config.Account) string {
	if account != nil {
		if r := regionFromProfileArn(account.ProfileArn); r != "" {
			return r
		}
		if r := strings.TrimSpace(account.EffectiveApiRegion()); r != "" {
			return r
		}
	}
	return "us-east-1"
}

// regionalizeURL points a hardcoded us-east-1 Kiro endpoint at the account's
// region. Amazon Q is regional (q.{region}.amazonaws.com), but the CodeWhisperer
// REST host only exists in us-east-1 — non-us-east-1 accounts are served by the
// regional Amazon Q host instead. So for those accounts both us-east-1 hosts map
// to q.{region}. It is a no-op for us-east-1 accounts.
func regionalizeURL(rawURL string, account *config.Account) string {
	return regionalizeURLForRegion(rawURL, kiroRegion(account))
}

// regionalizeURLForRegion rewrites the hardcoded us-east-1 Kiro hosts to target
// an explicit region. It is a no-op for us-east-1 (and empty) regions. Used both
// by regionalizeURL (account-driven) and by region probing, which must build a
// URL for a region the account is not yet pinned to.
func regionalizeURLForRegion(rawURL, region string) string {
	region = strings.TrimSpace(region)
	if region == "" || region == "us-east-1" {
		return rawURL
	}
	regionalHost := "q." + region + ".amazonaws.com"
	return strings.NewReplacer(
		"q.us-east-1.amazonaws.com", regionalHost,
		"codewhisperer.us-east-1.amazonaws.com", regionalHost,
	).Replace(rawURL)
}

// accountEmailForLog returns a nil-safe string for logging account identity.
func accountEmailForLog(account *config.Account) string {
	if account == nil {
		return "<nil>"
	}
	return account.Email
}

// ensureRestProfileArn resolves the account profile ARN before REST calls,
// softly skipping when resolution is unsupported (e.g. Builder ID accounts).
// api_key accounts authenticate via API_KEY tokentype and do not need a profileArn.
func ensureRestProfileArn(account *config.Account) error {
	if account == nil || strings.TrimSpace(account.ProfileArn) != "" {
		return nil
	}
	// api_key accounts don't need profileArn — they auth with tokentype: API_KEY
	if account.AuthMethod == "api_key" {
		return nil
	}
	profileArn, err := ResolveProfileArn(account)
	if err != nil {
		if isProfileArnResolutionSoftError(err) {
			logger.Debugf("[ProfileArn] Continuing REST request without profile ARN for %s: %v", accountEmailForLog(account), err)
			return nil
		}
		return err
	}
	account.ProfileArn = profileArn
	return nil
}

// GetUsageLimits 获取账户使用量和订阅信息
func GetUsageLimits(account *config.Account) (*UsageLimitsResponse, error) {
	// Same guard as ListAvailableModels: a relay key is meaningless at the real AWS
	// host, so this call would answer 403 and trigger an auth-failure ban.
	if account != nil && account.IsRelayCredential() {
		return nil, ErrModelListingUnsupported
	}

	if err := ensureRestProfileArn(account); err != nil {
		return nil, fmt.Errorf("resolve profileArn: %w", err)
	}

	url := fmt.Sprintf("%s/getUsageLimits?origin=AI_EDITOR&resourceType=AGENTIC_REQUEST&isEmailRequired=true", kiroRestAPIBase)
	url = regionalizeURL(url, account)
	url = withProfileArnQuery(url, account)

	resp, err := doRESTWithProxySwap(account, func() (*http.Request, error) {
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return nil, err
		}
		setKiroHeaders(req, account)
		if account.AuthMethod == "external_idp" {
			req.Header.Set("TokenType", "EXTERNAL_IDP")
		}
		return req, nil
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result UsageLimitsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// GetUserInfo 获取用户信息
func GetUserInfo(account *config.Account) (*UserInfoResponse, error) {
	url := regionalizeURL(fmt.Sprintf("%s/GetUserInfo", kiroRestAPIBase), account)

	payload := `{"origin":"KIRO_IDE"}`
	resp, err := doRESTWithProxySwap(account, func() (*http.Request, error) {
		req, err := http.NewRequest("POST", url, strings.NewReader(payload))
		if err != nil {
			return nil, err
		}
		setKiroHeaders(req, account)
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result UserInfoResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return &result, nil
}

// ListAvailableModels 获取可用模型列表
// ErrModelListingUnsupported reports that this credential has no model-listing
// endpoint to query, so there is nothing to fetch — as opposed to a fetch that
// was attempted and failed.
//
// Callers must treat it as a no-op and specifically must NOT pass it to
// handleAccountFailure. See the guard in ListAvailableModels for what happened
// when they did.
var ErrModelListingUnsupported = errors.New("model listing unsupported for this credential")

func ListAvailableModels(account *config.Account) ([]ModelInfo, error) {
	// Guarded here, not at the call sites. There are three of them and the earlier
	// fix only covered one, which produced exactly the failure this guard prevents:
	//
	// ListAvailableModels targets the real AWS host (kiroRestAPIBase), never the
	// relay. A relay-issued key is meaningless there, so AWS answers
	// 403 "The bearer token included in the request is invalid." That reaches
	// handleAccountFailure, isAuthErrorMessage matches the 403, and a perfectly
	// working relay account gets disabled and marked BANNED — on a timer, roughly
	// ten seconds after startup and every thirty minutes after that, with no
	// request from anyone needed to trigger it.
	if account != nil && account.IsRelayCredential() {
		return nil, ErrModelListingUnsupported
	}

	if err := ensureRestProfileArn(account); err != nil {
		return nil, fmt.Errorf("resolve profileArn: %w", err)
	}

	url := fmt.Sprintf("%s/ListAvailableModels?origin=AI_EDITOR&maxResults=50", kiroRestAPIBase)
	url = regionalizeURL(url, account)
	url = withProfileArnQuery(url, account)

	resp, err := doRESTWithProxySwap(account, func() (*http.Request, error) {
		req, err := http.NewRequest("GET", url, nil)
		if err != nil {
			return nil, err
		}
		setKiroHeaders(req, account)
		if account.AuthMethod == "external_idp" {
			req.Header.Set("TokenType", "EXTERNAL_IDP")
		}
		return req, nil
	})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Models []ModelInfo `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	return result.Models, nil
}

// ResolveProfileArn returns the account profile ARN, fetching and caching it
// when it is missing. It probes ListAvailableProfiles across the regions Kiro
// supports (starting with the account's configured region), because Q Developer
// profiles are regional and an account's profile may live in a region other than
// its SSO/auth region. If no region yields a profile, it falls back to refreshing
// the token (whose response carries a profileArn). Builder ID "unsupported"
// errors are cached for 24h to avoid redundant failing calls.
//
// On success the resolved ARN and its owning region are persisted together so
// future data-plane calls target the correct regional endpoint directly.
func ResolveProfileArn(account *config.Account) (string, error) {
	if account == nil {
		return "", fmt.Errorf("account is nil")
	}
	if profileArn := strings.TrimSpace(account.ProfileArn); profileArn != "" {
		return profileArn, nil
	}

	profileLookupSuppressed := isProfileArnResolutionSuppressed(account)
	var profileUnsupportedErr error
	var profileUnsupported bool

	if !profileLookupSuppressed {
		// Probe ListAvailableProfiles across candidate regions. The first region
		// is the account's configured one; the rest are the remaining supported
		// Kiro regions, so a profile provisioned outside the SSO region is found.
		for _, region := range profileProbeRegions(account) {
			profileArn, err := listAvailableProfilesWithRetry(account, region)
			if err == nil && profileArn != "" {
				if updateErr := config.UpdateAccountProfileArnWithRegion(account.ID, profileArn, region); updateErr != nil {
					logger.Warnf("[ProfileArn] Failed to cache profile ARN for %s: %v", account.Email, updateErr)
				}
				account.ProfileArn = profileArn
				if account.ApiRegion == "" {
					account.ApiRegion = region
				}
				if region != defaultProbeRegion(account) {
					logger.Infof("[ProfileArn] Resolved profile for %s in region %s (differs from configured region)", accountEmailForLog(account), region)
				}
				return profileArn, nil
			}
			// Builder ID is unsupported regardless of region — stop probing.
			if isBuilderIDProfileUnsupportedError(account, err) {
				profileUnsupportedErr = err
				profileUnsupported = true
				break
			}
			profileUnsupportedErr = err
		}
	}

	// Fallback: refresh token to get profileArn from auth response.
	// Persist any rotated credentials even when no ARN comes back — some IdC
	// providers rotate the refresh token on every use, so discarding the new
	// token here would invalidate the account on the next refresh cycle.
	if account.RefreshToken != "" {
		newAccess, newRefresh, newExpiry, refreshedArn, refreshErr := auth.RefreshToken(account)
		if refreshErr == nil {
			// Persist rotated credentials regardless of whether an ARN was returned.
			if newAccess != "" || newRefresh != "" {
				storeRefresh := newRefresh
				if storeRefresh == "" {
					storeRefresh = account.RefreshToken
				}
				if err := config.UpdateAccountToken(account.ID, newAccess, storeRefresh, newExpiry); err != nil {
					logger.Warnf("[ProfileArn] Failed to persist rotated token for %s: %v", account.Email, err)
				} else {
					account.AccessToken = newAccess
					account.ExpiresAt = newExpiry
					if newRefresh != "" {
						account.RefreshToken = newRefresh
					}
				}
			}
			if refreshedArn != "" {
				region := regionFromProfileArn(refreshedArn)
				if updateErr := config.UpdateAccountProfileArnWithRegion(account.ID, refreshedArn, region); updateErr != nil {
					logger.Warnf("[ProfileArn] Failed to cache profile ARN for %s: %v", account.Email, updateErr)
				}
				account.ProfileArn = refreshedArn
				if region != "" && account.ApiRegion == "" {
					account.ApiRegion = region
				}
				return refreshedArn, nil
			}
		}
	}

	if profileLookupSuppressed {
		return "", fmt.Errorf("profile ARN resolution skipped: previous Builder ID profile lookup was unsupported")
	}
	if profileUnsupported {
		suppressProfileArnResolution(account, profileArnUnsupportedCooldown)
		logger.Debugf("[ProfileArn] Builder ID profile lookup unsupported for %s: %v", accountEmailForLog(account), profileUnsupportedErr)
		return "", fmt.Errorf("profile ARN unsupported for Builder ID account")
	}

	// No profile was found in any region and the refresh token fallback also
	// yielded nothing. Cache this negative result so subsequent requests skip the
	// expensive multi-region probe + token refresh for the next 5 minutes instead
	// of redoing it on every single request.
	suppressProfileArnResolution(account, profileArnMissingCooldown)
	logger.Warnf("[ProfileArn] No available Kiro profile for %s (will suppress re-probe for %.0fm)", accountEmailForLog(account), profileArnMissingCooldown.Minutes())
	return "", fmt.Errorf("no available Kiro profile")
}

// defaultProbeRegion returns the region ResolveProfileArn tries first: the
// account's effective API region, or us-east-1.
func defaultProbeRegion(account *config.Account) string {
	if account != nil {
		if r := strings.TrimSpace(account.EffectiveApiRegion()); r != "" {
			return r
		}
	}
	return "us-east-1"
}

// profileProbeRegions returns the ordered, de-duplicated list of regions to
// probe for an account's profile: its configured region first, then the other
// supported Kiro regions.
func profileProbeRegions(account *config.Account) []string {
	first := defaultProbeRegion(account)
	regions := []string{first}
	seen := map[string]bool{first: true}
	for _, r := range kiroProfileRegions {
		if !seen[r] {
			regions = append(regions, r)
			seen[r] = true
		}
	}
	return regions
}

func isBuilderIDProfileUnsupportedError(account *config.Account, err error) bool {
	if account == nil || err == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(account.Provider), "BuilderId") {
		return false
	}
	msg := err.Error()
	return strings.HasPrefix(msg, "HTTP 403") && strings.Contains(msg, "AWS Builder ID is not supported for this operation")
}

func profileArnCooldownKey(account *config.Account) string {
	if account == nil {
		return ""
	}
	provider := strings.TrimSpace(account.Provider)
	if id := strings.TrimSpace(account.ID); id != "" {
		return provider + "\x00" + id
	}
	if userID := strings.TrimSpace(account.UserId); userID != "" {
		return provider + "\x00" + userID
	}
	return provider + "\x00" + strings.TrimSpace(account.Email)
}

func suppressProfileArnResolution(account *config.Account, cooldown time.Duration) {
	key := profileArnCooldownKey(account)
	if key == "" {
		return
	}
	profileArnResolutionCooldowns.Store(key, time.Now().Add(cooldown))
}

func isProfileArnResolutionSuppressed(account *config.Account) bool {
	key := profileArnCooldownKey(account)
	if key == "" {
		return false
	}
	value, ok := profileArnResolutionCooldowns.Load(key)
	if !ok {
		return false
	}
	until, ok := value.(time.Time)
	if !ok || time.Now().After(until) {
		profileArnResolutionCooldowns.Delete(key)
		return false
	}
	return true
}

func isProfileArnResolutionSkippedError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "profile ARN resolution skipped")
}

func isProfileArnResolutionUnsupportedError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "profile ARN unsupported for Builder ID account")
}

func isProfileArnResolutionMissingError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "no available Kiro profile")
}

func isProfileArnResolutionSoftError(err error) bool {
	return isProfileArnResolutionSkippedError(err) ||
		isProfileArnResolutionUnsupportedError(err) ||
		isProfileArnResolutionMissingError(err)
}

func listAvailableProfilesWithRetry(account *config.Account, region string) (string, error) {
	// Retry transient failures (network errors, 5xx, 429) with short backoff.
	// An empty profile list or 4xx (other than 429) is treated as authoritative
	// and not retried — they reflect account state, not upstream flakiness.
	const maxAttempts = 3
	backoff := 200 * time.Millisecond

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		profileArn, err := listAvailableProfiles(account, region)
		if err == nil {
			return profileArn, nil
		}
		lastErr = err
		if !isTransientProfileFetchError(err) || attempt == maxAttempts {
			return "", err
		}
		logger.Debugf("[ProfileArn] ListAvailableProfiles transient failure for %s in %s (attempt %d/%d): %v",
			account.Email, region, attempt, maxAttempts, err)
		time.Sleep(backoff)
		backoff *= 2
	}
	return "", lastErr
}

// isTransientProfileFetchError reports whether a ListAvailableProfiles error
// is worth retrying. Network errors and upstream 5xx/429 are transient; other
// HTTP errors and an empty profile list are not.
func isTransientProfileFetchError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if strings.Contains(msg, "empty profile list") {
		return false
	}
	if strings.HasPrefix(msg, "HTTP ") {
		return strings.HasPrefix(msg, "HTTP 5") || strings.HasPrefix(msg, "HTTP 429")
	}
	// Non-HTTP errors are network/transport level — retry.
	return true
}

func listAvailableProfiles(account *config.Account, region string) (string, error) {
	url := regionalizeURLForRegion(fmt.Sprintf("%s/ListAvailableProfiles", kiroRestAPIBase), region)
	resp, err := doRESTWithProxySwap(account, func() (*http.Request, error) {
		req, err := http.NewRequest("POST", url, strings.NewReader(`{"maxResults":10}`))
		if err != nil {
			return nil, err
		}
		setKiroHeaders(req, account)
		req.Header.Set("Content-Type", "application/json")
		if account.AuthMethod == "external_idp" {
			req.Header.Set("TokenType", "EXTERNAL_IDP")
		}
		return req, nil
	})
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		Profiles []struct {
			Arn string `json:"arn"`
		} `json:"profiles"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", err
	}
	for _, profile := range result.Profiles {
		if profileArn := strings.TrimSpace(profile.Arn); profileArn != "" {
			return profileArn, nil
		}
	}
	return "", fmt.Errorf("empty profile list")
}

func withProfileArnQuery(rawURL string, account *config.Account) string {
	if account == nil {
		return rawURL
	}
	profileArn := strings.TrimSpace(account.ProfileArn)
	if profileArn == "" {
		return rawURL
	}
	return rawURL + "&profileArn=" + neturl.QueryEscape(profileArn)
}

func setKiroHeaders(req *http.Request, account *config.Account) {
	host := ""
	if req.URL != nil {
		host = req.URL.Host
	}
	headerValues := buildRuntimeHeaderValues(account, host)

	req.Header.Set("Accept", "application/json")
	applyKiroBaseHeaders(req, account, headerValues)
}

// RefreshAccountInfo 刷新账户信息（使用量、订阅等）
// ErrAccountInfoUnsupported reports that this account has no usage/subscription
// REST surface to read, so there is nothing to refresh — as opposed to a refresh
// that was attempted and failed.
//
// Callers should treat it as a no-op, not an error worth logging at warn level.
var ErrAccountInfoUnsupported = errors.New("account info refresh unsupported for this credential")

func RefreshAccountInfo(account *config.Account) (*config.AccountInfo, error) {
	// Guarded here rather than at each call site: there are seven of them, and the
	// next one added would quietly reintroduce the problem. It is not merely noise
	// either — GetUsageLimits answers 404 on a relay, and the token-error branch
	// below matches the bare substring "invalid", so a relay whose 404 body happened
	// to contain that word would be misread as having an expired token.
	if account != nil && account.IsRelayCredential() {
		return nil, ErrAccountInfoUnsupported
	}

	info := &config.AccountInfo{
		LastRefresh: time.Now().Unix(),
	}

	// 获取使用量和订阅信息
	usage, err := GetUsageLimits(account)
	if err != nil {
		// 检测封禁状态
		errMsg := err.Error()
		if strings.Contains(errMsg, "TEMPORARILY_SUSPENDED") {
			// 账户被暂时封禁，自动禁用并标记封禁状态
			logger.Warnf("[RefreshAccountInfo] Account %s is temporarily suspended: %v", account.Email, err)

			// 更新账户封禁状态并自动禁用
			updatedAccount := *account
			updatedAccount.Enabled = false
			updatedAccount.BanStatus = "BANNED"
			updatedAccount.BanReason = "AWS temporarily suspended - unusual user activity detected"
			updatedAccount.BanTime = time.Now().Unix()

			// 保存更新后的账户状态
			if updateErr := config.UpdateAccount(account.ID, updatedAccount); updateErr != nil {
				logger.Errorf("[RefreshAccountInfo] Failed to update account ban status: %v", updateErr)
			}

			return nil, fmt.Errorf("Account suspended: %w", err)
		} else if strings.Contains(errMsg, "403") || strings.Contains(errMsg, "401") ||
			strings.Contains(errMsg, "invalid") || strings.Contains(errMsg, "expired") {
			// Token 相关错误，可能需要重新认证
			// external_idp accounts: Microsoft-issued token may fail usage API but work for chat
			if account.AuthMethod == "external_idp" {
				logger.Warnf("[RefreshAccountInfo] Usage API auth error for external_idp account %s (token may not have usage scope): %v", account.Email, err)
				return nil, fmt.Errorf("GetUsageLimits: %w", err)
			}
			logger.Warnf("[RefreshAccountInfo] Authentication error for %s: %v", account.Email, err)

			// 更新账户封禁状态为认证失败并自动禁用
			updatedAccount := *account
			updatedAccount.Enabled = false
			updatedAccount.BanStatus = "BANNED"
			updatedAccount.BanReason = "Authentication failed - token invalid or expired"
			updatedAccount.BanTime = time.Now().Unix()

			// 保存更新后的账户状态
			if updateErr := config.UpdateAccount(account.ID, updatedAccount); updateErr != nil {
				logger.Errorf("[RefreshAccountInfo] Failed to update account ban status: %v", updateErr)
			}
		}

		return nil, fmt.Errorf("GetUsageLimits: %w", err)
	}

	// 如果成功获取信息，清除封禁状态（如果之前被标记）
	if account.BanStatus != "" && account.BanStatus != "ACTIVE" {
		logger.Infof("[RefreshAccountInfo] Account %s is now active, clearing ban status", account.Email)

		updatedAccount := *account
		updatedAccount.BanStatus = "ACTIVE"
		updatedAccount.BanReason = ""
		updatedAccount.BanTime = 0

		// 保存更新后的账户状态
		if updateErr := config.UpdateAccount(account.ID, updatedAccount); updateErr != nil {
			logger.Errorf("[RefreshAccountInfo] Failed to clear account ban status: %v", updateErr)
		}
	}

	// 解析用户信息
	if usage.UserInfo != nil {
		info.Email = usage.UserInfo.Email
		info.UserId = usage.UserInfo.UserId
	}

	// 解析订阅信息
	if usage.SubscriptionInfo != nil {
		// 优先从 SubscriptionTitle 或 SubscriptionName 解析类型
		titleOrName := usage.SubscriptionInfo.SubscriptionTitle
		if titleOrName == "" {
			titleOrName = usage.SubscriptionInfo.SubscriptionName
		}
		if titleOrName == "" {
			titleOrName = usage.SubscriptionInfo.SubscriptionType
		}
		info.SubscriptionType = parseSubscriptionType(titleOrName)
		info.SubscriptionTitle = usage.SubscriptionInfo.SubscriptionTitle
		if info.SubscriptionTitle == "" {
			info.SubscriptionTitle = usage.SubscriptionInfo.SubscriptionName
		}
		logger.Debugf("[RefreshAccountInfo] Subscription: type=%s, title=%s, name=%s, parsed=%s",
			usage.SubscriptionInfo.SubscriptionType,
			usage.SubscriptionInfo.SubscriptionTitle,
			usage.SubscriptionInfo.SubscriptionName,
			info.SubscriptionType)
	}

	// 解析使用量
	if len(usage.UsageBreakdownList) > 0 {
		breakdown := usage.UsageBreakdownList[0]
		info.UsageCurrent = breakdown.CurrentUsage
		info.UsageLimit = breakdown.UsageLimit
		if info.UsageLimit > 0 {
			info.UsagePercent = info.UsageCurrent / info.UsageLimit
		}
	}

	// 解析重置日期
	if usage.NextDateReset != "" {
		if ts, err := usage.NextDateReset.Int64(); err == nil && ts > 0 {
			info.NextResetDate = time.Unix(ts, 0).Format("2006-01-02")
		} else if f, err := usage.NextDateReset.Float64(); err == nil && f > 0 {
			info.NextResetDate = time.Unix(int64(f), 0).Format("2006-01-02")
		}
	}

	// 解析试用配额信息
	if len(usage.UsageBreakdownList) > 0 {
		breakdown := usage.UsageBreakdownList[0]
		if breakdown.FreeTrialInfo != nil {
			info.TrialUsageCurrent = breakdown.FreeTrialInfo.CurrentUsage
			info.TrialUsageLimit = breakdown.FreeTrialInfo.UsageLimit
			if info.TrialUsageLimit > 0 {
				info.TrialUsagePercent = info.TrialUsageCurrent / info.TrialUsageLimit
			}
			info.TrialStatus = breakdown.FreeTrialInfo.FreeTrialStatus

			// 解析试用到期时间
			if breakdown.FreeTrialInfo.FreeTrialExpiry != "" {
				if ts, err := breakdown.FreeTrialInfo.FreeTrialExpiry.Int64(); err == nil && ts > 0 {
					info.TrialExpiresAt = ts
				} else if f, err := breakdown.FreeTrialInfo.FreeTrialExpiry.Float64(); err == nil && f > 0 {
					info.TrialExpiresAt = int64(f)
				}
			}
		}
	}

	return info, nil
}

func parseSubscriptionType(raw string) string {
	upper := strings.ToUpper(raw)
	if strings.Contains(upper, "PRO_PLUS") || strings.Contains(upper, "PROPLUS") {
		return "PRO_PLUS"
	}
	if strings.Contains(upper, "POWER") {
		return "POWER"
	}
	if strings.Contains(upper, "PRO") {
		return "PRO"
	}
	return "FREE"
}

// 响应结构体
type UsageLimitsResponse struct {
	UsageBreakdownList []UsageBreakdown  `json:"usageBreakdownList"`
	NextDateReset      json.Number       `json:"nextDateReset"`
	SubscriptionInfo   *SubscriptionInfo `json:"subscriptionInfo"`
	UserInfo           *UserInfo         `json:"userInfo"`
}

type UsageBreakdown struct {
	ResourceType  string         `json:"resourceType"`
	CurrentUsage  float64        `json:"currentUsage"`
	UsageLimit    float64        `json:"usageLimit"`
	Currency      string         `json:"currency"`
	Unit          string         `json:"unit"`
	OverageRate   float64        `json:"overageRate"`
	FreeTrialInfo *FreeTrialInfo `json:"freeTrialInfo"`
	Bonuses       []BonusInfo    `json:"bonuses"`
}

type FreeTrialInfo struct {
	CurrentUsage    float64     `json:"currentUsage"`
	UsageLimit      float64     `json:"usageLimit"`
	FreeTrialStatus string      `json:"freeTrialStatus"`
	FreeTrialExpiry json.Number `json:"freeTrialExpiry"`
}

type BonusInfo struct {
	BonusCode    string      `json:"bonusCode"`
	DisplayName  string      `json:"displayName"`
	CurrentUsage float64     `json:"currentUsage"`
	UsageLimit   float64     `json:"usageLimit"`
	ExpiresAt    json.Number `json:"expiresAt"`
	Status       string      `json:"status"`
}

type SubscriptionInfo struct {
	SubscriptionName  string `json:"subscriptionName"`
	SubscriptionTitle string `json:"subscriptionTitle"`
	SubscriptionType  string `json:"subscriptionType"`
	Status            string `json:"status"`
	UpgradeCapability string `json:"upgradeCapability"`
}

type UserInfo struct {
	Email  string `json:"email"`
	UserId string `json:"userId"`
}

type UserInfoResponse struct {
	Email  string `json:"email"`
	UserId string `json:"userId"`
	Idp    string `json:"idp"`
	Status string `json:"status"`
}

type ModelInfo struct {
	ModelId        string   `json:"modelId"`
	ModelName      string   `json:"modelName"`
	Description    string   `json:"description"`
	InputTypes     []string `json:"supportedInputTypes"`
	RateMultiplier float64  `json:"rateMultiplier"`
	TokenLimits    *struct {
		MaxInputTokens  int `json:"maxInputTokens"`
		MaxOutputTokens int `json:"maxOutputTokens"`
	} `json:"tokenLimits"`
}
