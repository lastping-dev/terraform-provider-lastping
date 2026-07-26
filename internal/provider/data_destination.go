package provider

import (
	"context"
	"fmt"

	"github.com/hashicorp/terraform-plugin-framework-validators/datasourcevalidator"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	"github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/lastping-dev/terraform-provider-lastping/internal/client"
)

var (
	_ datasource.DataSource                     = (*destinationDataSource)(nil)
	_ datasource.DataSourceWithConfigure        = (*destinationDataSource)(nil)
	_ datasource.DataSourceWithConfigValidators = (*destinationDataSource)(nil)
)

// destinationDataSourceModel is one alert destination as the API reports it.
//
// SECURITY: there are no credential attributes here, and there cannot be. The
// API never returns a destination's config — only the non-secret `target` hint
// — so a data source can expose nothing a lookup could not already see.
type destinationDataSourceModel struct {
	ID            types.String `tfsdk:"id"`
	Name          types.String `tfsdk:"name"`
	Kind          types.String `tfsdk:"kind"`
	Target        types.String `tfsdk:"target"`
	Verified      types.Bool   `tfsdk:"verified"`
	Disabled      types.Bool   `tfsdk:"disabled"`
	DisableReason types.String `tfsdk:"disable_reason"`
	CreatedAt     types.String `tfsdk:"created_at"`
}

// NewDestinationDataSource returns a new lastping_destination data source.
func NewDestinationDataSource() datasource.DataSource {
	return &destinationDataSource{}
}

type destinationDataSource struct {
	client *client.Client
}

func (d *destinationDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_destination"
}

func (d *destinationDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "Look up a single alert destination by `id` or `name`.\n\n" +
			"Credentials are never returned by the API, so this data source reports only the non-secret " +
			"`target` hint. Destination names are not unique: a name matching more than one destination " +
			"is an error, not an arbitrary pick — look those up by `id`.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Destination UUID. Supply this or `name`, not both. Computed when " +
					"the destination is looked up by name.",
			},
			"name": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "Human-readable name. Supply this or `id`, not both. Computed when " +
					"the destination is looked up by id.",
			},
			"kind": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Delivery mechanism: `webhook`, `telegram`, `discord`, `slack`, " +
					"`msteams`, `googlechat`, `ntfy`, `pushover`, or `email`.",
			},
			"target": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Non-secret delivery hint: the URL, address or topic for the kinds " +
					"where that value is not itself a credential, and a fixed label such as `slack " +
					"webhook` for the kinds where it is.",
			},
			"verified": schema.BoolAttribute{
				Computed: true,
				MarkdownDescription: "Whether the destination is confirmed. Only `email` destinations " +
					"start unverified, and they do not receive alerts until they are.",
			},
			"disabled": schema.BoolAttribute{
				Computed:            true,
				MarkdownDescription: "Whether LastPing disabled this destination after repeated delivery failures.",
			},
			"disable_reason": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Why the destination was disabled. Null while it is healthy.",
			},
			"created_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "RFC 3339 UTC timestamp when the destination was created.",
			},
		},
	}
}

func (d *destinationDataSource) ConfigValidators(context.Context) []datasource.ConfigValidator {
	return []datasource.ConfigValidator{
		datasourcevalidator.ExactlyOneOf(path.MatchRoot("id"), path.MatchRoot("name")),
	}
}

func (d *destinationDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *destinationDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg destinationDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	var (
		out *client.Destination
		err error
	)
	switch {
	case !cfg.ID.IsNull():
		out, err = d.client.GetDestination(ctx, cfg.ID.ValueString())
	default:
		out, err = d.client.GetDestinationByName(ctx, cfg.Name.ValueString())
	}
	if err != nil {
		resp.Diagnostics.AddError("Unable to read destination", err.Error())
		return
	}

	state := destinationDataSourceModel{
		ID:            types.StringValue(out.ID),
		Name:          types.StringValue(out.Name),
		Kind:          types.StringValue(out.Kind),
		Target:        stringOrNull(out.Target),
		Verified:      types.BoolValue(out.Verified),
		Disabled:      types.BoolValue(out.Disabled),
		DisableReason: stringOrNull(out.DisableReason),
		CreatedAt:     stringOrNull(out.CreatedAt),
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
