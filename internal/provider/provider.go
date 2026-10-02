// Copyright (c) wncservices
// SPDX-License-Identifier: MPL-2.0

package provider

import (
	"context"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/wncservices/terraform-provider-omada/internal/omada"
)

// Ensure OmadaProvider satisfies the provider.Provider interface.
var _ provider.Provider = &OmadaProvider{}

// OmadaProvider is the provider implementation.
type OmadaProvider struct {
	version string
}

// OmadaProviderModel maps provider schema data to a Go type.
type OmadaProviderModel struct {
	URL           types.String `tfsdk:"url"`
	Username      types.String `tfsdk:"username"`
	Password      types.String `tfsdk:"password"`
	TOTPSecret    types.String `tfsdk:"totp_secret"`
	SkipTLSVerify types.Bool   `tfsdk:"skip_tls_verify"`
	Site          types.String `tfsdk:"site"`

	OpenAPIClientID     types.String `tfsdk:"openapi_client_id"`
	OpenAPIClientSecret types.String `tfsdk:"openapi_client_secret"`
}

func New(version string) func() provider.Provider {
	return func() provider.Provider {
		return &OmadaProvider{version: version}
	}
}

func (p *OmadaProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "omada"
	resp.Version = p.version
}

func (p *OmadaProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manage TP-Link Omada controller (v6, OC200/OC300 or Software Controller) configuration as infrastructure-as-code.",
		Attributes: map[string]schema.Attribute{
			"url": schema.StringAttribute{
				MarkdownDescription: "Base URL of the Omada controller, e.g. `https://10.0.0.2:443`. May also be set via `OMADA_URL`.",
				Optional:            true,
			},
			"username": schema.StringAttribute{
				MarkdownDescription: "Controller admin username. May also be set via `OMADA_USERNAME`.",
				Optional:            true,
			},
			"password": schema.StringAttribute{
				MarkdownDescription: "Controller admin password. May also be set via `OMADA_PASSWORD`.",
				Optional:            true,
				Sensitive:           true,
			},
			"totp_secret": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "Authenticator-app (TOTP) secret for `username`, used only when the " +
					"controller enforces two-factor authentication. Accepts the bare base32 secret or the " +
					"whole `otpauth://` URL behind the enrolment QR code. May also be set via " +
					"`OMADA_TOTP_SECRET`.\n\n" +
					"A controller with 2FA enforced (*Global View → Settings → Account Security*) refuses a " +
					"password-only login with error `-30165`, which no amount of retrying fixes. With this " +
					"set, the provider answers the challenge the way the controller's own login page does. " +
					"Only authenticator-app codes work — an account set up for emailed codes cannot be " +
					"automated.\n\n" +
					"~> **This weakens the second factor for this account.** The secret sits next to the " +
					"password, so anything that can read one can read the other. It is worth it to keep 2FA " +
					"enforced for every human account on the controller; it is not a substitute for a " +
					"dedicated, least-privilege account.",
			},
			"openapi_client_id": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Client ID of an application registered under *Settings → Platform " +
					"Integration → Open API*. Only needed for the few capabilities the controller exposes " +
					"solely through its Open API. May also be set via `OMADA_OPENAPI_CLIENT_ID`.",
			},
			"openapi_client_secret": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "Client secret paired with `openapi_client_id`. May also be set via " +
					"`OMADA_OPENAPI_CLIENT_SECRET`.\n\n" +
					"This is **not** the admin password: the controller refuses a web session on the Open " +
					"API with error `-44116`, so the two credential sets are genuinely separate.",
			},
			"skip_tls_verify": schema.BoolAttribute{
				MarkdownDescription: "Skip TLS verification of the controller's (typically self-signed) certificate. Defaults to `true`. May also be set via `OMADA_SKIP_TLS_VERIFY`.",
				Optional:            true,
			},
			"site": schema.StringAttribute{
				MarkdownDescription: "Default site name used by site-scoped resources when they don't set one explicitly. Defaults to the controller's **primary** site (real sites are often named e.g. `Home`, not `Default`). May also be set via `OMADA_SITE`.",
				Optional:            true,
			},
		},
	}
}

func (p *OmadaProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var config OmadaProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Attribute wins over env var; env var provides the fallback.
	url := firstNonEmpty(config.URL, os.Getenv("OMADA_URL"))
	username := firstNonEmpty(config.Username, os.Getenv("OMADA_USERNAME"))
	password := firstNonEmpty(config.Password, os.Getenv("OMADA_PASSWORD"))
	totpSecret := firstNonEmpty(config.TOTPSecret, os.Getenv("OMADA_TOTP_SECRET"))

	if url == "" {
		resp.Diagnostics.AddAttributeError(pathRoot("url"), "Missing controller URL",
			"Set the provider `url` attribute or the OMADA_URL environment variable.")
	}
	if username == "" {
		resp.Diagnostics.AddAttributeError(pathRoot("username"), "Missing controller username",
			"Set the provider `username` attribute or the OMADA_USERNAME environment variable.")
	}
	if password == "" {
		resp.Diagnostics.AddAttributeError(pathRoot("password"), "Missing controller password",
			"Set the provider `password` attribute or the OMADA_PASSWORD environment variable.")
	}
	if resp.Diagnostics.HasError() {
		return
	}

	// Defaults to true — controllers ship self-signed certs.
	skipTLS := true
	if !config.SkipTLSVerify.IsNull() {
		skipTLS = config.SkipTLSVerify.ValueBool()
	} else if v := os.Getenv("OMADA_SKIP_TLS_VERIFY"); v == "false" || v == "0" {
		skipTLS = false
	}

	// Empty site means "use the controller's primary site" — resolved lazily by
	// the client so we don't hard-code a name like "Default" (real sites are
	// often named e.g. "Home").
	site := firstNonEmpty(config.Site, os.Getenv("OMADA_SITE"))

	// Open API credentials are optional: only a few capabilities need them, and
	// most configurations never will. Supplying one without the other is a
	// mistake worth catching here rather than at the first Open API call.
	openAPIID := firstNonEmpty(config.OpenAPIClientID, os.Getenv("OMADA_OPENAPI_CLIENT_ID"))
	openAPISecret := firstNonEmpty(config.OpenAPIClientSecret, os.Getenv("OMADA_OPENAPI_CLIENT_SECRET"))
	switch {
	case openAPIID != "" && openAPISecret == "":
		resp.Diagnostics.AddAttributeError(pathRoot("openapi_client_secret"),
			"Missing Open API client secret",
			"`openapi_client_id` was set without `openapi_client_secret`; the Open API needs both.")
	case openAPISecret != "" && openAPIID == "":
		resp.Diagnostics.AddAttributeError(pathRoot("openapi_client_id"),
			"Missing Open API client ID",
			"`openapi_client_secret` was set without `openapi_client_id`; the Open API needs both.")
	}
	if resp.Diagnostics.HasError() {
		return
	}

	client, err := omada.NewClientWithConfig(ctx, omada.Config{
		URL:                 url,
		Username:            username,
		Password:            password,
		TOTPSecret:          totpSecret,
		OpenAPIClientID:     openAPIID,
		OpenAPIClientSecret: openAPISecret,
		SkipTLSVerify:       skipTLS,
	})
	if err != nil {
		resp.Diagnostics.AddError("Unable to connect to the Omada controller", err.Error())
		return
	}

	// Hand a configured client + default site to data sources and resources.
	data := &providerData{client: client, defaultSite: site}
	resp.DataSourceData = data
	resp.ResourceData = data
}

func (p *OmadaProvider) DataSources(_ context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		NewSitesDataSource,
		NewNetworksDataSource,
		NewWANDataSource,
		NewPortForwardsDataSource,
		NewFirewallACLsDataSource,
		NewDevicesDataSource,
	}
}

func (p *OmadaProvider) Resources(_ context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewNetworkResource,
		NewLanDNSResource,
		NewPortForwardResource,
		NewIPGroupResource,
		NewPortGroupResource,
		NewFirewallACLResource,
		NewWLANGroupResource,
		NewMDNSReflectorResource,
		NewMDNSProfileResource,
		NewPortProfileResource,
		NewWirelessNetworkResource,
		NewVPNResource,
		NewStaticRouteResource,
		NewPortalResource,
		NewSiteSettingsResource,
		NewNotificationSettingsResource,
		NewAuditNotificationResource,
		NewAttackDefenseResource,
		NewMACFilterResource,
		NewALGResource,
		NewSessionLimitResource,
		NewGatewayBandwidthControlResource,
		NewPortalAccessControlResource,
		NewSwitchPortResource,
		NewWANIPv6Resource,
		NewGRETunnelResource,
		NewIoTRadioResource,
		NewIoTServerResource,
		NewIoTBeaconResource,
		NewIPTVResource,
		NewDNSProxyResource,
		NewAccessPointResource,
		NewGatewayResource,
		NewSSHSettingsResource,
		NewDot1XResource,
		NewMACAuthResource,
		NewSNMPResource,
		NewUPnPResource,
		NewIPSResource,
		NewIPSWhitelistResource,
		NewTimeRangeResource,
		NewRateLimitProfileResource,
		NewServiceTypeResource,
		NewQoSBandwidthControlResource,
		NewDisableNATResource,
		NewDHCPReservationResource,
		NewRadiusProfileResource,

		// Controller-scoped: no site in the path, no `site` attribute.
		NewControllerSettingsResource,
		NewFirmwareUpgradeScheduleResource,
		NewBackupResource,
	}
}

// providerData is passed to every data source / resource via Configure.
type providerData struct {
	client      *omada.Client
	defaultSite string
}

func firstNonEmpty(v types.String, fallback string) string {
	if !v.IsNull() && v.ValueString() != "" {
		return v.ValueString()
	}
	return fallback
}
