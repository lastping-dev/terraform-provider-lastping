package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/lastping-dev/terraform-provider-lastping/internal/client"
)

var (
	_ datasource.DataSource              = (*monitorsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*monitorsDataSource)(nil)
)

// monitorsDataSourceModel is the lastping_monitors result set.
//
// Tag is plain Optional, never Optional+Computed: it is a query filter the API
// does not echo back, so there is nothing for the server to compute and marking
// it Computed would let a stale value linger in state.
type monitorsDataSourceModel struct {
	Tag      types.String       `tfsdk:"tag"`
	Monitors []monitorDataModel `tfsdk:"monitors"`
}

// NewMonitorsDataSource returns a new lastping_monitors data source.
func NewMonitorsDataSource() datasource.DataSource {
	return &monitorsDataSource{}
}

type monitorsDataSource struct {
	client *client.Client
}

func (d *monitorsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_monitors"
}

func (d *monitorsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Every monitor in the project the API key belongs to, optionally filtered " +
			"by tag.\n\nUseful for building a `lastping_status_page` from a tag rather than a hand-maintained " +
			"list of ids, and for auditing monitors created outside Terraform.",
		Attributes: map[string]schema.Attribute{
			"tag": schema.StringAttribute{
				Optional: true,
				MarkdownDescription: "Return only monitors carrying this exact tag, for example " +
					"`agent:claude`. Omit for every monitor in the project. Matching is exact, not a " +
					"prefix or substring match.",
			},
			"monitors": schema.ListNestedAttribute{
				Computed: true,
				MarkdownDescription: "Matching monitors, in the order the API returned them. Empty when " +
					"nothing matches — an empty result is not an error.",
				NestedObject: schema.NestedAttributeObject{Attributes: monitorDataAttributes()},
			},
		},
	}
}

func (d *monitorsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	c, ok := req.ProviderData.(*client.Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected data source configure type",
			fmt.Sprintf("Expected *client.Client, got %T. This is a provider bug.", req.ProviderData))
		return
	}
	d.client = c
}

func (d *monitorsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg monitorsDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	list, err := d.client.ListMonitors(ctx, cfg.Tag.ValueString())
	if err != nil {
		resp.Diagnostics.AddError("Unable to list monitors", err.Error())
		return
	}

	state := monitorsDataSourceModel{Tag: cfg.Tag, Monitors: make([]monitorDataModel, 0, len(list))}
	for i := range list {
		m, diags := monitorDataFromAPI(ctx, &list[i])
		resp.Diagnostics.Append(diags...)
		if resp.Diagnostics.HasError() {
			return
		}
		state.Monitors = append(state.Monitors, m)
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
