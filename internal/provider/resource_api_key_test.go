package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
	"github.com/hashicorp/terraform-plugin-testing/terraform"

	"github.com/lastping-dev/terraform-provider-lastping/internal/client"
)

// testAccClientWithKey builds a client authenticating as the given plaintext
// key, so a test can prove a minted key actually works — the only check that
// demonstrates the stored hash matches what the auth middleware computes.
//
// The key is never logged: it goes into the client and nowhere else.
func testAccClientWithKey(key string) *client.Client {
	endpoint := os.Getenv("LASTPING_ENDPOINT")
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	return client.New(endpoint, key, "acc-test")
}

// testAccKeyAuthenticates reports whether the key can make an authenticated
// call. Errors deliberately carry no key material.
func testAccKeyAuthenticates(key string) error {
	if _, err := testAccClientWithKey(key).ListAPIKeys(context.Background()); err != nil {
		return fmt.Errorf("minted key could not authenticate: %w", err)
	}
	return nil
}

// testAccKeyIsRejected is the inverse, and it is specifically a 401 — a network
// failure or a 500 would prove nothing about revocation.
func testAccKeyIsRejected(key string) error {
	_, err := testAccClientWithKey(key).ListAPIKeys(context.Background())
	if err == nil {
		return errors.New("revoked key still authenticates")
	}
	var p *client.Problem
	if !errors.As(err, &p) || p.Status != http.StatusUnauthorized {
		return fmt.Errorf("expected 401 for a revoked key, got: %w", err)
	}
	return nil
}

// TestAccAPIKey_lifecycle is the managed resource's core contract: the server
// mints a key, the plaintext comes back exactly once and lands in state, that
// plaintext really authenticates, a rename replaces the key (the API has no
// update path), and destroying it revokes it.
func TestAccAPIKey_lifecycle(t *testing.T) {
	// Captured so the post-destroy check can prove revocation. Never printed.
	var first, second string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		CheckDestroy: func(*terraform.State) error {
			// Destroy revoked the key: presenting it now must 401.
			return testAccKeyIsRejected(second)
		},
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_api_key" "k" {
  name = "acc-managed-key"
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_api_key.k", "name", "acc-managed-key"),
					resource.TestCheckResourceAttrSet("lastping_api_key.k", "id"),
					resource.TestCheckResourceAttrSet("lastping_api_key.k", "created_at"),
					// expires_at was not configured, so it must read back absent
					// rather than as an empty string.
					resource.TestCheckNoResourceAttr("lastping_api_key.k", "expires_at"),
					// Nothing has authenticated with this key yet — Create mints it
					// but never uses it — so last_used_at must read back absent too.
					resource.TestCheckNoResourceAttr("lastping_api_key.k", "last_used_at"),
					resource.TestMatchResourceAttr("lastping_api_key.k", "prefix",
						regexp.MustCompile(`^lp_.{7}$`)),
					resource.TestCheckResourceAttrWith("lastping_api_key.k", "key", func(key string) error {
						if !strings.HasPrefix(key, "lp_") {
							return errors.New("key does not have the lp_ prefix")
						}
						first = key
						// The point of the whole resource: this must work.
						return testAccKeyAuthenticates(key)
					}),
					// prefix is the non-secret handle for the same key.
					resource.TestCheckResourceAttrWith("lastping_api_key.k", "prefix", func(prefix string) error {
						if !strings.HasPrefix(first, prefix) {
							return errors.New("prefix is not a prefix of the minted key")
						}
						return nil
					}),
				),
			},
			{
				// A refresh must not clobber the plaintext: it is unreadable, so
				// state is the only copy and Read has to leave it alone.
				RefreshState: true,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttrWith("lastping_api_key.k", "key", func(key string) error {
						if key != first {
							return errors.New("refresh changed the stored key")
						}
						return testAccKeyAuthenticates(key)
					}),
					// The previous step's own "key" check already authenticated with
					// this key once, so by this refresh the server has recorded a use
					// and last_used_at must have flipped from absent to set.
					resource.TestCheckResourceAttrSet("lastping_api_key.k", "last_used_at"),
				),
			},
			{
				// Renaming replaces: the API cannot rename a key.
				Config: `
resource "lastping_api_key" "k" {
  name = "acc-managed-key-renamed"
}`,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("lastping_api_key.k", plancheck.ResourceActionReplace),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_api_key.k", "name", "acc-managed-key-renamed"),
					resource.TestCheckResourceAttrWith("lastping_api_key.k", "key", func(key string) error {
						if key == first {
							return errors.New("replacement returned the same key")
						}
						second = key
						if err := testAccKeyAuthenticates(key); err != nil {
							return err
						}
						// The replaced key is gone, which is the part that bites
						// anyone rotating without create_before_destroy.
						return testAccKeyIsRejected(first)
					}),
				),
			},
		},
	})
}

// TestAccAPIKey_expiresAt covers the server-side safety net: a key with an
// expiry stops working on its own. The configured RFC 3339 spelling must survive
// the round trip even though the API normalises to UTC, or every subsequent plan
// would propose a replacement — and for this resource a replacement invalidates
// a live credential.
func TestAccAPIKey_expiresAt(t *testing.T) {
	// A non-UTC offset, deliberately: the API answers in UTC, so this is the
	// case where a naive implementation reports permanent drift.
	expiry := time.Now().Add(48 * time.Hour).
		In(time.FixedZone("UTC+2", 2*60*60)).Format(time.RFC3339)
	config := fmt.Sprintf(`
resource "lastping_api_key" "exp" {
  name       = "acc-expiring-key"
  expires_at = %q
}`, expiry)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_api_key.exp", "expires_at", expiry),
					// The server really stored it, in its own UTC spelling.
					resource.TestCheckResourceAttrWith("lastping_api_key.exp", "id", func(id string) error {
						key, err := testAccDirectClient(t).GetAPIKey(context.Background(), id)
						if err != nil {
							return err
						}
						if key.ExpiresAt == nil {
							return errors.New("server stored no expires_at")
						}
						want, err := time.Parse(time.RFC3339, expiry)
						if err != nil {
							return err
						}
						if !key.ExpiresAt.Equal(want) {
							return fmt.Errorf("server stored %s, configured %s",
								key.ExpiresAt.Format(time.RFC3339), expiry)
						}
						return nil
					}),
				),
			},
			{
				// The offset spelling must not read back as a diff.
				Config:   config,
				PlanOnly: true,
			},
			{
				// Changing the expiry replaces: the API cannot move it.
				Config: fmt.Sprintf(`
resource "lastping_api_key" "exp" {
  name       = "acc-expiring-key"
  expires_at = %q
}`, time.Now().Add(96*time.Hour).UTC().Format(time.RFC3339)),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("lastping_api_key.exp", plancheck.ResourceActionReplace),
					},
				},
			},
		},
	})
}

// TestAccAPIKey_revokedOutOfBand: a key revoked in the dashboard must drop out
// of state so the next apply mints a replacement, rather than failing the plan
// with a 404 on refresh.
func TestAccAPIKey_revokedOutOfBand(t *testing.T) {
	const config = `
resource "lastping_api_key" "gone" {
  name = "acc-key-gone"
}`

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.TestCheckResourceAttrWith("lastping_api_key.gone", "id", func(id string) error {
					return testAccDirectClient(t).RevokeAPIKey(context.Background(), id)
				}),
				ExpectNonEmptyPlan: true,
			},
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("lastping_api_key.gone", plancheck.ResourceActionCreate),
					},
				},
				Check: resource.TestCheckResourceAttrSet("lastping_api_key.gone", "key"),
			},
		},
	})
}

// TestAccAPIKey_invalidConfig: both expiry mistakes are caught at plan time,
// where they name the attribute, rather than as a 400 partway through an apply.
func TestAccAPIKey_invalidConfig(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_api_key" "bad" {
  name       = "acc-bad-key"
  expires_at = "next tuesday"
}`,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`(?s)Invalid timestamp`),
			},
			{
				Config: `
resource "lastping_api_key" "bad" {
  name       = "acc-bad-key"
  expires_at = "2020-01-01T00:00:00Z"
}`,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`(?s)Expiry is not in the future`),
			},
		},
	})
}
