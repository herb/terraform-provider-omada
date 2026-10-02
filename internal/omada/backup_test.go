// Copyright (c) wncservices
// SPDX-License-Identifier: MPL-2.0

package omada

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newBackupTestController mimics the auto-backup endpoint. It records the last
// PUT body so a test can assert what was (and was not) written.
func newBackupTestController(t *testing.T, doc map[string]any, lastPut *map[string]any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, 0, "", map[string]any{"omadacId": "abc123", "controllerVer": "6.2.14.11", "apiVer": "3", "type": 1})
	})
	mux.HandleFunc("/abc123/api/v2/login", func(w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, 0, "", map[string]any{"token": "tok-xyz"})
	})
	mux.HandleFunc("/abc123/api/v2/autoBackup/autoBackupTask", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Csrf-Token") != "tok-xyz" {
			writeEnvelope(w, -1400, "invalid csrf token", nil)
			return
		}
		switch r.Method {
		case http.MethodGet:
			writeEnvelope(w, 0, "", doc)
		case http.MethodPut:
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				writeEnvelope(w, -1001, "bad body", nil)
				return
			}
			if lastPut != nil {
				*lastPut = body
			}
			writeEnvelope(w, 0, "", nil)
		default:
			writeEnvelope(w, -1600, "unsupported", nil)
		}
	})
	return httptest.NewServer(mux)
}

func TestGetAutoBackupParsesDocument(t *testing.T) {
	doc := map[string]any{
		"enable":            true,
		"occurrence":        map[string]any{"timingType": 1, "hour": 3, "minute": 0, "dayOfWeek": 0, "dayOfMonth": 1, "monthOfYear": 1},
		"maxNumberOfFile":   14,
		"retention":         30,
		"type":              "soft",
		"retainSetting":     true,
		"retainUser":        true,
		"retainAuthRecord":  false,
		"retainFirmwareLog": true,
		"fileServerConfig":  map[string]any{"enable": false, "protocol": "FTP", "serverConfig": []any{}},
	}
	srv := newBackupTestController(t, doc, nil)
	defer srv.Close()

	c, err := NewClient(context.Background(), srv.URL, "admin", "secret", true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ab, raw, err := c.GetAutoBackup(context.Background())
	if err != nil {
		t.Fatalf("GetAutoBackup: %v", err)
	}
	if !ab.Enable || ab.Occurrence.TimingType != 1 || ab.Occurrence.Hour != 3 {
		t.Errorf("unexpected timing: %+v", ab.Occurrence)
	}
	if ab.MaxFiles != 14 || ab.Retention != 30 || ab.Type != "soft" {
		t.Errorf("unexpected counts/type: %+v", ab)
	}
	if !ab.Retain.Setting || !ab.Retain.User || ab.Retain.AuthRecord || !ab.Retain.FirmwareLog {
		t.Errorf("unexpected retain flags: %+v", ab.Retain)
	}
	if raw["type"] != "soft" {
		t.Errorf("raw document missing unmodelled passthrough: %v", raw)
	}
}

func TestUpdateAutoBackupDropsRuntimeFields(t *testing.T) {
	doc := map[string]any{"enable": true, "retention": 30}
	var lastPut map[string]any
	srv := newBackupTestController(t, doc, &lastPut)
	defer srv.Close()

	c, err := NewClient(context.Background(), srv.URL, "admin", "secret", true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	// The caller round-trips the raw document, which still carries runtime
	// fields; the client must strip them before the write.
	raw := map[string]any{
		"enable":     false,
		"nowStatus":  -1,
		"dataSheets": []any{},
		"retention":  30,
	}
	if err := c.UpdateAutoBackup(context.Background(), raw); err != nil {
		t.Fatalf("UpdateAutoBackup: %v", err)
	}
	if _, ok := lastPut["nowStatus"]; ok {
		t.Errorf("nowStatus leaked into the write: %v", lastPut)
	}
	if _, ok := lastPut["dataSheets"]; ok {
		t.Errorf("dataSheets leaked into the write: %v", lastPut)
	}
	if lastPut["enable"] != false || intFromAny(lastPut["retention"]) != 30 {
		t.Errorf("expected fields not written: %v", lastPut)
	}
}

func TestStripAutoBackupRuntimeKeys(t *testing.T) {
	// The UI serializer and the GET response both carry keys the controller
	// must never receive back. Pin the exact strip set, and prove the nested
	// credential list is preserved.
	doc := map[string]any{
		"enable":           true,
		"retention":        7,
		"occurrence":       map[string]any{"timingType": 2, "hour": 3, "minute": 0, "dayOfWeek": 0},
		"nowStatus":        -1,
		"dataSheets":       []any{},
		"status":           true,
		"deviceMacs":       []any{},
		"availablePaths":   []any{},
		"fileServerEnable": 0,
		"lastConfig":       "FTP",
		"filePath":         "/tmp",
		"tftpScpFilePath":  "/tmp",
		"serverConfig":     map[string]any{"user": "op"},
		"startTime":        "12:00",
		"timingType":       2,
		"dayOfWeek":        0,
		"fileServerConfig": map[string]any{
			"enable": false, "protocol": "FTP",
			"serverConfig": []any{map[string]any{"password": "SECRET-FTP"}},
		},
	}
	out := StripAutoBackupRuntimeKeys(doc)
	for _, key := range []string{
		"nowStatus", "dataSheets", "status", "deviceMacs",
		"availablePaths", "fileServerEnable", "lastConfig", "filePath",
		"tftpScpFilePath", "serverConfig", "startTime", "timingType", "dayOfWeek",
	} {
		if _, ok := out[key]; ok {
			t.Errorf("key %q should have been stripped", key)
		}
	}
	for _, key := range []string{"enable", "retention", "occurrence", "fileServerConfig"} {
		if _, ok := out[key]; !ok {
			t.Errorf("key %q should have been preserved", key)
		}
	}
	fs, _ := out["fileServerConfig"].(map[string]any)
	cfg, _ := fs["serverConfig"].([]any)
	if len(cfg) != 1 {
		t.Fatalf("nested fileServerConfig.serverConfig not preserved: %v", out["fileServerConfig"])
	}
	first, _ := cfg[0].(map[string]any)
	if first["password"] != "SECRET-FTP" {
		t.Errorf("nested credential altered: %v", first)
	}
}

func TestGetAutoBackupAuthErrorIsNotMissingResource(t *testing.T) {
	// A controller that refuses the token must surface an API error, not a
	// silent empty document.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/info", func(w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, 0, "", map[string]any{"omadacId": "abc123", "controllerVer": "6.2.14.11", "apiVer": "3", "type": 1})
	})
	mux.HandleFunc("/abc123/api/v2/login", func(w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, 0, "", map[string]any{"token": "tok-xyz"})
	})
	mux.HandleFunc("/abc123/api/v2/autoBackup/autoBackupTask", func(w http.ResponseWriter, _ *http.Request) {
		writeEnvelope(w, -44116, "unauthorized", nil)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c, err := NewClient(context.Background(), srv.URL, "admin", "secret", true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, _, err := c.GetAutoBackup(context.Background()); err == nil {
		t.Fatal("expected an error for an unauthorized read, got nil")
	}
}
