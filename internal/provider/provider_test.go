package provider

import (
	"context"
	"testing"

	fwprovider "github.com/hashicorp/terraform-plugin-framework/provider"
	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/stretchr/testify/require"
)

// testAccProtoV6ProviderFactories is used by every acceptance test in this package.
var testAccProtoV6ProviderFactories = map[string]func() (tfprotov6.ProviderServer, error){
	"lastping": providerserver.NewProtocol6WithError(New("test")()),
}

func TestProvider_Schema(t *testing.T) {
	p := New("test")()
	resp := &fwprovider.SchemaResponse{}
	p.Schema(context.Background(), fwprovider.SchemaRequest{}, resp)

	require.False(t, resp.Diagnostics.HasError(), "%v", resp.Diagnostics)

	endpoint, ok := resp.Schema.Attributes["endpoint"]
	require.True(t, ok, "provider must expose an endpoint attribute")
	require.True(t, endpoint.IsOptional())

	apiKey, ok := resp.Schema.Attributes["api_key"]
	require.True(t, ok, "provider must expose an api_key attribute")
	require.True(t, apiKey.IsOptional(), "api_key is optional so LASTPING_API_KEY can supply it")
	require.True(t, apiKey.IsSensitive(), "api_key must be marked sensitive")
}

func TestProvider_Metadata(t *testing.T) {
	p := New("test")()
	resp := &fwprovider.MetadataResponse{}
	p.Metadata(context.Background(), fwprovider.MetadataRequest{}, resp)
	require.Equal(t, "lastping", resp.TypeName)
}
