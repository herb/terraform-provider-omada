// Copyright (c) wncservices
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"fmt"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/wncservices/terraform-provider-omada/internal/omada"
)

var (
	_ resource.Resource                   = &portProfileResource{}
	_ resource.ResourceWithConfigure      = &portProfileResource{}
	_ resource.ResourceWithImportState    = &portProfileResource{}
	_ resource.ResourceWithValidateConfig = &portProfileResource{}
)

// tagAllNetworks is networkTagsSetting 0, "tag every network". On a custom
// profile (type 2) the controller accepts it on create but not on update: a
// PATCH carrying 0 is stored as 2 ("tag a specific list") with tagNetworkIds
// emptied, so the ports tag no network at all. Seen twice on controller
// 6.3.0.45, once from a profile created with 0 and once from one already at 2.
// The built-in "All" profile (type 0) is the only one the controller itself
// keeps at 0.
const tagAllNetworks = 0

const tagAllDetail = "On a custom profile the controller accepts network_tags_setting = 0 (tag every network) " +
	"when the profile is created, but an update that carries it is stored as 2 (tag a specific list) with " +
	"tagged_network_ids emptied, which leaves the ports tagging no network. Every update of a profile at 0 " +
	"carries it. Use 2 and list the networks in tagged_network_ids instead."

func NewPortProfileResource() resource.Resource { return &portProfileResource{} }

type portProfileResource struct{ data *providerData }

type portProfileResourceModel struct {
	ID              types.String `tfsdk:"id"`
	Site            types.String `tfsdk:"site"`
	SiteID          types.String `tfsdk:"site_id"`
	Name            types.String `tfsdk:"name"`
	POE             types.Int64  `tfsdk:"poe"`
	NativeNetworkID types.String `tfsdk:"native_network_id"`
	TaggedIDs       types.List   `tfsdk:"tagged_network_ids"`
	UntaggedIDs     types.List   `tfsdk:"untagged_network_ids"`
	VLANConfig      types.Bool   `tfsdk:"vlan_config_enable"`

	NetworkTagsSetting types.Int64 `tfsdk:"network_tags_setting"`
	VoiceNetworkEnable types.Bool  `tfsdk:"voice_network_enable"`
	VoiceDscpEnable    types.Bool  `tfsdk:"voice_dscp_enable"`
	Dot1x              types.Int64 `tfsdk:"dot1x"`
	Dot1pPriority      types.Int64 `tfsdk:"dot1p_priority"`
	TrustMode          types.Int64 `tfsdk:"trust_mode"`
	ProfileType        types.Int64 `tfsdk:"type"`

	PortIsolationEnable     types.Bool  `tfsdk:"port_isolation_enable"`
	LLDPMedEnable           types.Bool  `tfsdk:"lldp_med_enable"`
	BandWidthCtrlType       types.Int64 `tfsdk:"band_width_ctrl_type"`
	LoopbackDetectEnable    types.Bool  `tfsdk:"loopback_detect_enable"`
	LoopbackDetectVlanBased types.Bool  `tfsdk:"loopback_detect_vlan_based"`
	EEEEnable               types.Bool  `tfsdk:"eee_enable"`
	FlowControlEnable       types.Bool  `tfsdk:"flow_control_enable"`
	FastLeaveEnable         types.Bool  `tfsdk:"fast_leave_enable"`
	IGMPFastLeave           types.Bool  `tfsdk:"igmp_fast_leave_enable"`
	MLDFastLeave            types.Bool  `tfsdk:"mld_fast_leave_enable"`
	SupportESEnable         types.Bool  `tfsdk:"support_es_enable"`
	DHCPL2Relay             types.Bool  `tfsdk:"dhcp_l2_relay_enable"`

	SpanningTreeEnable types.Bool  `tfsdk:"spanning_tree_enable"`
	STPPriority        types.Int64 `tfsdk:"stp_priority"`
	STPExtPathCost     types.Int64 `tfsdk:"stp_ext_path_cost"`
	STPIntPathCost     types.Int64 `tfsdk:"stp_int_path_cost"`
	STPP2PLink         types.Int64 `tfsdk:"stp_p2p_link"`
	STPEdgePort        types.Bool  `tfsdk:"stp_edge_port"`
	STPMcheck          types.Bool  `tfsdk:"stp_mcheck"`
	STPLoopProtect     types.Bool  `tfsdk:"stp_loop_protect"`
	STPRootProtect     types.Bool  `tfsdk:"stp_root_protect"`
	STPTCGuard         types.Bool  `tfsdk:"stp_tc_guard"`
	STPBPDUProtect     types.Bool  `tfsdk:"stp_bpdu_protect"`
	STPBPDUFilter      types.Bool  `tfsdk:"stp_bpdu_filter"`
	STPBPDUForward     types.Bool  `tfsdk:"stp_bpdu_forward"`
}

func (r *portProfileResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_port_profile"
}

func (r *portProfileResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	b := func(d string) schema.BoolAttribute {
		return schema.BoolAttribute{Optional: true, Computed: true, MarkdownDescription: d}
	}
	i := func(d string) schema.Int64Attribute {
		return schema.Int64Attribute{Optional: true, Computed: true, MarkdownDescription: d}
	}
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manages a switch port profile: VLAN tagging, PoE, 802.1X, LLDP-MED, loopback detection, spanning tree and more.\n\n" +
			"Unset attributes keep their current controller value; unmodelled keys (including the STP `instances` list) are preserved on update.",
		Attributes: map[string]schema.Attribute{
			"id":      schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"site":    schema.StringAttribute{Optional: true, MarkdownDescription: "Site name. Defaults to the primary site. Changing forces replacement.", PlanModifiers: []planmodifier.String{stringplanmodifier.RequiresReplace()}},
			"site_id": schema.StringAttribute{Computed: true, PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()}},
			"name":    schema.StringAttribute{Required: true, MarkdownDescription: "Profile name."},
			"poe":     i("PoE mode: 0=off, 1=on, 2=keep-device-setting."),
			"native_network_id": schema.StringAttribute{
				Optional: true, Computed: true, MarkdownDescription: "Untagged/native network (VLAN) ID for the port.",
			},
			"tagged_network_ids":   schema.ListAttribute{ElementType: types.StringType, Optional: true, Computed: true, MarkdownDescription: "Tagged (trunk) network IDs."},
			"untagged_network_ids": schema.ListAttribute{ElementType: types.StringType, Optional: true, Computed: true, MarkdownDescription: "Additional untagged network IDs."},
			"vlan_config_enable":   b("Custom VLAN tagging on the profile."),

			"network_tags_setting": i("Network tagging mode: `0` = tag every network, `1` = tag none, `2` = tag the networks in `tagged_network_ids`. " +
				"Use `2` with an explicit list rather than `0`: the controller accepts `0` on create, but an update carrying it " +
				"is stored as `2` with the tagged list emptied, so the provider refuses to update a profile at `0`."),
			"voice_network_enable": b("Voice network."),
			"voice_dscp_enable":    b("Voice DSCP marking."),
			"dot1x":                i("802.1X mode."),
			"dot1p_priority":       i("802.1p priority."),
			"trust_mode":           i("QoS trust mode."),
			"type":                 i("Profile type code."),

			"port_isolation_enable":      b("Port isolation."),
			"lldp_med_enable":            b("LLDP-MED."),
			"band_width_ctrl_type":       i("Bandwidth control type."),
			"loopback_detect_enable":     b("Loopback detection."),
			"loopback_detect_vlan_based": b("VLAN-based loopback detection."),
			"eee_enable":                 b("Energy Efficient Ethernet."),
			"flow_control_enable":        b("Flow control."),
			"fast_leave_enable":          b("Fast leave."),
			"igmp_fast_leave_enable":     b("IGMP fast leave."),
			"mld_fast_leave_enable":      b("MLD fast leave."),
			"support_es_enable":          b("Energy saving support."),
			"dhcp_l2_relay_enable":       b("DHCP L2 relay on this profile."),

			"spanning_tree_enable": b("Spanning tree on this profile."),
			"stp_priority":         i("STP port priority."),
			"stp_ext_path_cost":    i("STP external path cost."),
			"stp_int_path_cost":    i("STP internal path cost."),
			"stp_p2p_link":         i("STP point-to-point link mode."),
			"stp_edge_port":        b("STP edge port."),
			"stp_mcheck":           b("STP mCheck."),
			"stp_loop_protect":     b("STP loop protection."),
			"stp_root_protect":     b("STP root protection."),
			"stp_tc_guard":         b("STP TC guard."),
			"stp_bpdu_protect":     b("STP BPDU protection."),
			"stp_bpdu_filter":      b("STP BPDU filter."),
			"stp_bpdu_forward":     b("STP BPDU forwarding."),
		},
	}
}

// ValidateConfig warns about network_tags_setting = 0. Create works, so an
// existing config that never changes keeps working, but every later update is
// refused (see Update), and the plan is the place to find that out.
func (r *portProfileResource) ValidateConfig(ctx context.Context, req resource.ValidateConfigRequest, resp *resource.ValidateConfigResponse) {
	var cfg portProfileResourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if !cfg.NetworkTagsSetting.IsNull() && !cfg.NetworkTagsSetting.IsUnknown() && cfg.NetworkTagsSetting.ValueInt64() == tagAllNetworks {
		resp.Diagnostics.AddAttributeWarning(path.Root("network_tags_setting"),
			"network_tags_setting = 0 cannot be updated", tagAllDetail)
	}
}

func (r *portProfileResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	if data, ok := req.ProviderData.(*providerData); ok {
		r.data = data
	} else {
		resp.Diagnostics.AddError("Unexpected provider data", fmt.Sprintf("expected *providerData, got %T", req.ProviderData))
	}
}

func (r *portProfileResource) siteName(m portProfileResourceModel) string {
	if !m.Site.IsNull() && m.Site.ValueString() != "" {
		return m.Site.ValueString()
	}
	return r.data.defaultSite
}

func (r *portProfileResource) fieldsFrom(ctx context.Context, m portProfileResourceModel) (map[string]any, diag.Diagnostics) {
	var diags diag.Diagnostics
	tagged, d := stringSlice(ctx, m.TaggedIDs)
	diags.Append(d...)
	untagged, d := stringSlice(ctx, m.UntaggedIDs)
	diags.Append(d...)

	f := map[string]any{
		"name":            m.Name.ValueString(),
		"tagNetworkIds":   nilToEmpty(tagged),
		"untagNetworkIds": nilToEmpty(untagged),
	}
	putBool := func(k string, v types.Bool) {
		if !v.IsNull() && !v.IsUnknown() {
			f[k] = v.ValueBool()
		}
	}
	putInt := func(k string, v types.Int64) {
		if !v.IsNull() && !v.IsUnknown() {
			f[k] = v.ValueInt64()
		}
	}
	putInt("poe", m.POE)
	putBool("vlanConfigEnable", m.VLANConfig)
	if !m.NativeNetworkID.IsNull() && !m.NativeNetworkID.IsUnknown() && m.NativeNetworkID.ValueString() != "" {
		f["nativeNetworkId"] = m.NativeNetworkID.ValueString()
	}
	putInt("networkTagsSetting", m.NetworkTagsSetting)
	putBool("voiceNetworkEnable", m.VoiceNetworkEnable)
	putBool("voiceDscpEnable", m.VoiceDscpEnable)
	putInt("dot1x", m.Dot1x)
	putInt("dot1pPriority", m.Dot1pPriority)
	putInt("trustMode", m.TrustMode)
	putInt("type", m.ProfileType)
	putBool("portIsolationEnable", m.PortIsolationEnable)
	putBool("lldpMedEnable", m.LLDPMedEnable)
	putInt("bandWidthCtrlType", m.BandWidthCtrlType)
	putBool("loopbackDetectEnable", m.LoopbackDetectEnable)
	putBool("loopbackDetectVlanBasedEnable", m.LoopbackDetectVlanBased)
	putBool("eeeEnable", m.EEEEnable)
	putBool("flowControlEnable", m.FlowControlEnable)
	putBool("fastLeaveEnable", m.FastLeaveEnable)
	putBool("igmpFastLeaveEnable", m.IGMPFastLeave)
	putBool("mldFastLeaveEnable", m.MLDFastLeave)
	putBool("supportESEnable", m.SupportESEnable)
	putBool("spanningTreeEnable", m.SpanningTreeEnable)
	if !m.DHCPL2Relay.IsNull() && !m.DHCPL2Relay.IsUnknown() {
		f["dhcpL2RelaySettings"] = map[string]any{"enable": m.DHCPL2Relay.ValueBool()}
	}

	stp := map[string]any{}
	sb := func(k string, v types.Bool) {
		if !v.IsNull() && !v.IsUnknown() {
			stp[k] = v.ValueBool()
		}
	}
	si := func(k string, v types.Int64) {
		if !v.IsNull() && !v.IsUnknown() {
			stp[k] = v.ValueInt64()
		}
	}
	si("priority", m.STPPriority)
	si("extPathCost", m.STPExtPathCost)
	si("intPathCost", m.STPIntPathCost)
	si("p2pLink", m.STPP2PLink)
	sb("edgePort", m.STPEdgePort)
	sb("mcheck", m.STPMcheck)
	sb("loopProtect", m.STPLoopProtect)
	sb("rootProtect", m.STPRootProtect)
	sb("tcGuard", m.STPTCGuard)
	sb("bpduProtect", m.STPBPDUProtect)
	sb("bpduFilter", m.STPBPDUFilter)
	sb("bpduForward", m.STPBPDUForward)
	if len(stp) > 0 {
		f["spanningTreeSetting"] = stp
	}
	return f, diags
}

func (r *portProfileResource) apply(ctx context.Context, p *omada.PortProfile, m *portProfileResourceModel) diag.Diagnostics {
	var diags diag.Diagnostics
	m.ID = types.StringValue(p.ID)
	m.Name = types.StringValue(p.Name)
	m.POE = types.Int64Value(int64(p.POE))
	m.NativeNetworkID = types.StringValue(p.NativeNetworkID)
	m.VLANConfig = types.BoolValue(p.VLANConfigEnable)
	m.NetworkTagsSetting = types.Int64Value(int64(p.NetworkTagsSetting))
	m.VoiceNetworkEnable = types.BoolValue(p.VoiceNetworkEnable)
	m.VoiceDscpEnable = types.BoolValue(p.VoiceDscpEnable)
	m.Dot1x = types.Int64Value(int64(p.Dot1x))
	m.Dot1pPriority = types.Int64Value(int64(p.Dot1pPriority))
	m.TrustMode = types.Int64Value(int64(p.TrustMode))
	m.ProfileType = types.Int64Value(int64(p.Type))
	m.PortIsolationEnable = types.BoolValue(p.PortIsolationEnable)
	m.LLDPMedEnable = types.BoolValue(p.LLDPMedEnable)
	m.BandWidthCtrlType = types.Int64Value(int64(p.BandWidthCtrlType))
	m.LoopbackDetectEnable = types.BoolValue(p.LoopbackDetectEnable)
	m.LoopbackDetectVlanBased = types.BoolValue(p.LoopbackDetectVlanBased)
	m.EEEEnable = types.BoolValue(p.EEEEnable)
	m.FlowControlEnable = types.BoolValue(p.FlowControlEnable)
	m.FastLeaveEnable = types.BoolValue(p.FastLeaveEnable)
	m.IGMPFastLeave = types.BoolValue(p.IGMPFastLeave)
	m.MLDFastLeave = types.BoolValue(p.MLDFastLeave)
	m.SupportESEnable = types.BoolValue(p.SupportESEnable)
	m.DHCPL2Relay = types.BoolValue(p.DHCPL2Relay.Enable)

	m.SpanningTreeEnable = types.BoolValue(p.SpanningTreeEnable)
	m.STPPriority = types.Int64Value(int64(p.SpanningTree.Priority))
	m.STPExtPathCost = types.Int64Value(int64(p.SpanningTree.ExtPathCost))
	m.STPIntPathCost = types.Int64Value(int64(p.SpanningTree.IntPathCost))
	m.STPP2PLink = types.Int64Value(int64(p.SpanningTree.P2PLink))
	m.STPEdgePort = types.BoolValue(p.SpanningTree.EdgePort)
	m.STPMcheck = types.BoolValue(p.SpanningTree.Mcheck)
	m.STPLoopProtect = types.BoolValue(p.SpanningTree.LoopProtect)
	m.STPRootProtect = types.BoolValue(p.SpanningTree.RootProtect)
	m.STPTCGuard = types.BoolValue(p.SpanningTree.TCGuard)
	m.STPBPDUProtect = types.BoolValue(p.SpanningTree.BPDUProtect)
	m.STPBPDUFilter = types.BoolValue(p.SpanningTree.BPDUFilter)
	m.STPBPDUForward = types.BoolValue(p.SpanningTree.BPDUForward)

	tagged, d := stringListValue(ctx, p.TagNetworkIDs)
	diags.Append(d...)
	m.TaggedIDs = tagged
	untagged, d := stringListValue(ctx, p.UntagNetworkIDs)
	diags.Append(d...)
	m.UntaggedIDs = untagged
	return diags
}

func (r *portProfileResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan portProfileResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	siteID, err := r.data.client.ResolveSiteID(ctx, r.siteName(plan))
	if err != nil {
		resp.Diagnostics.AddError("Unable to resolve site", err.Error())
		return
	}
	fields, diags := r.fieldsFrom(ctx, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	created, err := r.data.client.CreatePortProfile(ctx, siteID, fields)
	if err != nil {
		resp.Diagnostics.AddError("Unable to create port profile", err.Error())
		return
	}
	resp.Diagnostics.Append(r.apply(ctx, created, &plan)...)
	plan.SiteID = types.StringValue(siteID)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *portProfileResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state portProfileResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	siteID := state.SiteID.ValueString()
	if siteID == "" {
		var err error
		if siteID, err = r.data.client.ResolveSiteID(ctx, r.siteName(state)); err != nil {
			resp.Diagnostics.AddError("Unable to resolve site", err.Error())
			return
		}
	}
	p, err := r.data.client.GetPortProfile(ctx, siteID, state.ID.ValueString())
	if err != nil {
		resp.State.RemoveResource(ctx)
		return
	}
	resp.Diagnostics.Append(r.apply(ctx, p, &state)...)
	state.SiteID = types.StringValue(siteID)
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *portProfileResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan portProfileResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	siteID, err := r.data.client.ResolveSiteID(ctx, r.siteName(plan))
	if err != nil {
		resp.Diagnostics.AddError("Unable to resolve site", err.Error())
		return
	}
	// Refuse before writing anything: the controller would store this as "tag
	// a specific list" with no networks, and on a trunk that is an outage.
	if !plan.NetworkTagsSetting.IsNull() && !plan.NetworkTagsSetting.IsUnknown() && plan.NetworkTagsSetting.ValueInt64() == tagAllNetworks {
		resp.Diagnostics.AddAttributeError(path.Root("network_tags_setting"),
			"Refusing to update a port profile with network_tags_setting = 0", tagAllDetail)
		return
	}
	fields, diags := r.fieldsFrom(ctx, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	updated, err := r.data.client.UpdatePortProfile(ctx, siteID, plan.ID.ValueString(), fields)
	if err != nil {
		resp.Diagnostics.AddError("Unable to update port profile", err.Error())
		return
	}
	resp.Diagnostics.Append(r.apply(ctx, updated, &plan)...)
	plan.SiteID = types.StringValue(siteID)
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *portProfileResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state portProfileResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	if err := r.data.client.DeletePortProfile(ctx, state.SiteID.ValueString(), state.ID.ValueString()); err != nil {
		resp.Diagnostics.AddError("Unable to delete port profile", err.Error())
	}
}

func (r *portProfileResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	parts := strings.SplitN(req.ID, "/", 2)
	if len(parts) == 2 {
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("site"), parts[0])...)
		resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), parts[1])...)
		return
	}
	resp.Diagnostics.Append(resp.State.SetAttribute(ctx, path.Root("id"), req.ID)...)
}
