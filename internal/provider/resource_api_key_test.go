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

// testAccCreatingKeyExpiry returns the expiry of the key the provider itself
// authenticates with — the parent of every key this suite mints — or nil when
// that key never expires. It is matched by prefix, which is the non-secret
// handle for a key; the plaintext goes nowhere.
//
// It reports failure as an error rather than calling t.Fatal, because its caller
// is a TestCheckFunc: t.Fatal there runs on the test framework's own goroutine
// handling and skips the rest of the check chain, where a returned error is
// reported as the step failure it actually is.
func testAccCreatingKeyExpiry(t *testing.T) (*time.Time, error) {
	t.Helper()
	plaintext := os.Getenv("LASTPING_API_KEY")
	keys, err := testAccDirectClient(t).ListAPIKeys(context.Background())
	if err != nil {
		return nil, fmt.Errorf("listing keys to find the creating key: %w", err)
	}
	for i := range keys {
		if keys[i].Prefix != "" && strings.HasPrefix(plaintext, keys[i].Prefix) {
			return keys[i].ExpiresAt, nil
		}
	}
	return nil, errors.New("the configured LASTPING_API_KEY does not appear in its own project's key list")
}

// testAccCheckExpiryInheritedFromCreator asserts what an OMITTED expires_at may
// read back as, and it is the assertion the monorepo's provider-acceptance job
// pins LASTPING_SEED_KEY_EXPIRY=never to avoid (see .github/workflows/ci.yml
// there): it replaces a flat TestCheckNoResourceAttr, which could only ever hold
// for a permanent creating key.
//
// A key may not outlive the key that minted it, so with an expiring creating key
// the server may hand back its expiry for a config that asked for nothing. Two
// values are therefore legitimate and NOTHING else is:
//
//   - absent — the creating key never expires, or LP_API_KEY_INHERIT_EXPIRY is
//     off on the backend, which is how the flag ships until this behaviour is
//     released;
//   - exactly the creating key's own expiry — inheritance, on.
//
// An arbitrary other timestamp fails, so this does not degrade into "any value
// is fine": the inherited value must be the parent's, to the second.
func testAccCheckExpiryInheritedFromCreator(t *testing.T, name string) resource.TestCheckFunc {
	return func(s *terraform.State) error {
		rs, ok := s.RootModule().Resources[name]
		if !ok {
			return fmt.Errorf("%s not found in state", name)
		}
		got := rs.Primary.Attributes["expires_at"]
		parent, err := testAccCreatingKeyExpiry(t)
		if err != nil {
			return err
		}
		if got == "" {
			if parent == nil {
				return nil
			}
			// Inheritance disabled server-side. Legitimate, and worth saying out
			// loud: the interesting half of this test did not run.
			t.Logf("expires_at absent while the creating key expires at %s: "+
				"LP_API_KEY_INHERIT_EXPIRY is off on this backend",
				parent.UTC().Format(time.RFC3339))
			return nil
		}
		if parent == nil {
			return fmt.Errorf("expires_at is %q for a config that omitted it, "+
				"and the creating key never expires — nothing could have set it", got)
		}
		gotTime, err := time.Parse(time.RFC3339, got)
		if err != nil {
			return fmt.Errorf("expires_at %q is not an RFC 3339 timestamp: %w", got, err)
		}
		// To the second, because that is the precision state can hold: the
		// column keeps microseconds, and the provider writes timestamps with
		// time.RFC3339, which drops the fraction. Comparing instants would fail
		// on a sub-second tail that no plan can ever see.
		if want := parent.UTC().Format(time.RFC3339); gotTime.UTC().Format(time.RFC3339) != want {
			return fmt.Errorf("inherited expires_at is %s, but the creating key expires at %s",
				gotTime.UTC().Format(time.RFC3339), want)
		}
		return nil
	}
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
					// expires_at was not configured, so it reads back absent — or,
					// under an expiring creating key, as that key's own expiry.
					testAccCheckExpiryInheritedFromCreator(t, "lastping_api_key.k"),
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
				// The apply-then-plan property, against the real backend: whatever
				// the server decided about expires_at for a config that omits it
				// must produce an EMPTY plan. Both halves can break this — a
				// server-assigned value read into a non-Computed attribute, and a
				// Computed attribute planning as unknown every run — and either
				// one proposes replacing a live credential on every apply.
				Config: `
resource "lastping_api_key" "k" {
  name = "acc-managed-key"
}`,
				PlanOnly: true,
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

// testAccCheckServerScope asserts what the SERVER stored, not merely what state
// says. State can be right about a value the request never carried — the whole
// failure mode this suite exists to catch — so the scope is read back through a
// second, out-of-band client.
func testAccCheckServerScope(t *testing.T, name, want string) resource.TestCheckFunc {
	t.Helper()
	return resource.TestCheckResourceAttrWith(name, "id", func(id string) error {
		key, err := testAccDirectClient(t).GetAPIKey(context.Background(), id)
		if err != nil {
			return err
		}
		if key.Scope != want {
			return fmt.Errorf("server stored scope %q, configuration asked for %q", key.Scope, want)
		}
		return nil
	})
}

// TestAccAPIKey_scopes covers the tier on a real backend: each of the three
// values is accepted and actually stored, an omitted scope really is `write`,
// and changing the scope replaces the key — because the API has no way to move
// one.
func TestAccAPIKey_scopes(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_api_key" "read" {
  name  = "acc-scope-read"
  scope = "read"
}

resource "lastping_api_key" "write" {
  name  = "acc-scope-write"
  scope = "write"
}

resource "lastping_api_key" "admin" {
  name  = "acc-scope-admin"
  scope = "admin"
}

resource "lastping_api_key" "defaulted" {
  name = "acc-scope-defaulted"
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_api_key.read", "scope", "read"),
					testAccCheckServerScope(t, "lastping_api_key.read", "read"),
					resource.TestCheckResourceAttr("lastping_api_key.write", "scope", "write"),
					testAccCheckServerScope(t, "lastping_api_key.write", "write"),
					resource.TestCheckResourceAttr("lastping_api_key.admin", "scope", "admin"),
					testAccCheckServerScope(t, "lastping_api_key.admin", "admin"),
					// The default is the point of this one: nothing configured,
					// and the key must still come out as write rather than
					// inheriting the creating key's admin.
					resource.TestCheckResourceAttr("lastping_api_key.defaulted", "scope", "write"),
					testAccCheckServerScope(t, "lastping_api_key.defaulted", "write"),
					// Lineage: these keys were minted BY the key the provider is
					// configured with, so every one of them names a parent.
					resource.TestCheckResourceAttrSet("lastping_api_key.defaulted", "created_by_key_id"),
					resource.TestCheckResourceAttrWith("lastping_api_key.read", "created_by_key_id",
						func(parent string) error {
							keys, err := testAccDirectClient(t).ListAPIKeys(context.Background())
							if err != nil {
								return err
							}
							for _, k := range keys {
								if k.ID == parent {
									return nil
								}
							}
							return fmt.Errorf("created_by_key_id %s is not a key in this project", parent)
						}),
				),
			},
			{
				// Nothing configured must not drift: the default has to survive
				// the round trip, or every plan proposes replacing live keys.
				Config: `
resource "lastping_api_key" "read" {
  name  = "acc-scope-read"
  scope = "read"
}

resource "lastping_api_key" "write" {
  name  = "acc-scope-write"
  scope = "write"
}

resource "lastping_api_key" "admin" {
  name  = "acc-scope-admin"
  scope = "admin"
}

resource "lastping_api_key" "defaulted" {
  name = "acc-scope-defaulted"
}`,
				PlanOnly: true,
			},
			{
				// Raising a scope replaces the key. The API cannot change one,
				// so anything else would be a state lie.
				Config: `
resource "lastping_api_key" "read" {
  name  = "acc-scope-read"
  scope = "write"
}

resource "lastping_api_key" "write" {
  name  = "acc-scope-write"
  scope = "write"
}

resource "lastping_api_key" "admin" {
  name  = "acc-scope-admin"
  scope = "admin"
}

resource "lastping_api_key" "defaulted" {
  name = "acc-scope-defaulted"
}`,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("lastping_api_key.read", plancheck.ResourceActionReplace),
					},
				},
				Check: testAccCheckServerScope(t, "lastping_api_key.read", "write"),
			},
			{
				// REMOVING scope from a configuration must propose NOTHING.
				//
				// This is the grandfathered-key hazard, in the only shape an
				// acceptance test can build it: state holds a scope the
				// configuration no longer mentions, exactly as it would for a
				// key minted before the attribute existed (every such key is
				// `admin` on the server, by that migration's grandfathering
				// clause). A default applied on every plan would propose
				// replacing these keys, and replacing an API key revokes it
				// along with every key it ever minted — on a bare provider
				// upgrade, with no user action at all.
				//
				// The plan check is the assertion, and PlanOnly is deliberately
				// NOT set — the two are mutually exclusive in the test
				// framework, and this way the step also runs the framework's
				// own post-apply empty-plan check, so both "the plan proposes
				// nothing" and "it still proposes nothing afterwards" are
				// covered.
				Config: `
resource "lastping_api_key" "read" {
  name = "acc-scope-read"
}

resource "lastping_api_key" "write" {
  name = "acc-scope-write"
}

resource "lastping_api_key" "admin" {
  name = "acc-scope-admin"
}

resource "lastping_api_key" "defaulted" {
  name = "acc-scope-defaulted"
}`,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("lastping_api_key.admin", plancheck.ResourceActionNoop),
						plancheck.ExpectResourceAction("lastping_api_key.read", plancheck.ResourceActionNoop),
					},
				},
			},
		},
	})
}

// TestAccAPIKey_invalidScope: a tier the API does not store is a plan-time
// error naming the attribute, not a 400 partway through an apply.
func TestAccAPIKey_invalidScope(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_api_key" "bad" {
  name  = "acc-bad-scope"
  scope = "owner"
}`,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`(?s)Invalid Attribute Value Match`),
			},
		},
	})
}

// TestAccAPIKey_scopeCapIsEnforcedByTheServer is the half no unit test can
// prove: a Terraform run authenticating with a WRITE key cannot mint an admin
// key, whatever the configuration says.
//
// The write key is minted in-test, by the suite's own admin key, because the
// seeded acceptance key is always admin (the monorepo's scripts/seed-acc-key.sh
// inserts the row without a scope, taking the column default). It is revoked
// again at the end.
//
// TWO refusals are acceptable, and which one arrives depends on a server flag
// this repository does not control:
//
//   - LP_API_KEY_SCOPES_ENFORCE off (today): the write key reaches the endpoint
//     and the create-time scope CAP refuses it — 400, max_scope.
//   - enforcement on: the key never reaches the handler at all, and scoped()
//     refuses it — 403, required_scope.
//
// Both are the provider's scope diagnostics, and pinning only one would make
// this test fail the day the flag flips, which is precisely when it matters.
func TestAccAPIKey_scopeCapIsEnforcedByTheServer(t *testing.T) {
	if os.Getenv("LASTPING_API_KEY") == "" {
		t.Skip("LASTPING_API_KEY not set; skipping acceptance test")
	}

	ctx := context.Background()
	admin := testAccDirectClient(t)
	writer, err := admin.CreateAPIKey(ctx, client.CreateAPIKeyInput{
		Name:  "acc-scope-cap-writer",
		Scope: "write",
	})
	if err != nil {
		t.Fatalf("minting the write key this test authenticates with: %v", err)
	}
	if writer.Scope != "write" {
		t.Fatalf("the key this test relies on has scope %q, not write; the test would prove nothing",
			writer.Scope)
	}
	t.Cleanup(func() {
		if err := admin.RevokeAPIKey(context.Background(), writer.ID); err != nil &&
			!client.IsNotFound(err) {
			t.Logf("could not revoke the test's write key %s: %v", writer.ID, err)
		}
	})

	endpoint := os.Getenv("LASTPING_ENDPOINT")
	if endpoint == "" {
		endpoint = defaultEndpoint
	}

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				// The plaintext appears in this configuration and nowhere else:
				// the apply fails, so nothing reaches state, and the key is
				// revoked in the cleanup above.
				Config: fmt.Sprintf(`
provider "lastping" {
  alias    = "writer"
  endpoint = %q
  api_key  = %q
}

resource "lastping_api_key" "escalate" {
  provider = lastping.writer

  name  = "acc-scope-cap-child"
  scope = "admin"
}`, endpoint, writer.Key),
				ExpectError: regexp.MustCompile(
					`(?s)(exceeds the creating key's own scope|cannot manage API keys)`),
			},
		},
	})
}
