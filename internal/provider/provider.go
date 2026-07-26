package provider

import (
	"context"
	"os"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/ephemeral"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/lastping-dev/terraform-provider-lastping/internal/client"
)

const defaultEndpoint = "https://app.lastping.dev"

var (
	_ provider.Provider                       = (*lastpingProvider)(nil)
	_ provider.ProviderWithEphemeralResources = (*lastpingProvider)(nil)
)

type lastpingProvider struct{ version string }

// New returns the provider factory. version is stamped by GoReleaser.
func New(version string) func() provider.Provider {
	return func() provider.Provider { return &lastpingProvider{version: version} }
}

type providerModel struct {
	Endpoint types.String `tfsdk:"endpoint"`
	APIKey   types.String `tfsdk:"api_key"`
}

func (p *lastpingProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "lastping"
	resp.Version = p.version
}

func (p *lastpingProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Manage LastPing monitors, destinations, routing, alert messages, " +
			"status pages and API keys as code.",
		Attributes: map[string]schema.Attribute{
			"endpoint": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "LastPing API base URL. Defaults to `" + defaultEndpoint +
					"`. May also be set with the `LASTPING_ENDPOINT` environment variable.",
			},
			"api_key": schema.StringAttribute{
				Optional:  true,
				Sensitive: true,
				MarkdownDescription: "LastPing API key (`lp_…`). May also be set with the " +
					"`LASTPING_API_KEY` environment variable, which is preferred so the key " +
					"does not appear in configuration. Create one at " +
					"https://app.lastping.dev/app/settings.",
			},
		},
	}
}

func (p *lastpingProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var cfg providerModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	endpoint := os.Getenv("LASTPING_ENDPOINT")
	if !cfg.Endpoint.IsNull() && cfg.Endpoint.ValueString() != "" {
		endpoint = cfg.Endpoint.ValueString()
	}
	if endpoint == "" {
		endpoint = defaultEndpoint
	}

	apiKey := os.Getenv("LASTPING_API_KEY")
	if !cfg.APIKey.IsNull() && cfg.APIKey.ValueString() != "" {
		apiKey = cfg.APIKey.ValueString()
	}
	if apiKey == "" {
		resp.Diagnostics.AddAttributeError(
			path.Root("api_key"),
			"Missing LastPing API key",
			"Set the api_key provider attribute or the LASTPING_API_KEY environment variable. "+
				"Create a key at https://app.lastping.dev/app/settings.",
		)
		return
	}

	c := client.New(endpoint, apiKey, p.version)
	resp.ResourceData = c
	resp.DataSourceData = c
	resp.EphemeralResourceData = c
}

func (p *lastpingProvider) Resources(context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		NewMonitorResource,
	}
}

func (p *lastpingProvider) DataSources(context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{}
}

func (p *lastpingProvider) EphemeralResources(context.Context) []func() ephemeral.EphemeralResource {
	return []func() ephemeral.EphemeralResource{}
}
