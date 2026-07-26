package provider

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/hashicorp/terraform-plugin-framework-validators/listvalidator"
	"github.com/hashicorp/terraform-plugin-framework-validators/stringvalidator"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/path"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringdefault"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/schema/validator"
	"github.com/hashicorp/terraform-plugin-framework/types"

	"github.com/lastping-dev/terraform-provider-lastping/internal/client"
)

var (
	_ resource.Resource                = (*statusPageResource)(nil)
	_ resource.ResourceWithConfigure   = (*statusPageResource)(nil)
	_ resource.ResourceWithImportState = (*statusPageResource)(nil)
)

// NewStatusPageResource returns a new lastping_status_page resource.
func NewStatusPageResource() resource.Resource {
	return &statusPageResource{}
}

type statusPageResource struct {
	client *client.Client
}

// statusPageResourceModel is the Terraform representation of a
// lastping_status_page.
//
// Slug is the one attribute here that is Optional *and* Computed, and it earns
// it: the server genuinely supplies a value when none is configured
// (api/statuspages.go: generateStatusPageSlug). check_ids is plain Optional so
// that removing it from the configuration actually empties the page.
type statusPageResourceModel struct {
	ID         types.String `tfsdk:"id"`
	Slug       types.String `tfsdk:"slug"`
	Title      types.String `tfsdk:"title"`
	Visibility types.String `tfsdk:"visibility"`
	CheckIDs   types.List   `tfsdk:"check_ids"`

	// Computed.
	PublicURL types.String `tfsdk:"public_url"`
	CreatedAt types.String `tfsdk:"created_at"`
}

func (r *statusPageResource) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_status_page"
}

func (r *statusPageResource) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = schema.Schema{
		MarkdownDescription: "A status page: the current state of a chosen set of monitors, rendered at " +
			"`/status/<slug>` for a `public` page or visible only to your account for a `private` one.\n\n" +
			"~> **Renaming a public status page releases its slug.** Slugs are global across all LastPing " +
			"accounts, and Terraform destroys the old page before creating the new one by default — so a failure " +
			"mid-replace can leave you with neither. Add `lifecycle { create_before_destroy = true }` to public " +
			"status pages.\n\n" +
			"The free tier allows **one public status page per project**; private pages are unlimited.",
		Attributes: map[string]schema.Attribute{
			"id": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "Status page UUID, assigned by the server.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"slug": schema.StringAttribute{
				Optional: true,
				Computed: true,
				MarkdownDescription: "The page's identifier in its public URL, `/status/<slug>`. " +
					"**Slugs are globally unique across every LastPing account**, not scoped to your project — " +
					"a public URL can only belong to one page anywhere — so a slug you have never seen may " +
					"already be taken by a stranger, and a configuration that applied cleanly yesterday can fail " +
					"to recreate today for that reason alone. Omit it and the server generates a random " +
					"10-character slug, which is the safest choice for a private page; set it only when the URL " +
					"itself matters. The API has no way to rename a page without releasing the old slug, so " +
					"changing this attribute replaces the resource — read the note at the top of this page " +
					"before doing that to a public one. Must be given already normalised: lowercase, trimmed, " +
					"matching `^[a-z0-9][a-z0-9-]{1,48}[a-z0-9]$`, and not UUID-shaped.",
				Validators: []validator.String{
					stringvalidator.RegexMatches(slugPattern,
						"must be lowercase and trimmed, 3-50 characters of [a-z0-9-] starting and ending "+
							"alphanumeric (the server normalises slugs, so pass the lowercase/trimmed form "+
							"here — for example \"platform\", not \" Platform \")"),
					notUUIDSlugValidator{},
				},
				PlanModifiers: []planmodifier.String{
					// UseStateForUnknown first: without it an unconfigured slug
					// plans as unknown on every change, which the replace check
					// below would then read as a rename of a page the operator
					// never touched.
					stringplanmodifier.UseStateForUnknown(),
					stringplanmodifier.RequiresReplaceIfConfigured(),
				},
			},
			"title": schema.StringAttribute{
				Required:            true,
				MarkdownDescription: "Heading shown on the page. Renaming is an in-place update.",
			},
			"visibility": schema.StringAttribute{
				Optional: true,
				Computed: true,
				Default:  stringdefault.StaticString("private"),
				MarkdownDescription: "`private` (the default — only your account can view it) or `public` " +
					"(anyone with the URL). Switching a page to `public` fails when the project already has a " +
					"public page on the free tier.",
				Validators: []validator.String{stringvalidator.OneOf("public", "private")},
			},
			"check_ids": schema.ListAttribute{
				Optional:    true,
				ElementType: types.StringType,
				MarkdownDescription: "Monitors shown on the page, in display order. Every id must belong to " +
					"this project. Omit or empty it for a page with no monitors on it. Duplicates are rejected " +
					"at plan time because the API silently de-duplicates them, which would otherwise fail the " +
					"apply as an inconsistent result.",
				Validators: []validator.List{
					listvalidator.UniqueValues(),
					listvalidator.ValueStringsAre(uuidValidator{}),
				},
			},
			"public_url": schema.StringAttribute{
				Computed: true,
				MarkdownDescription: "Full URL of the published page. Empty while `visibility = \"private\"`, " +
					"since a private page has no public address.",
			},
			"created_at": schema.StringAttribute{
				Computed:            true,
				MarkdownDescription: "RFC 3339 UTC timestamp when the page was created.",
				PlanModifiers:       []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
		},
	}
}

func (r *statusPageResource) Configure(_ context.Context, req resource.ConfigureRequest, resp *resource.ConfigureResponse) {
	if req.ProviderData == nil {
		return
	}
	c, ok := req.ProviderData.(*client.Client)
	if !ok {
		resp.Diagnostics.AddError("Unexpected resource configure type",
			fmt.Sprintf("Expected *client.Client, got %T. This is a provider bug.", req.ProviderData))
		return
	}
	r.client = c
}

// statusPageCheckIDs reads the configured monitor list.
func statusPageCheckIDs(ctx context.Context, m statusPageResourceModel) ([]string, diag.Diagnostics) {
	ids := []string{}
	if m.CheckIDs.IsNull() || m.CheckIDs.IsUnknown() {
		return ids, nil
	}
	diags := m.CheckIDs.ElementsAs(ctx, &ids, false)
	return ids, diags
}

// checkIDsValue maps the API's monitor list back to state. The API always
// returns an array, never null, so "no monitors" has to be mapped onto whichever
// empty form the configuration used — null when check_ids is absent, [] when it
// is explicitly empty — or the applied state would not match the planned state.
func checkIDsValue(ctx context.Context, apiIDs []string, prior types.List) (types.List, diag.Diagnostics) {
	if len(apiIDs) == 0 {
		// Only reuse prior's shape when prior was itself empty (an explicit
		// check_ids = []). A non-empty prior means the API actually dropped
		// monitors Terraform thought were still listed — that must surface as
		// null, not be masked by echoing the stale value back.
		if prior.IsNull() || prior.IsUnknown() || len(prior.Elements()) > 0 {
			return types.ListNull(types.StringType), nil
		}
		return prior, nil
	}
	return types.ListValueFrom(ctx, types.StringType, apiIDs)
}

// modelFromStatusPage builds Terraform state from an API response. prior is the
// plan (create/update) or the previous state (read), consulted only for the
// null-versus-empty shape of check_ids.
func modelFromStatusPage(ctx context.Context, sp *client.StatusPage, prior statusPageResourceModel) (statusPageResourceModel, diag.Diagnostics) {
	ids, diags := checkIDsValue(ctx, sp.CheckIDs, prior.CheckIDs)
	return statusPageResourceModel{
		ID:         types.StringValue(sp.ID),
		Slug:       types.StringValue(sp.Slug),
		Title:      types.StringValue(sp.Title),
		Visibility: types.StringValue(sp.Visibility),
		CheckIDs:   ids,
		// public_url is "" for a private page, which is the API's own way of
		// saying "not published" — mapping it to null would make an attribute
		// that is always set by the server look sometimes-absent.
		PublicURL: types.StringValue(sp.PublicURL),
		CreatedAt: types.StringValue(sp.CreatedAt),
	}, diags
}

// addSlugConflictDiag reports the 409 the API returns for a slug that is already
// registered.
//
// This is the only error in the provider that a *different customer* can cause,
// so it gets its own diagnostic instead of the generic "unable to create":
// telling someone their apply failed without saying the namespace is global
// sends them hunting for a bug in their own configuration that is not there.
// Both remedies are offered because both are legitimate — the page may be
// someone else's (pick another slug) or their own (import it).
func (r *statusPageResource) addSlugConflictDiag(diags *diag.Diagnostics, slug string) {
	diags.AddAttributeError(
		path.Root("slug"),
		"Status page slug already taken",
		fmt.Sprintf("The slug %q is already in use.\n\n"+
			"Status page slugs are GLOBAL across all LastPing accounts, because the public URL "+
			"%s/status/%s can only belong to one page — so this slug may well be held by another "+
			"account rather than by you, and there is no way to see whose it is.\n\n"+
			"Choose a different slug, or omit `slug` entirely to have one generated. If you believe you "+
			"own this page already, import it instead of creating it:\n"+
			"  terraform import lastping_status_page.<name> %s",
			slug, r.client.BaseURL, slug, slug),
	)
}

// addPublicPageLimitDiag reports the free-tier cap of one public page per
// project.
//
// It is kept strictly separate from the slug conflict above. Both are 4xx
// failures when creating a public page, but this one is about the caller's own
// plan and is fixed by making the page private or removing the other one —
// advice that would be actively misleading if it mentioned slugs at all.
func addPublicPageLimitDiag(diags *diag.Diagnostics) {
	diags.AddAttributeError(
		path.Root("visibility"),
		"Project already has a public status page",
		"The free tier allows one public status page per project, and this project already has one. "+
			"This has nothing to do with the slug — the name is available, the plan limit is not.\n\n"+
			"Set visibility = \"private\", or remove the existing public page first.",
	)
}

func (r *statusPageResource) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan statusPageResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}

	ids, diags := statusPageCheckIDs(ctx, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// An unknown slug is an omitted one: the server generates it.
	slug := ""
	if !plan.Slug.IsUnknown() && !plan.Slug.IsNull() {
		slug = plan.Slug.ValueString()
	}

	out, err := r.client.CreateStatusPage(ctx, client.StatusPage{
		Slug:       slug,
		Title:      plan.Title.ValueString(),
		Visibility: plan.Visibility.ValueString(),
		CheckIDs:   ids,
	})
	if err != nil {
		switch {
		// A generated slug never reaches this branch: the server retries its own
		// collisions internally and only ever returns 409 for a slug the caller
		// chose.
		case client.IsSlugConflict(err) && slug != "":
			r.addSlugConflictDiag(&resp.Diagnostics, slug)
		case client.IsPublicPageLimit(err):
			addPublicPageLimitDiag(&resp.Diagnostics)
		default:
			resp.Diagnostics.AddError("Unable to create status page", err.Error())
		}
		return
	}

	state, diags := modelFromStatusPage(ctx, out, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *statusPageResource) Read(ctx context.Context, req resource.ReadRequest, resp *resource.ReadResponse) {
	var state statusPageResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	// Read — a deleted page must be removed from state, not error the plan.
	// The endpoint is project-scoped, so a page that now belongs to another
	// account (impossible without a delete first, since slugs are released on
	// delete) is also a 404.
	out, err := r.client.GetStatusPage(ctx, state.ID.ValueString())
	if err != nil {
		if client.IsNotFound(err) {
			resp.State.RemoveResource(ctx)
			return
		}
		resp.Diagnostics.AddError("Unable to read status page", err.Error())
		return
	}

	newState, diags := modelFromStatusPage(ctx, out, state)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *statusPageResource) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan, state statusPageResourceModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	ids, diags := statusPageCheckIDs(ctx, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}

	// PATCH is a full replacement and requires slug, unlike POST. A slug change
	// would have forced a replacement, so the planned value is the stored one.
	out, err := r.client.UpdateStatusPage(ctx, state.ID.ValueString(), client.StatusPage{
		Slug:       plan.Slug.ValueString(),
		Title:      plan.Title.ValueString(),
		Visibility: plan.Visibility.ValueString(),
		CheckIDs:   ids,
	})
	if err != nil {
		switch {
		case client.IsSlugConflict(err):
			// Reachable only through a race: another account can claim this
			// slug between the plan and the apply.
			r.addSlugConflictDiag(&resp.Diagnostics, plan.Slug.ValueString())
		case client.IsPublicPageLimit(err):
			addPublicPageLimitDiag(&resp.Diagnostics)
		default:
			resp.Diagnostics.AddError("Unable to update status page", err.Error())
		}
		return
	}

	newState, diags := modelFromStatusPage(ctx, out, plan)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &newState)...)
}

func (r *statusPageResource) Delete(ctx context.Context, req resource.DeleteRequest, resp *resource.DeleteResponse) {
	var state statusPageResourceModel
	resp.Diagnostics.Append(req.State.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}

	if err := r.client.DeleteStatusPage(ctx, state.ID.ValueString()); err != nil && !client.IsNotFound(err) {
		resp.Diagnostics.AddError("Unable to delete status page", err.Error())
	}
}

// ImportState accepts a UUID or a slug.
//
// SECURITY: the slug branch resolves against the caller's own project list and
// nothing else. Slugs are global, so a slug the caller does not own may exist —
// and confirming that would make `terraform import` an oracle for other
// accounts' page names. A foreign slug and a nonexistent one produce the same
// plain not-found message, deliberately.
func (r *statusPageResource) ImportState(ctx context.Context, req resource.ImportStateRequest, resp *resource.ImportStateResponse) {
	// UUIDs are tried first because the server rejects UUID-shaped slugs, so
	// the two forms cannot be confused. GET by id is project-scoped and already
	// 404s for another account's page.
	if _, err := uuid.Parse(req.ID); err == nil {
		resource.ImportStatePassthroughID(ctx, path.Root("id"), req, resp)
		return
	}

	sp, err := r.client.GetStatusPageBySlug(ctx, req.ID)
	switch {
	case err == nil:
		resource.ImportStatePassthroughID(ctx, path.Root("id"),
			resource.ImportStateRequest{ID: sp.ID}, resp)
	case client.IsNotFound(err):
		resp.Diagnostics.AddError("Unable to import status page",
			fmt.Sprintf("No status page found with slug or ID %q in this project.", req.ID))
	default:
		// The lookup itself failed (network, 5xx, bad credentials). Reporting
		// that as "no such page" would send the operator hunting for a missing
		// resource instead of a broken backend.
		resp.Diagnostics.AddError("Unable to look up status page for import",
			fmt.Sprintf("Listing status pages to resolve %q failed: %s", req.ID, err))
	}
}
