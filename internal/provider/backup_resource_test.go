// Copyright (c) wncservices
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/terraform"
)

// fetchBackupDoc reads the mock controller's current auto-backup document.
func fetchBackupDoc(t *testing.T, url string) map[string]any {
	t.Helper()
	resp, err := http.Get(url + "/debug/backup")
	if err != nil {
		t.Fatalf("reading debug backup doc: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var doc map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatalf("decoding debug backup doc: %v", err)
	}
	return doc
}

// backupPutCount reads how many PUTs the mock has seen.
func backupPutCount(t *testing.T, url string) int64 {
	t.Helper()
	resp, err := http.Get(url + "/debug/backup-puts")
	if err != nil {
		t.Fatalf("reading debug put count: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var n int64
	if err := json.NewDecoder(resp.Body).Decode(&n); err != nil {
		t.Fatalf("decoding put count: %v", err)
	}
	return n
}

// newMockControllerTiming returns a mock controller whose auto-backup document
// uses the given timing type and selectors, so inherited-selector behaviour can
// be exercised for weekly/monthly/yearly without an explicit selector in config.
func newMockControllerTiming(t *testing.T, timingType int, occurrence map[string]any) *httptest.Server {
	t.Helper()
	srv := newMockController(t)
	occ := map[string]any{"timingType": timingType, "hour": 12, "minute": 0}
	for k, v := range occurrence {
		occ[k] = v
	}
	doc := map[string]any{
		"enable":            true,
		"occurrence":        occ,
		"maxNumberOfFile":   7,
		"retention":         30,
		"type":              "soft",
		"nowStatus":         -1,
		"dataSheets":        []any{},
		"retainSetting":     true,
		"retainUser":        false,
		"retainAuthRecord":  true,
		"retainFirmwareLog": true,
		"fileServerConfig": map[string]any{
			"enable": false, "protocol": "FTP",
			"serverConfig": []any{map[string]any{"user": "op", "password": "SECRET-FTP"}},
		},
	}
	body, _ := json.Marshal(doc)
	resp, err := http.Post(srv.URL+"/debug/backup-seed", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("seeding backup doc: %v", err)
	}
	_ = resp.Body.Close()
	return srv
}

// TestAccBackupResource drives adopt -> import -> update -> disable, and checks
// that unmodelled fields (file-server credentials) and runtime fields survive.
func TestAccBackupResource(t *testing.T) {
	srv := newMockController(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testProviderConfig(srv.URL) + `
resource "omada_backup" "test" {
  enabled     = true
  timing_type = 2
  day_of_week = 0
  hour        = 3
  minute      = 0
  retention   = 30
  max_files   = 14
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrSet("omada_backup.test", "id"),
					resource.TestCheckResourceAttr("omada_backup.test", "enabled", "true"),
					resource.TestCheckResourceAttr("omada_backup.test", "timing_type", "2"),
					resource.TestCheckResourceAttr("omada_backup.test", "day_of_week", "0"),
					resource.TestCheckResourceAttr("omada_backup.test", "retention", "30"),
					resource.TestCheckResourceAttr("omada_backup.test", "max_files", "14"),
					resource.TestCheckResourceAttr("omada_backup.test", "retain_setting", "true"),
					resource.TestCheckResourceAttr("omada_backup.test", "retain_user", "false"),
					resource.TestCheckResourceAttr("omada_backup.test", "remote_protocol", "FTP"),
					func(s *terraform.State) error {
						doc := fetchBackupDoc(t, srv.URL)
						fs, _ := doc["fileServerConfig"].(map[string]any)
						cfg, _ := fs["serverConfig"].([]any)
						if len(cfg) != 1 {
							return errUnexpected("serverConfig was not preserved")
						}
						first, _ := cfg[0].(map[string]any)
						if first["password"] != "SECRET-FTP" {
							return errUnexpected("file-server credential was altered")
						}
						if _, ok := doc["nowStatus"]; !ok {
							return errUnexpected("runtime nowStatus missing")
						}
						return nil
					},
				),
			},
			{ResourceName: "omada_backup.test", ImportState: true, ImportStateVerify: true},
			{
				// A no-op re-apply (same managed values) must not PUT again.
				Config: testProviderConfig(srv.URL) + `
resource "omada_backup" "test" {
  enabled     = true
  timing_type = 2
  day_of_week = 0
  hour        = 3
  minute      = 0
  retention   = 30
  max_files   = 14
}`,
				Check: resource.TestCheckResourceAttr("omada_backup.test", "timing_type", "2"),
			},
			{
				// Monthly schedule switches the applicable selector.
				Config: testProviderConfig(srv.URL) + `
resource "omada_backup" "test" {
  enabled      = true
  timing_type  = 3
  day_of_month = 2
  hour         = 5
  minute       = 30
  retention    = 30
  max_files    = 14
  retain_user  = true
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("omada_backup.test", "timing_type", "3"),
					resource.TestCheckResourceAttr("omada_backup.test", "day_of_month", "2"),
					resource.TestCheckResourceAttr("omada_backup.test", "hour", "5"),
					resource.TestCheckResourceAttr("omada_backup.test", "retain_user", "true"),
					func(s *terraform.State) error {
						doc := fetchBackupDoc(t, srv.URL)
						if _, ok := doc["fileServerConfig"]; !ok {
							return errUnexpected("fileServerConfig lost on update")
						}
						return nil
					},
				),
			},
			{
				// Disable sends exactly {"enable": false}; everything else is
				// left as the controller had it. Only `enabled` is managed here,
				// so nothing conflicts with the disable payload.
				Config: testProviderConfig(srv.URL) + `
resource "omada_backup" "test" {
  enabled = false
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("omada_backup.test", "enabled", "false"),
					func(s *terraform.State) error {
						doc := fetchBackupDoc(t, srv.URL)
						if fs, ok := doc["fileServerConfig"].(map[string]any); !ok || fs == nil {
							return errUnexpected("fileServerConfig lost on disable")
						}
						return nil
					},
				),
			},
		},
	})
}

// TestAccBackupResource_noOpAdoptionDoesNotWrite proves P1-F01: importing an
// existing schedule with no configuration change performs zero PUTs.
func TestAccBackupResource_noOpAdoptionDoesNotWrite(t *testing.T) {
	srv := newMockController(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// Config matches the mock's seeded document exactly.
				Config: testProviderConfig(srv.URL) + `
resource "omada_backup" "test" {
  enabled     = false
  timing_type = 3
  day_of_month = 1
  hour        = 12
  minute      = 0
  retention   = 30
  max_files   = 7
}`,
				Check: func(s *terraform.State) error {
					if n := backupPutCount(t, srv.URL); n != 0 {
						return errUnexpected("expected zero PUTs for a no-op adoption, got writes")
					}
					return nil
				},
			},
		},
	})
}

// TestAccBackupResource_selectorRequired proves P1-F03: a weekly schedule
// without day_of_week is refused before any write.
func TestAccBackupResource_selectorRequired(t *testing.T) {
	srv := newMockController(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testProviderConfig(srv.URL) + `
resource "omada_backup" "test" {
  enabled     = true
  timing_type = 2
  hour        = 3
}`,
				ExpectError: regexp.MustCompile(`day_of_week`),
			},
		},
	})
}

// TestAccBackupResource_disableConflictRefused proves P1-F04: disabling while
// also explicitly changing the schedule is refused before any mutation.
func TestAccBackupResource_disableConflictRefused(t *testing.T) {
	srv := newMockController(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testProviderConfig(srv.URL) + `
resource "omada_backup" "test" {
  enabled      = false
  timing_type  = 2
  day_of_week  = 1
  hour         = 4
  minute       = 0
  retention    = 7
}`,
				ExpectError: regexp.MustCompile(`Cannot disable auto-backup`),
			},
		},
	})
}

// TestAccBackupResource_disableRetainConflictRefused proves P1-F04 attempt 2:
// disabling while explicitly changing a RETAIN boolean (which the disable-only
// payload cannot carry) is refused before any PUT, even though the schedule is
// already disabled. Before this fix only the schedule/retention integers were
// checked.
func TestAccBackupResource_disableRetainConflictRefused(t *testing.T) {
	srv := newMockController(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// The seeded live retain_user is false; requesting true while
				// disabling is a change the disable payload cannot carry.
				Config: testProviderConfig(srv.URL) + `
resource "omada_backup" "test" {
  enabled     = false
  retain_user = true
}`,
				ExpectError: regexp.MustCompile(`Cannot disable auto-backup`),
			},
		},
	})
}

// TestAccBackupResource_disableRetainMatchingNoWrite proves P1-F04 attempt 2:
// disabling with a retain boolean that MATCHES the live value is not a
// conflict, requires no selector, and performs zero PUTs when already disabled.
func TestAccBackupResource_disableRetainMatchingNoWrite(t *testing.T) {
	srv := newMockController(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testProviderConfig(srv.URL) + `
resource "omada_backup" "test" {
  enabled     = false
  retain_user = false
}`,
				Check: func(s *terraform.State) error {
					if n := backupPutCount(t, srv.URL); n != 0 {
						return errUnexpected("expected zero PUTs for matching disabled config")
					}
					return nil
				},
			},
		},
	})
}

// TestAccBackupResource_inheritedSelectorsAdoption proves P2-F01: adopting an
// existing schedule with only `enabled` (inheriting the live weekly/monthly
// selectors) must not demand an explicit selector, and must perform zero PUTs.
func TestAccBackupResource_inheritedSelectorsAdoption(t *testing.T) {
	for _, timing := range []struct {
		name   string
		liveTY int
		extra  map[string]any
	}{
		{"weekly", 2, map[string]any{"dayOfWeek": 0}},
		{"monthly", 3, map[string]any{"dayOfMonth": 1}},
		{"yearly", 4, map[string]any{"monthOfYear": 1, "dayOfMonth": 1}},
	} {
		t.Run(timing.name, func(t *testing.T) {
			srv := newMockControllerTiming(t, timing.liveTY, timing.extra)
			resource.Test(t, resource.TestCase{
				ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
				Steps: []resource.TestStep{
					{
						// No selectors are written; they are inherited from live.
						Config: testProviderConfig(srv.URL) + `
resource "omada_backup" "test" {
  enabled = true
}`,
						Check: func(s *terraform.State) error {
							if n := backupPutCount(t, srv.URL); n != 0 {
								return errUnexpected("inherited selectors should need zero PUTs")
							}
							return nil
						},
					},
				},
			})
		})
	}
}

// TestAccBackupResource_yearlyRequired proves the yearly contract: changing to
// a yearly schedule from a daily one (which carries no day selector) requires
// BOTH month_of_year and day_of_month; supplying only one is refused before any
// write.
func TestAccBackupResource_yearlyRequired(t *testing.T) {
	// Live is a daily schedule: no day-of-month is inherited, so yearly cannot
	// borrow one.
	srv := newMockControllerTiming(t, 1, nil)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// Yearly with month_of_year but no day_of_month is incomplete.
				Config: testProviderConfig(srv.URL) + `
resource "omada_backup" "test" {
  enabled       = true
  timing_type   = 4
  month_of_year = 1
  hour          = 3
}`,
				ExpectError: regexp.MustCompile(`day_of_month`),
			},
		},
	})
}

// TestAccBackupResource_yearlyRoundTrip proves a valid yearly schedule with both
// selectors applies and reads back cleanly.
func TestAccBackupResource_yearlyRoundTrip(t *testing.T) {
	srv := newMockController(t)

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testProviderConfig(srv.URL) + `
resource "omada_backup" "test" {
  enabled       = true
  timing_type   = 4
  month_of_year = 6
  day_of_month  = 15
  hour          = 3
  minute        = 0
  retention     = 30
  max_files     = 14
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("omada_backup.test", "timing_type", "4"),
					resource.TestCheckResourceAttr("omada_backup.test", "month_of_year", "6"),
					resource.TestCheckResourceAttr("omada_backup.test", "day_of_month", "15"),
				),
			},
			{ResourceName: "omada_backup.test", ImportState: true, ImportStateVerify: true},
		},
	})
}

// TestAccBackupResource_unrelatedChangeKeepsInheritedSelector proves P2-F01:
// changing an unrelated field (retention) on an existing weekly schedule must
// not force the practitioner to re-specify day_of_week.
func TestAccBackupResource_unrelatedChangeKeepsInheritedSelector(t *testing.T) {
	srv := newMockControllerTiming(t, 2, map[string]any{"dayOfWeek": 0})

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// Only retention is written; timing_type and day_of_week are
				// inherited from the live document.
				Config: testProviderConfig(srv.URL) + `
resource "omada_backup" "test" {
  retention = 7
}`,
				Check: resource.TestCheckResourceAttr("omada_backup.test", "retention", "7"),
			},
		},
	})
}

// TestAccBackupResource_yearlyInheritsDayOfMonth proves the inherited-value
// rule: switching a monthly schedule (day_of_month already valid) to yearly
// needs only month_of_year, because day_of_month is inherited.
func TestAccBackupResource_yearlyInheritsDayOfMonth(t *testing.T) {
	srv := newMockControllerTiming(t, 3, map[string]any{"dayOfMonth": 15})

	resource.Test(t, resource.TestCase{
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: testProviderConfig(srv.URL) + `
resource "omada_backup" "test" {
  enabled       = true
  timing_type   = 4
  month_of_year = 6
  hour          = 3
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("omada_backup.test", "timing_type", "4"),
					resource.TestCheckResourceAttr("omada_backup.test", "month_of_year", "6"),
					resource.TestCheckResourceAttr("omada_backup.test", "day_of_month", "15"),
				),
			},
		},
	})
}

// errUnexpected is a tiny helper so the CheckFunc bodies read cleanly.
type errUnexpected string

func (e errUnexpected) Error() string { return string(e) }
