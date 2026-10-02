// Copyright (c) wncservices
// SPDX-License-Identifier: MPL-2.0

package omada

import (
	"context"
	"fmt"
	"net/http"
)

// Controller auto-backup (Global View -> Settings -> Backup & Restore).
//
// This is a controller-scoped SINGLETON document (DESIGN §2.7b): there is one
// auto-backup configuration per controller, addressed with no id, and it is
// read/replaced rather than created/deleted. The provider resource built on it
// therefore manages adoption rather than lifecycle.
//
//	GET /api/v2/autoBackup/autoBackupTask    read the configuration
//	PUT /api/v2/autoBackup/autoBackupTask    replace the configuration
//
// The read shape was confirmed read-only against a live v6.2.14.11 software
// controller, and the update verb was taken from the controller's own web app,
// whose store exposes `proxy.read = GET` and `proxy.update = PUT` for this exact
// path. The PUT was deliberately NOT exercised against the live controller in
// the discovery phase, so the exact accepted field set is unverified (see the
// schema notes); treat that as a live-validation follow-up.
//
// # Runtime fields are not configuration
//
// The GET response carries fields the controller computes and owns:
//
//	nowStatus                     the scheduler's current state (-1 when off)
//	dataSheets                    per-dataset backup metadata
//	fileServerConfig.serverConfig the credential-bearing destination list
//
// `serverConfig` holds FTP/TFTP/SCP credentials. Those are secrets that belong
// in a dedicated resource, not a general settings document (DESIGN §2.6), so
// this client never models them: the whole document is preserved verbatim on
// update from the live `raw` map. `nowStatus` and `dataSheets` are dropped from
// any write.
type AutoBackup struct {
	Enable     bool               `json:"enable"`
	Occurrence AutoBackupTiming   `json:"occurrence"`
	MaxFiles   int                `json:"maxNumberOfFile"`
	Retention  int                `json:"retention"`
	Type       string             `json:"type"`
	Retain     AutoBackupRetain   `json:"-"`
	FileServer AutoBackupFileServ `json:"fileServerConfig"`
}

// AutoBackupRetain groups the content-selection booleans. The controller keeps
// them at the top level of the JSON document, so the raw map round-trip is what
// actually preserves them; this struct is a typed read view.
//
// Retention semantics (verified from the controller UI's `backupRetentionS`
// store, read-only, not by a live PUT): `-1` = settings only; `0` = all history;
// a positive value = the number of **days** of data to keep (`7`, `30`, `60`,
// `90`, `180`, `365`). It is retained-history length, NOT backup-file lifetime
// — `maxNumberOfFile` governs how many files are kept. `0` is disabled on
// hardware controllers by the UI.
type AutoBackupRetain struct {
	Setting     bool
	User        bool
	AuthRecord  bool
	FirmwareLog bool
}

// AutoBackupTiming is when the automatic backup runs.
//
// `timingType` follows the same enum the UI uses elsewhere: 1 daily, 2 weekly,
// 3 monthly, 4 yearly. `hour` and `minute` are in the controller's own time
// zone (controller settings `general.timeZone`), NOT a site's — this is a
// controller-scoped document.
//
// Applicability (from the UI serializer): `dayOfWeek` applies when weekly,
// `dayOfMonth` when monthly, `monthOfYear` when yearly. The controller reads
// only the selector its timing type uses, so the others are preserved but
// otherwise inert.
type AutoBackupTiming struct {
	TimingType  int `json:"timingType"`
	Hour        int `json:"hour"`
	Minute      int `json:"minute"`
	DayOfWeek   int `json:"dayOfWeek,omitempty"`
	DayOfMonth  int `json:"dayOfMonth,omitempty"`
	MonthOfYear int `json:"monthOfYear,omitempty"`
}

// AutoBackupFileServ is the (optional) off-box file server destination. The
// credentials live in ServerConfig and are never modelled by Terraform.
type AutoBackupFileServ struct {
	Enable       bool   `json:"enable"`
	Protocol     string `json:"protocol"`
	ServerConfig []any  `json:"serverConfig"`
}

const autoBackupPath = "/autoBackup/autoBackupTask"

// GetAutoBackup returns the auto-backup configuration plus the live document as
// a loose map. The raw map is what the provider's update round-trips, so
// unmodelled fields (the file-server credentials and the retain* booleans) are
// preserved exactly as the controller holds them.
func (c *Client) GetAutoBackup(ctx context.Context) (*AutoBackup, map[string]any, error) {
	var raw map[string]any
	if err := c.Do(ctx, http.MethodGet, autoBackupPath, nil, &raw); err != nil {
		return nil, nil, fmt.Errorf("reading auto-backup config: %w", err)
	}

	out := &AutoBackup{}
	out.Enable, _ = raw["enable"].(bool)
	out.MaxFiles = intFromAny(raw["maxNumberOfFile"])
	out.Retention = intFromAny(raw["retention"])
	out.Type, _ = raw["type"].(string)
	if occ, ok := raw["occurrence"].(map[string]any); ok {
		out.Occurrence.TimingType = intFromAny(occ["timingType"])
		out.Occurrence.Hour = intFromAny(occ["hour"])
		out.Occurrence.Minute = intFromAny(occ["minute"])
		out.Occurrence.DayOfWeek = intFromAny(occ["dayOfWeek"])
		out.Occurrence.DayOfMonth = intFromAny(occ["dayOfMonth"])
		out.Occurrence.MonthOfYear = intFromAny(occ["monthOfYear"])
	}
	if fs, ok := raw["fileServerConfig"].(map[string]any); ok {
		out.FileServer.Enable, _ = fs["enable"].(bool)
		out.FileServer.Protocol, _ = fs["protocol"].(string)
		out.FileServer.ServerConfig, _ = fs["serverConfig"].([]any)
	}
	out.Retain.Setting, _ = raw["retainSetting"].(bool)
	out.Retain.User, _ = raw["retainUser"].(bool)
	out.Retain.AuthRecord, _ = raw["retainAuthRecord"].(bool)
	out.Retain.FirmwareLog, _ = raw["retainFirmwareLog"].(bool)

	return out, raw, nil
}

// autoBackupRuntimeKeys are fields the controller (or its UI) owns and that
// must never be written back. They come from two sources:
//
//   - Scheduler runtime state the GET response includes: nowStatus, dataSheets.
//   - UI-serializer helpers the web app builds client-side from the document
//     (see its `autoBackupModel` convert/serialize): availablePaths,
//     fileServerEnable, lastConfig, filePath, tftpScpFilePath, serverConfig,
//     plus the derived timing fields status, deviceMacs, startTime, hour,
//     minute, timingType, time.
//
// The UI deletes all of these before submitting. A read-modify-write built on
// the GET response therefore has to strip them too, or the round-trip sends the
// controller fields it never returns.
//
// NOTE: this is a TOP-LEVEL strip only. fileServerConfig.serverConfig is
// deliberately preserved — it is the real, credential-bearing destination list,
// not the client-side helper, and dropping it would destroy stored credentials.
var autoBackupRuntimeKeys = map[string]struct{}{
	// runtime / controller-owned
	"nowStatus":  {},
	"dataSheets": {},
	"status":     {},
	"deviceMacs": {},
	// UI-serializer helpers (not part of the wire document)
	"availablePaths":   {},
	"fileServerEnable": {},
	"lastConfig":       {},
	"filePath":         {},
	"tftpScpFilePath":  {},
	"serverConfig":     {},
	"startTime":        {},
	"hour":             {},

	// derived timing fields the UI flattens out of `occurrence`
	"timingType":  {},
	"time":        {},
	"dayOfWeek":   {},
	"dayOfMonth":  {},
	"monthOfYear": {},
	"minute":      {},
}

// UpdateAutoBackup replaces the auto-backup configuration.
//
// The caller passes the loose document returned by GetAutoBackup with the
// modelled fields already changed, so unmodelled fields survive the round-trip.
// Runtime and UI-helper keys are dropped before the write; nested
// fileServerConfig (including its credential-bearing serverConfig) is preserved.
func (c *Client) UpdateAutoBackup(ctx context.Context, raw map[string]any) error {
	body := StripAutoBackupRuntimeKeys(raw)
	if err := c.Do(ctx, http.MethodPut, autoBackupPath, body, nil); err != nil {
		return fmt.Errorf("updating auto-backup config: %w", err)
	}
	return nil
}

// StripAutoBackupRuntimeKeys returns a copy of doc without the runtime and
// UI-helper keys that must not be written. Nested objects (fileServerConfig)
// are copied verbatim. It is exported so the resource layer can assert an
// equivalent path and so tests can pin the exact strip set.
func StripAutoBackupRuntimeKeys(doc map[string]any) map[string]any {
	body := make(map[string]any, len(doc))
	for k, v := range doc {
		if _, drop := autoBackupRuntimeKeys[k]; drop {
			continue
		}
		body[k] = v
	}
	return body
}

// intFromAny coerces a JSON number (float64) or an int to int, returning 0 for
// anything else. The controller sends numbers as float64.
func intFromAny(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	default:
		return 0
	}
}
