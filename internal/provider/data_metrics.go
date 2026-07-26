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
	_ datasource.DataSource              = (*metricsDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*metricsDataSource)(nil)
)

// metricsDataSourceModel carries the metrics endpoint's body verbatim.
//
// Text is deliberately not parsed into attributes. GET /api/v1/metrics is the
// one endpoint that does not answer in JSON — it emits Prometheus text
// exposition format 0.0.4 — and the metric set it exposes is documented to grow.
// Projecting today's metric names onto Terraform attributes would freeze that
// list into the provider's schema and turn every addition on the server into a
// provider release.
type metricsDataSourceModel struct {
	Text types.String `tfsdk:"text"`
}

// NewMetricsDataSource returns a new lastping_metrics data source.
func NewMetricsDataSource() datasource.DataSource {
	return &metricsDataSource{}
}

type metricsDataSource struct {
	client *client.Client
}

func (d *metricsDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_metrics"
}

func (d *metricsDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The project's monitors rendered as Prometheus gauges, exactly as " +
			"`GET /api/v1/metrics` returns them.\n\n" +
			"Intended for writing a scrape snapshot to disk or feeding a dashboard module. Note that " +
			"the body changes as monitors ping, so a configuration that interpolates it produces a diff " +
			"on nearly every plan.",
		Attributes: map[string]schema.Attribute{
			"text": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Prometheus text exposition format 0.0.4. Includes " +
					"`lastping_checks`, and per monitor `lastping_check_up`, `lastping_check_state`, " +
					"`lastping_check_grace_seconds`, `lastping_check_paused`, and the " +
					"`*_timestamp_seconds` gauges.",
			},
		},
	}
}

func (d *metricsDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *metricsDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	text, err := d.client.GetMetrics(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Unable to read metrics", err.Error())
		return
	}
	state := metricsDataSourceModel{Text: types.StringValue(text)}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
