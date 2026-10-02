// Copyright (c) wncservices
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/int64validator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/boolplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/int64planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/tfsdk"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/wncservices/terraform-provider-omada/internal/omada"
)

var (
	_ resource.Resource                = &backupResource{}
	_ resource.ResourceWithConfigure   = &backupResource{}
	_ resource.ResourceWithImportState = &backupResource{}
)

// NewBackupResource exposes the controller's automatic backup schedule.
func NewBackupResource() resource.Resource { return &backupResource{} }

// backupResource manages the controller's auto-backup configuration. It is
// controller-scoped (DESIGN §2.7b) and a singleton, so it has no `site`.
//
// # Adopt, don't create
//
// The configuration always exists on the controller. "Create" here means
// "start managing it", so nothing checks whether a schedule is already present.
// `Delete` forgets it without disabling automatic backups or removing any
// backup file: deleting files is destructive and outside a settings resource.
//
// # Remote file-server credentials are deliberately not managed
//
// The document can point automatic backups at an FTP/TFTP/SCP server, whose
// fields carry credentials. Those belong in their own resource (DESIGN §2.6),
// so this resource never reads or writes them; they are preserved verbatim on
// update by round-tripping the live document.
type backupResource struct{ data *providerData }

type backupResourceModel struct {
	ID types.String `tfsdk:"id"`

	Enabled types.Bool `tfsdk:"enabled"`

	TimingType types.Int64 `tfsdk:"timing_type"`
	Hour       types.Int64 `tfsdk:"hour"`
	Minute     types.Int64 `tfsdk:"minute"`

	// Schedule selectors, each applicable only to one timing type. Preserved
	// when not applicable so switching frequency does not wipe the others.
	DayOfWeek   types.Int64 `tfsdk:"day_of_week"`   // weekly: 0 Sunday .. 6 Saturday
	DayOfMonth  types.Int64 `tfsdk:"day_of_month"`  // monthly: 1..31
	MonthOfYear types.Int64 `tfsdk:"month_of_year"` // yearly: 1 January .. 12 December

	Retention types.Int64 `tfsdk:"retention"`
	MaxFiles  types.Int64 `tfsdk:"max_files"`

	// Content-selection flags. Optional+Computed so that leaving one unset
	// reports the controller's value rather than overwriting it with false
	// ("null is not false", DESIGN §2.6).
	RetainSetting     types.Bool `tfsdk:"retain_setting"`
	RetainUser        types.Bool `tfsdk:"retain_user"`
	RetainAuthRecord  types.Bool `tfsdk:"retain_auth_record"`
	RetainFirmwareLog types.Bool `tfsdk:"retain_firmware_log"`

	// Read-only views of the destination. The credentials behind it are not
	// modelled; these are reported for visibility only.
	RemoteEnabled  types.Bool   `tfsdk:"remote_enabled"`
	RemoteProtocol types.String `tfsdk:"remote_protocol"`
}

func (r *backupResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_backup"
}

func (r *backupResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages the controller's automatic backup schedule (Global View → Settings → Backup & Restore).\n\n" +
			"~> **Controller-scoped.** Automatic backup belongs to the controller, not a site, so " +
			"there is no `site` attribute. Times are in the controller's own time zone " +
			"(`omada_controller_settings.time_zone`), not a site's.\n\n" +
			"~> **This resource manages the backup schedule, not backup files.** `terraform destroy` " +
			"stops managing the schedule but does **not** disable automatic backups or delete any " +
			"backup the controller has already produced.\n\n" +
			"~> **Retention is retained-data history, not file lifetime.** `retention` is the number of " +
			"days of data kept (`-1` settings only, `0` all history); `max_files` separately caps how " +
			"many backup files exist. The semantics come from the controller UI's own choices " +
			"(read-only observation), not from a live write.\n\n" +
			"Remote file-server destinations (`fileServerConfig`) carry FTP/TFTP/SCP credentials and " +
			"are deliberately not managed here; `remote_enabled` / `remote_protocol` report the " +
			"controller's current setting without reading or writing those credentials.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "The controller's omadac id. A singleton, so this is stable.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"enabled": schema.BoolAttribute{
				Optional:            true,
				Computed:            true,
				MarkdownDescription: "Whether automatic backups run on the schedule below.",
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"timing_type": schema.Int64Attribute{
				Optional: true, Computed: true,
				Validators:          []validator.Int64{int64validator.Between(1, 4)},
				MarkdownDescription: "How often the backup runs: `1` daily, `2` weekly, `3` monthly, `4` yearly.",
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"hour": schema.Int64Attribute{
				Optional: true, Computed: true,
				Validators:          []validator.Int64{int64validator.Between(0, 23)},
				MarkdownDescription: "Hour of the run, 0-23, in the controller's time zone.",
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"minute": schema.Int64Attribute{
				Optional: true, Computed: true,
				Validators:          []validator.Int64{int64validator.Between(0, 59)},
				MarkdownDescription: "Minute of the run, 0-59.",
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"day_of_week": schema.Int64Attribute{
				Optional: true, Computed: true,
				Validators: []validator.Int64{int64validator.Between(0, 6)},
				MarkdownDescription: "Weekly schedules: `0` Sunday through `6` Saturday. Required when " +
					"`timing_type = 2`; ignored (but preserved) otherwise.",
				PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"day_of_month": schema.Int64Attribute{
				Optional: true, Computed: true,
				Validators: []validator.Int64{int64validator.Between(1, 31)},
				MarkdownDescription: "Monthly and yearly schedules: day of the month, 1-31. Required when " +
					"`timing_type` is `3` or `4`; ignored (but preserved) otherwise.",
				PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"month_of_year": schema.Int64Attribute{
				Optional: true, Computed: true,
				Validators: []validator.Int64{int64validator.Between(1, 12)},
				MarkdownDescription: "Yearly schedules: `1` January through `12` December. Required when " +
					"`timing_type = 4`; ignored (but preserved) otherwise.",
				PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"retention": schema.Int64Attribute{
				Optional: true, Computed: true,
				Validators: []validator.Int64{int64validator.OneOf(-1, 0, 7, 30, 60, 90, 180, 365)},
				MarkdownDescription: "How much backup data the controller retains. `-1` settings only; `0` all " +
					"history (disabled on hardware controllers); a positive value is the number of **days** " +
					"of data to keep (`7`, `30`, `60`, `90`, `180`, `365`).\n\n" +
					"~> This is retained-**data history**, not backup-file lifetime — `max_files` governs " +
					"how many files are kept. Semantics were confirmed from the controller UI's own " +
					"retention choices (read-only), not by a live write.",
				PlanModifiers: []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"max_files": schema.Int64Attribute{
				Optional: true, Computed: true,
				Validators:          []validator.Int64{int64validator.Between(1, 50)},
				MarkdownDescription: "Maximum number of backup files the controller keeps.",
				PlanModifiers:       []planmodifier.Int64{int64planmodifier.UseStateForUnknown()},
			},
			"retain_setting": schema.BoolAttribute{
				Optional: true, Computed: true,
				MarkdownDescription: "Include controller/site settings in each backup.",
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"retain_user": schema.BoolAttribute{
				Optional: true, Computed: true,
				MarkdownDescription: "Include user data in each backup.",
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"retain_auth_record": schema.BoolAttribute{
				Optional: true, Computed: true,
				MarkdownDescription: "Include authentication records in each backup.",
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"retain_firmware_log": schema.BoolAttribute{
				Optional: true, Computed: true,
				MarkdownDescription: "Include firmware logs in each backup.",
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"remote_enabled": schema.BoolAttribute{
				Computed:            true,
				MarkdownDescription: "Whether a remote file-server destination is configured. Its credentials are not managed here.",
				PlanModifiers:       []planmodifier.Bool{boolplanmodifier.UseStateForUnknown()},
			},
			"remote_protocol": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Protocol of the configured remote destination (`FTP`, …), if any. Not settable here.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *backupResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	data, ok := req.ProviderData.(*providerData)
	if !ok {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("got %T", req.ProviderData))
		return
	}
	r.data = data
}

// refresh writes the controller's document into the model. Every attribute is
// Optional+Computed and filled from the read, so `terraform show` describes the
// controller rather than only the managed fields.
func (r *backupResource) refresh(ctx context.Context, ab *omada.AutoBackup, m *backupResourceModel) diag.Diagnostics {
	var diags diag.Diagnostics
	m.ID = types.StringValue(r.data.client.OmadacID())
	m.Enabled = types.BoolValue(ab.Enable)
	m.TimingType = types.Int64Value(int64(ab.Occurrence.TimingType))
	m.Hour = types.Int64Value(int64(ab.Occurrence.Hour))
	m.Minute = types.Int64Value(int64(ab.Occurrence.Minute))
	m.DayOfWeek = types.Int64Value(int64(ab.Occurrence.DayOfWeek))
	m.DayOfMonth = types.Int64Value(int64(ab.Occurrence.DayOfMonth))
	m.MonthOfYear = types.Int64Value(int64(ab.Occurrence.MonthOfYear))
	m.Retention = types.Int64Value(int64(ab.Retention))
	m.MaxFiles = types.Int64Value(int64(ab.MaxFiles))
	m.RetainSetting = types.BoolValue(ab.Retain.Setting)
	m.RetainUser = types.BoolValue(ab.Retain.User)
	m.RetainAuthRecord = types.BoolValue(ab.Retain.AuthRecord)
	m.RetainFirmwareLog = types.BoolValue(ab.Retain.FirmwareLog)
	m.RemoteEnabled = types.BoolValue(ab.FileServer.Enable)
	m.RemoteProtocol = types.StringValue(ab.FileServer.Protocol)
	_ = ctx
	return diags
}

// desired returns the values the REQUEST (config) explicitly sets, keyed by
// Terraform attribute. Reading from the request config rather than the plan is
// what distinguishes "the practitioner wrote this value" from "it was inherited
// from Optional+Computed state", so an apply that changes nothing is detected
// as a no-op instead of always reporting drift.
//
// A value is "set" only when the config attribute is known and not null.
func desired(ctx context.Context, cfg tfsdk.Config) (map[string]any, diag.Diagnostics) {
	var diags diag.Diagnostics
	out := map[string]any{}
	getBool := func(attr string) {
		var v types.Bool
		diags.Append(cfg.GetAttribute(ctx, path.Root(attr), &v)...)
		if !v.IsNull() && !v.IsUnknown() {
			out[attr] = v.ValueBool()
		}
	}
	getInt := func(attr string) {
		var v types.Int64
		diags.Append(cfg.GetAttribute(ctx, path.Root(attr), &v)...)
		if !v.IsNull() && !v.IsUnknown() {
			out[attr] = v.ValueInt64()
		}
	}
	getBool("enabled")
	for _, attr := range []string{"timing_type", "hour", "minute", "day_of_week", "day_of_month", "month_of_year", "retention", "max_files"} {
		getInt(attr)
	}
	for _, attr := range []string{"retain_setting", "retain_user", "retain_auth_record", "retain_firmware_log"} {
		getBool(attr)
	}
	return out, diags
}

// semanticDiff returns whether the managed desired values differ from the live
// document. It is the single source of truth for "is a PUT needed": a plain
// adoption, or an apply whose desired values already match, yields no PUT.
func semanticDiff(live *omada.AutoBackup, want map[string]any) bool {
	if v, ok := want["enabled"].(bool); ok && v != live.Enable {
		return true
	}
	if v, ok := want["timing_type"].(int64); ok && int(v) != live.Occurrence.TimingType {
		return true
	}
	if v, ok := want["hour"].(int64); ok && int(v) != live.Occurrence.Hour {
		return true
	}
	if v, ok := want["minute"].(int64); ok && int(v) != live.Occurrence.Minute {
		return true
	}
	if v, ok := want["day_of_week"].(int64); ok && int(v) != live.Occurrence.DayOfWeek {
		return true
	}
	if v, ok := want["day_of_month"].(int64); ok && int(v) != live.Occurrence.DayOfMonth {
		return true
	}
	if v, ok := want["month_of_year"].(int64); ok && int(v) != live.Occurrence.MonthOfYear {
		return true
	}
	if v, ok := want["retention"].(int64); ok && int(v) != live.Retention {
		return true
	}
	if v, ok := want["max_files"].(int64); ok && int(v) != live.MaxFiles {
		return true
	}
	if v, ok := want["retain_setting"].(bool); ok && v != live.Retain.Setting {
		return true
	}
	if v, ok := want["retain_user"].(bool); ok && v != live.Retain.User {
		return true
	}
	if v, ok := want["retain_auth_record"].(bool); ok && v != live.Retain.AuthRecord {
		return true
	}
	if v, ok := want["retain_firmware_log"].(bool); ok && v != live.Retain.FirmwareLog {
		return true
	}
	return false
}

// requiredSelectors returns the selector attributes a schedule of the given
// timing type must have, per the controller UI serializer:
//
//	1 daily   -> none
//	2 weekly  -> day_of_week
//	3 monthly -> day_of_month
//	4 yearly  -> month_of_year AND day_of_month
var requiredSelectors = map[int64][]string{
	1: nil,
	2: {"day_of_week"},
	3: {"day_of_month"},
	4: {"month_of_year", "day_of_month"},
}

// selectorTargets maps each selector attribute to the timing type it applies
// to, per the controller UI's serializer.
var selectorTargets = map[string]int64{
	"day_of_week":   2, // weekly
	"day_of_month":  3, // monthly and yearly
	"month_of_year": 4, // yearly
}

// validateSelectors checks selectors against the effective timing type, but
// ONLY when the schedule is actually changing.
//
//   - Unchanged schedule (timing type equals the live value): every selector is
//     accepted as inherited. A no-op adoption, or a change to some unrelated
//     field such as `retention` on an existing weekly schedule, must not force
//     the practitioner to re-specify a selector the controller already holds
//     and that is already valid.
//   - Changed schedule: the selectors the new timing type requires must be
//     present, either explicitly in the request or already valid in the live
//     occurrence (an inherited value that satisfies the new type). If a
//     required selector is neither supplied nor inherited, the change is
//     refused before any write.
//   - A selector explicitly set for a timing type it does not apply to, while
//     the schedule IS changing, is refused; an inherited (unset) one is fine.
func validateSelectors(want map[string]any, live *omada.AutoBackup) diag.Diagnostics {
	var diags diag.Diagnostics
	liveTiming := int64(live.Occurrence.TimingType)

	effectiveTiming := liveTiming
	if v, ok := want["timing_type"].(int64); ok {
		effectiveTiming = v
	}
	scheduleChanging := effectiveTiming != liveTiming

	if !scheduleChanging {
		// Unchanged schedule: nothing to validate, selectors are inherited.
		return diags
	}

	required := requiredSelectors[effectiveTiming]
	haveLive := func(attr string) bool {
		switch attr {
		case "day_of_week":
			return live.Occurrence.DayOfWeek != 0 || liveTiming == 2
		case "day_of_month":
			return live.Occurrence.DayOfMonth != 0
		case "month_of_year":
			return live.Occurrence.MonthOfYear != 0
		default:
			return false
		}
	}
	for _, attr := range required {
		if _, explicit := want[attr]; explicit {
			continue
		}
		if haveLive(attr) {
			continue // an inherited value that satisfies the new type
		}
		diags.AddAttributeError(path.Root(attr),
			"Schedule selector required",
			fmt.Sprintf("`%s` must be set when changing the schedule to %s.", attr, timingName(effectiveTiming)))
	}

	// Reject explicitly setting a selector for an inapplicable timing type.
	requiredSet := map[string]bool{}
	for _, attr := range required {
		requiredSet[attr] = true
	}
	for attr := range selectorTargets {
		if _, explicit := want[attr]; explicit && !requiredSet[attr] {
			diags.AddAttributeError(path.Root(attr),
				"Schedule selector not applicable",
				fmt.Sprintf("`%s` does not apply to %s schedules; the effective `timing_type` is %s. "+
					"Setting it here would be silently ignored by the controller.", attr, timingName(effectiveTiming), timingName(effectiveTiming)))
		}
	}
	return diags
}

func timingName(timing int64) string {
	switch timing {
	case 1:
		return "daily"
	case 2:
		return "weekly"
	case 3:
		return "monthly"
	case 4:
		return "yearly"
	default:
		return fmt.Sprintf("timing_type=%d", timing)
	}
}

// apply reads the live document, computes whether the managed desired values
// differ, and writes only when they do. Create and Update are identical: the
// document always exists.
//
// It deliberately does NOT treat every field as managed: an attribute the
// practitioner never wrote is neither compared nor sent.
func (r *backupResource) apply(ctx context.Context, plan *backupResourceModel, cfg tfsdk.Config) diag.Diagnostics {
	var diags diag.Diagnostics

	live, raw, err := r.data.client.GetAutoBackup(ctx)
	if err != nil {
		diags.AddError("Unable to read auto-backup configuration", err.Error())
		return diags
	}

	want, d := desired(ctx, cfg)
	diags.Append(d...)
	if diags.HasError() {
		return diags
	}

	// A request that disables automatic backup must not silently drop other
	// explicit changes. The controller's disable payload is exactly
	// {"enable": false}, which cannot carry them, so refuse rather than produce
	// a state that disagrees with the request.
	//
	// This is checked before selector validation: while the schedule is off the
	// selectors are irrelevant, so disabling must not demand one.
	if v, ok := want["enabled"].(bool); ok && !v {
		if conflicting := disablingConflicts(want, live); len(conflicting) > 0 {
			diags.AddError(
				"Cannot disable auto-backup and change it in the same apply",
				fmt.Sprintf("Disabling auto-backup sends exactly {\"enable\": false} and cannot also apply: %v. "+
					"Apply the other changes first, then disable in a separate apply, or remove those settings.",
					conflicting),
			)
			return diags
		}
		if live.Enable {
			if err := r.data.client.UpdateAutoBackup(ctx, map[string]any{"enable": false}); err != nil {
				diags.AddError("Unable to disable auto-backup", err.Error())
				return diags
			}
			live, _, err = r.data.client.GetAutoBackup(ctx)
			if err != nil {
				diags.AddError("Unable to read auto-backup configuration after disabling", err.Error())
				return diags
			}
		}
		diags.Append(r.refresh(ctx, live, plan)...)
		return diags
	}

	// Selectors are validated only when the schedule is actually changing; an
	// unchanged schedule keeps its inherited selectors.
	if d := validateSelectors(want, live); d.HasError() {
		diags.Append(d...)
		return diags
	}

	if !semanticDiff(live, want) {
		// Nothing to change: report the live document, no PUT.
		diags.Append(r.refresh(ctx, live, plan)...)
		return diags
	}

	// Overlay only the managed fields onto the live document, then write.
	if v, ok := want["enabled"].(bool); ok {
		raw["enable"] = v
	}
	occ, _ := raw["occurrence"].(map[string]any)
	if occ == nil {
		occ = map[string]any{}
	}
	if v, ok := want["timing_type"].(int64); ok {
		occ["timingType"] = int(v)
	}
	if v, ok := want["hour"].(int64); ok {
		occ["hour"] = int(v)
	}
	if v, ok := want["minute"].(int64); ok {
		occ["minute"] = int(v)
	}
	if v, ok := want["day_of_week"].(int64); ok {
		occ["dayOfWeek"] = int(v)
	}
	if v, ok := want["day_of_month"].(int64); ok {
		occ["dayOfMonth"] = int(v)
	}
	if v, ok := want["month_of_year"].(int64); ok {
		occ["monthOfYear"] = int(v)
	}
	raw["occurrence"] = occ
	if v, ok := want["retention"].(int64); ok {
		raw["retention"] = int(v)
	}
	if v, ok := want["max_files"].(int64); ok {
		raw["maxNumberOfFile"] = int(v)
	}
	if v, ok := want["retain_setting"].(bool); ok {
		raw["retainSetting"] = v
	}
	if v, ok := want["retain_user"].(bool); ok {
		raw["retainUser"] = v
	}
	if v, ok := want["retain_auth_record"].(bool); ok {
		raw["retainAuthRecord"] = v
	}
	if v, ok := want["retain_firmware_log"].(bool); ok {
		raw["retainFirmwareLog"] = v
	}

	if err := r.data.client.UpdateAutoBackup(ctx, raw); err != nil {
		diags.AddError("Unable to update auto-backup configuration", err.Error())
		return diags
	}
	live, _, err = r.data.client.GetAutoBackup(ctx)
	if err != nil {
		diags.AddError("Unable to read auto-backup configuration after update", err.Error())
		return diags
	}
	diags.Append(r.refresh(ctx, live, plan)...)
	return diags
}

// disablingConflicts lists the managed attributes the request explicitly sets
// (to a value differing from live) that a disable-only payload cannot carry.
// The disable payload is exactly {"enable": false}; anything else the request
// asks for would be silently dropped, so it is a conflict.
//
// This covers the four retain booleans as well as the schedule/retention
// integers: `enabled=false` together with `retain_user=true` (against a live
// `false`) is a real requested change that a disable cannot carry, and must be
// refused BEFORE any PUT — even if the schedule is already disabled.
func disablingConflicts(want map[string]any, live *omada.AutoBackup) []string {
	var out []string
	checkInt := func(attr string, liveVal int) {
		if v, ok := want[attr].(int64); ok && int(v) != liveVal {
			out = append(out, attr)
		}
	}
	checkBool := func(attr string, liveVal bool) {
		if v, ok := want[attr].(bool); ok && v != liveVal {
			out = append(out, attr)
		}
	}
	checkInt("timing_type", live.Occurrence.TimingType)
	checkInt("hour", live.Occurrence.Hour)
	checkInt("minute", live.Occurrence.Minute)
	checkInt("day_of_week", live.Occurrence.DayOfWeek)
	checkInt("day_of_month", live.Occurrence.DayOfMonth)
	checkInt("month_of_year", live.Occurrence.MonthOfYear)
	checkInt("retention", live.Retention)
	checkInt("max_files", live.MaxFiles)
	checkBool("retain_setting", live.Retain.Setting)
	checkBool("retain_user", live.Retain.User)
	checkBool("retain_auth_record", live.Retain.AuthRecord)
	checkBool("retain_firmware_log", live.Retain.FirmwareLog)
	return out
}

// mustRead re-reads the document and records an error if it cannot.
func (r *backupResource) mustRead(ctx context.Context, diags *diag.Diagnostics) *omada.AutoBackup {
	ab, _, err := r.data.client.GetAutoBackup(ctx)
	if err != nil {
		diags.AddError("Unable to read auto-backup configuration", err.Error())
		return &omada.AutoBackup{}
	}
	return ab
}

func (r *backupResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan backupResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.apply(ctx, &plan, req.Config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *backupResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state backupResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	// A failed read is an error, never a missing resource: the document cannot
	// be deleted, so RemoveResource would turn an unreachable controller into a
	// plan that recreates the schedule.
	ab, _, err := r.data.client.GetAutoBackup(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Unable to read auto-backup configuration", err.Error())
		return
	}
	resp.Diagnostics.Append(r.refresh(ctx, ab, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *backupResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan backupResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(r.apply(ctx, &plan, req.Config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

// Delete forgets the schedule without disabling automatic backups or deleting
// any backup file.
func (r *backupResource) Delete(_ context.Context, _ resource.DeleteRequest, resp *resource.DeleteResponse) {
	resp.Diagnostics.AddWarning(
		"Automatic backup left as configured",
		"The auto-backup schedule cannot be deleted, so Terraform has only stopped managing it. "+
			"It is unchanged: automatic backups keep running and existing backup files are kept. "+
			"Disabling the schedule and removing backup files are deliberate, separate actions.",
	)
}

// ImportState takes any id; the document is a singleton, so nothing is
// addressed. `terraform import omada_backup.this controller` reads fine.
//
// A read failure is reported as an error, never as "not found": the document
// always exists, so an unreachable controller must not look like an empty state
// that a later plan would recreate.
func (r *backupResource) ImportState(ctx context.Context, _ resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	var m backupResourceModel
	resp.Diagnostics.Append(r.refresh(ctx, r.mustRead(ctx, &resp.Diagnostics), &m)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &m)...)
}
