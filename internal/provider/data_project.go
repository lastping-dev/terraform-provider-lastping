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
	_ datasource.DataSource              = (*projectDataSource)(nil)
	_ datasource.DataSourceWithConfigure = (*projectDataSource)(nil)
)

// projectDataSourceModel is the answer to "which project is this key for?".
// The whoami endpoint returns exactly one field, so this is the whole shape
// rather than a trimmed-down view of a larger project resource.
type projectDataSourceModel struct {
	ProjectID types.String `tfsdk:"project_id"`
}

// NewProjectDataSource returns a new lastping_project data source.
func NewProjectDataSource() datasource.DataSource {
	return &projectDataSource{}
}

type projectDataSource struct {
	client *client.Client
}

func (d *projectDataSource) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_project"
}

func (d *projectDataSource) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "The project the configured API key belongs to.\n\n" +
			"Every LastPing resource is scoped to the key's project, so this is how a configuration " +
			"discovers which project it is about to change. It doubles as a credential smoke-test: an " +
			"invalid or revoked key fails here, at plan time, rather than partway through an apply.",
		Attributes: map[string]schema.Attribute{
			"project_id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "UUID of the project the API key authenticates against.",
			},
		},
	}
}

func (d *projectDataSource) Configure(_ context.Context, req datasource.ConfigureRequest, resp *datasource.ConfigureResponse) {
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

func (d *projectDataSource) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	out, err := d.client.Whoami(ctx)
	if err != nil {
		resp.Diagnostics.AddError("Unable to read project", err.Error())
		return
	}
	state := projectDataSourceModel{ProjectID: types.StringValue(out.ProjectID)}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
