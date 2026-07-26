package provider

import (
	"context"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/types"
	"github.com/stretchr/testify/require"

	"github.com/lastping-dev/terraform-provider-lastping/internal/client"
)

// TestStatusPageCheckIDsValue covers the null-versus-empty round trip. The API
// always answers with an array, so "no monitors" has to be mapped back onto the
// shape the configuration used, or the applied state does not match the plan.
//
// The out-of-band case is the one with teeth: when prior is non-empty and the
// API reports none, the monitors really were removed elsewhere. Reusing prior
// there — the obvious "preserve what we had" shortcut — would mask that removal
// forever.
func TestStatusPageCheckIDsValue(t *testing.T) {
	ctx := context.Background()
	empty, diags := types.ListValueFrom(ctx, types.StringType, []string{})
	require.False(t, diags.HasError(), "%v", diags)
	populated, diags := types.ListValueFrom(ctx, types.StringType, []string{"a", "b"})
	require.False(t, diags.HasError(), "%v", diags)

	t.Run("absent stays absent", func(t *testing.T) {
		got, d := checkIDsValue(ctx, nil, types.ListNull(types.StringType))
		require.False(t, d.HasError(), "%v", d)
		require.True(t, got.IsNull())
	})

	t.Run("explicit empty stays empty", func(t *testing.T) {
		got, d := checkIDsValue(ctx, []string{}, empty)
		require.False(t, d.HasError(), "%v", d)
		require.False(t, got.IsNull())
		require.Empty(t, got.Elements())
	})

	t.Run("out-of-band removal surfaces instead of being masked", func(t *testing.T) {
		got, d := checkIDsValue(ctx, []string{}, populated)
		require.False(t, d.HasError(), "%v", d)
		require.True(t, got.IsNull(), "a non-empty prior must not be echoed back over an empty API answer")
	})

	t.Run("order is preserved", func(t *testing.T) {
		got, d := checkIDsValue(ctx, []string{"b", "a"}, populated)
		require.False(t, d.HasError(), "%v", d)
		var ids []string
		require.False(t, got.ElementsAs(ctx, &ids, false).HasError())
		require.Equal(t, []string{"b", "a"}, ids)
	})
}

// TestStatusPagePublicURLIsEmptyForPrivate: the API returns "" for a private
// page rather than omitting the field, and that is a real value ("not
// published"), not an absent one.
func TestStatusPagePublicURLIsEmptyForPrivate(t *testing.T) {
	ctx := context.Background()
	prior := statusPageResourceModel{CheckIDs: types.ListNull(types.StringType)}

	private, diags := modelFromStatusPage(ctx, &client.StatusPage{
		ID: "id", Slug: "s", Title: "t", Visibility: "private",
	}, prior)
	require.False(t, diags.HasError(), "%v", diags)
	require.False(t, private.PublicURL.IsNull())
	require.Equal(t, "", private.PublicURL.ValueString())

	public, diags := modelFromStatusPage(ctx, &client.StatusPage{
		ID: "id", Slug: "s", Title: "t", Visibility: "public",
		PublicURL: "https://app.lastping.dev/status/s",
	}, prior)
	require.False(t, diags.HasError(), "%v", diags)
	require.Equal(t, "https://app.lastping.dev/status/s", public.PublicURL.ValueString())
}

// TestStatusPageSlugConflictDiagnosticIsDistinct is the point of the whole
// cross-tenant story: the two 4xx failures a public page can hit on create mean
// opposite things, and a caller told the wrong one debugs entirely the wrong
// problem. So the slug diagnostic must name the global namespace, and the
// free-tier diagnostic must not mention slugs at all.
func TestStatusPageSlugConflictDiagnosticIsDistinct(t *testing.T) {
	r := &statusPageResource{client: client.New("https://app.lastping.dev", "k", "test")}

	var slugDiags diag.Diagnostics
	r.addSlugConflictDiag(&slugDiags, "platform")
	require.True(t, slugDiags.HasError())
	slug := slugDiags.Errors()[0]
	require.Equal(t, "Status page slug already taken", slug.Summary())
	require.Contains(t, slug.Detail(), "GLOBAL across all LastPing accounts")
	require.Contains(t, slug.Detail(), "another")
	require.Contains(t, slug.Detail(), "Choose a different slug")
	require.Contains(t, slug.Detail(), "terraform import lastping_status_page.<name> platform")
	require.Contains(t, slug.Detail(), "https://app.lastping.dev/status/platform")

	var capDiags diag.Diagnostics
	addPublicPageLimitDiag(&capDiags)
	require.True(t, capDiags.HasError())
	limit := capDiags.Errors()[0]
	require.Contains(t, limit.Summary(), "public status page")
	require.NotContains(t, limit.Summary(), "slug")
	// The cap is about the caller's own plan. Advice about slugs here would be
	// actively misleading — the name is available, the plan limit is not.
	require.NotContains(t, limit.Detail(), "GLOBAL")
	require.NotContains(t, limit.Detail(), "different slug")
	require.NotEqual(t, slug.Summary(), limit.Summary())
}
