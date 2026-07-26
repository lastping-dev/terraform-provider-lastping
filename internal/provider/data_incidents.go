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
	_ datasource.DataSource              = (*incidentsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*incidentsDataSource)(nil)
)

// incidentDataModel is one incident: a window during which a monitor was not
// healthy.
type incidentDataModel struct {
	OpenedAt types.String `tfsdk:"opened_at"`
	ClosedAt types.String `tfsdk:"closed_at"`
	Cause    types.String `tfsdk:"cause"`
	Detail   types.String `tfsdk:"detail"`
}

// incidentsDataSourceModel is the lastping_incidents result set.
//
// MonitorID is Required because the public API exposes incidents only per
// monitor (GET /api/v1/checks/{id}/incidents). There is no project-wide
// incident endpoint, so a project-wide data source would have to fan out over
// every monitor — one request per monitor on every plan — and would silently
// change cost and meaning if such an endpoint ever appeared.
type incidentsDataSourceModel struct {
	MonitorID types.String        `tfsdk:"monitor_id"`
	Limit     types.Int64         `tfsdk:"limit"`
	Incidents []incidentDataModel `tfsdk:"incidents"`
}

// NewIncidentsDataSource returns a new lastping_incidents data source.
func NewIncidentsDataSource() datasource.DataSource {
	return &incidentsDataSource{}
}

type incidentsDataSource struct {
	client *client.Client
}

func (d *incidentsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_incidents"
}

func (d *incidentsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "One monitor's incident history, newest first.\n\n" +
			"`monitor_id` is required: the API exposes incidents only per monitor, so there is no " +
			"project-wide form of this query.\n\n" +
			"Incidents change without Terraform doing anything, so a configuration that interpolates " +
			"them produces a diff whenever the monitor's history moves. Use it for outputs and reports, " +
			"not as an input to a managed resource.",
		Attributes: map[string]schema.Attribute{
			"monitor_id": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "UUID of the monitor whose incidents to read.",
			},
			"limit": schema.Int64Attribute{
				Optional: true,
				MarkdownDescription: "Maximum number of incidents to return. Omit for the server " +
					"default of 50. The server caps the result at 200 and clamps anything larger " +
					"rather than rejecting it.",
			},
			"incidents": schema.ListNestedAttribute{
				Computed: true,
				MarkdownDescription: "Matching incidents, newest first. Empty for a monitor that has " +
					"never had one.",
				NestedObject: schema.NestedAttributeObject{
					Attributes: map[string]schema.Attribute{
						"opened_at": schema.StringAttribute{
							Computed:            true,
							MarkdownDescription: "RFC 3339 UTC timestamp when the incident opened.",
						},
						"closed_at": schema.StringAttribute{
							Computed: true,
							MarkdownDescription: "RFC 3339 UTC timestamp when the incident was " +
								"resolved. Null while it is still open.",
						},
						"cause": schema.StringAttribute{
							Computed: true,
							MarkdownDescription: "Machine-readable cause, for example `late`, `fail`, " +
								"or `runaway`.",
						},
						"detail": schema.StringAttribute{
							Computed: true,
							MarkdownDescription: "Failure detail attached to the incident by a CI " +
								"workflow's failure-capture step. Null when none was posted.",
						},
					},
				},
			},
		},
	}
}

func (d *incidentsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *incidentsDataSource) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var cfg incidentsDataSourceModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &cfg)...)
	if resp.Diagnostics.HasError() {
		return
	}

	list, err := d.client.ListIncidents(ctx, cfg.MonitorID.ValueString(), cfg.Limit.ValueInt64())
	if err != nil {
		resp.Diagnostics.AddError("Unable to read incidents", err.Error())
		return
	}

	state := incidentsDataSourceModel{
		MonitorID: cfg.MonitorID,
		Limit:     cfg.Limit,
		Incidents: make([]incidentDataModel, 0, len(list)),
	}
	for _, inc := range list {
		state.Incidents = append(state.Incidents, incidentDataModel{
			OpenedAt: types.StringValue(inc.OpenedAt),
			ClosedAt: timestampOrNull(inc.ClosedAt),
			Cause:    stringOrNull(inc.Cause),
			Detail:   stringOrNull(inc.Detail),
		})
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
