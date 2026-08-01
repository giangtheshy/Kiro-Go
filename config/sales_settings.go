package config

import "errors"

// Sales & operations settings getters/setters.
// These read/write the Config fields added in 2025-08 for the sales/ops tooling.

// GetAlertCreditsPercent returns the remaining-credits threshold (as a
// percentage of the limit) that triggers a "credit_low" alert. 0 is stored as
// the zero value; callers should treat it as the default 10%.
func GetAlertCreditsPercent() float64 {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.AlertCreditsPercent <= 0 {
		return 10.0
	}
	return cfg.AlertCreditsPercent
}

// GetAlertExpiryDays returns the days-until-expiry threshold for the
// "expiry_soon" alert. 0 stored → default 3 days.
func GetAlertExpiryDays() int {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil || cfg.AlertExpiryDays <= 0 {
		return 3
	}
	return cfg.AlertExpiryDays
}

// GetAlertWebhook returns the URL that receives alert POST payloads.
func GetAlertWebhook() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return ""
	}
	return cfg.AlertWebhook
}

// GetAutoDisableExpired reports whether the system should automatically disable
// keys that have passed their ExpiresAt timestamp.
func GetAutoDisableExpired() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.AutoDisableExpired
}

// GetMaintenanceMode reports whether maintenance mode is active.
func GetMaintenanceMode() bool {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return false
	}
	return cfg.MaintenanceMode
}

// GetMaintenanceMessage returns the maintenance notice shown to clients.
func GetMaintenanceMessage() string {
	cfgLock.RLock()
	defer cfgLock.RUnlock()
	if cfg == nil {
		return ""
	}
	if cfg.MaintenanceMessage != "" {
		return cfg.MaintenanceMessage
	}
	return "The service is temporarily down for maintenance. Please try again shortly."
}

// SetAlertSettings updates alert thresholds and the webhook URL.
func SetAlertSettings(creditsPercent float64, expiryDays int, webhook string, autoDisable bool) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	cfg.AlertCreditsPercent = creditsPercent
	cfg.AlertExpiryDays = expiryDays
	cfg.AlertWebhook = webhook
	cfg.AutoDisableExpired = autoDisable
	return saveLocked()
}

// SetMaintenanceMode enables or disables maintenance mode and sets the message.
func SetMaintenanceMode(enabled bool, message string) error {
	cfgLock.Lock()
	defer cfgLock.Unlock()
	if cfg == nil {
		return errors.New("config not initialized")
	}
	cfg.MaintenanceMode = enabled
	cfg.MaintenanceMessage = message
	return saveLocked()
}
