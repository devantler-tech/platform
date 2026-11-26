package main

import "errors"

type recoveryCiliumMonitorConfig struct {
	Notifications, Audit bool
}

// Use realized options inside the identity-bearing native endpoint-get status.
// A separate configuration lookup by reusable numeric ID cannot establish that
// options belong to the same CNI attachment observed in another response.
func bindRecoveryCiliumMonitorConfig(o object) (recoveryCiliumMonitorConfig, error) {
	var cfg recoveryCiliumMonitorConfig
	refused := errors.New("native endpoint notifications or audit mode are not proven")
	if _, present := o["error"]; present {
		return cfg, refused
	}
	if text(o, "realized", "options", "DropNotification") != "Enabled" || text(o, "realized", "options", "PolicyAuditMode") != "Disabled" {
		return cfg, refused
	}
	return recoveryCiliumMonitorConfig{Notifications: true, Audit: false}, nil
}
