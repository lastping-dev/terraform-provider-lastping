package provider

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/plancheck"
)

// TestAccDestination_emailRenameIsUpdate is the reason this resource exists in
// its current shape: PATCH /api/v1/channels/{id} means a rename is an in-place
// update, not a destroy-and-recreate. For an email destination a recreate would
// also invalidate the recipient's confirmation link, so the plan action is
// asserted explicitly rather than inferred from the id surviving.
func TestAccDestination_emailRenameIsUpdate(t *testing.T) {
	var createdID string

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_destination" "email" {
  kind    = "email"
  name    = "acc-email"
  address = "acc@example.com"
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_destination.email", "kind", "email"),
					resource.TestCheckResourceAttr("lastping_destination.email", "name", "acc-email"),
					// target is the API's non-secret hint; for email it is the address.
					resource.TestCheckResourceAttr("lastping_destination.email", "target", "acc@example.com"),
					// Email destinations start unverified until the recipient confirms.
					resource.TestCheckResourceAttr("lastping_destination.email", "verified", "false"),
					resource.TestCheckResourceAttr("lastping_destination.email", "disabled", "false"),
					resource.TestCheckNoResourceAttr("lastping_destination.email", "disable_reason"),
					resource.TestCheckResourceAttrSet("lastping_destination.email", "created_at"),
					resource.TestCheckResourceAttrWith("lastping_destination.email", "id", func(v string) error {
						createdID = v
						return nil
					}),
				),
			},
			{
				// Rename only. This must be an Update, never a Replace.
				Config: `
resource "lastping_destination" "email" {
  kind    = "email"
  name    = "acc-renamed"
  address = "acc@example.com"
}`,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("lastping_destination.email", plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_destination.email", "name", "acc-renamed"),
					resource.TestCheckResourceAttr("lastping_destination.email", "address", "acc@example.com"),
					resource.TestCheckResourceAttrWith("lastping_destination.email", "id", func(v string) error {
						if v != createdID {
							return fmt.Errorf("rename recreated the destination: id was %s, now %s", createdID, v)
						}
						return nil
					}),
				),
			},
			{
				// email carries no credential, so the whole configuration round
				// trips through an import.
				ResourceName:      "lastping_destination.email",
				ImportState:       true,
				ImportStateVerify: true,
			},
			{
				// Import by name, which is what an agent has to hand.
				ResourceName:      "lastping_destination.email",
				ImportState:       true,
				ImportStateId:     "acc-renamed",
				ImportStateVerify: true,
			},
		},
	})
}

// TestAccDestination_emailAddressChangeResetsVerification pins the API's
// behaviour: changing the address re-sends confirmation and clears verified,
// while it stays an in-place update.
func TestAccDestination_emailAddressChangeResetsVerification(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_destination" "email" {
  kind    = "email"
  name    = "acc-email-addr"
  address = "first@example.com"
}`,
				Check: resource.TestCheckResourceAttr("lastping_destination.email", "target", "first@example.com"),
			},
			{
				Config: `
resource "lastping_destination" "email" {
  kind    = "email"
  name    = "acc-email-addr"
  address = "second@example.com"
}`,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("lastping_destination.email", plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_destination.email", "address", "second@example.com"),
					resource.TestCheckResourceAttr("lastping_destination.email", "target", "second@example.com"),
					resource.TestCheckResourceAttr("lastping_destination.email", "verified", "false"),
				),
			},
		},
	})
}

// TestAccDestination_slackSecretDoesNotDrift is the H2 guarantee. A Slack
// webhook URL is itself the credential, so the API returns only the fixed label
// "slack webhook" and never the URL. If Read refreshed webhook_url from that
// response it would null the attribute out and every subsequent plan would
// propose rewriting it. The RefreshState step followed by an implicitly-empty
// plan is the assertion.
func TestAccDestination_slackSecretDoesNotDrift(t *testing.T) {
	const hook = "https://hooks.slack.com/services/T00000000/B00000000/acc-token"
	config := fmt.Sprintf(`
resource "lastping_destination" "slack" {
  kind        = "slack"
  name        = "acc-slack"
  webhook_url = %q
}`, hook)

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_destination.slack", "webhook_url", hook),
					// Non-secret hint only — the API refuses to echo the URL.
					resource.TestCheckResourceAttr("lastping_destination.slack", "target", "slack webhook"),
					// Everything except email is verified by construction.
					resource.TestCheckResourceAttr("lastping_destination.slack", "verified", "true"),
					// Attributes for other kinds stay unset.
					resource.TestCheckNoResourceAttr("lastping_destination.slack", "url"),
					resource.TestCheckNoResourceAttr("lastping_destination.slack", "address"),
				),
			},
			{
				// Refresh from the API, then require an empty plan: the secret
				// survived the round trip instead of being nulled out.
				RefreshState: true,
				Check: resource.TestCheckResourceAttr(
					"lastping_destination.slack", "webhook_url", hook),
			},
			{
				Config:   config,
				PlanOnly: true,
			},
			{
				// A rotated credential is an in-place update.
				Config: `
resource "lastping_destination" "slack" {
  kind        = "slack"
  name        = "acc-slack"
  webhook_url = "https://hooks.slack.com/services/T00000000/B00000000/rotated"
}`,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("lastping_destination.slack", plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.TestCheckResourceAttr("lastping_destination.slack", "webhook_url",
					"https://hooks.slack.com/services/T00000000/B00000000/rotated"),
			},
			{
				// Import cannot recover a credential the API never returns, so
				// webhook_url is expected to come back empty.
				ResourceName:            "lastping_destination.slack",
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"webhook_url"},
			},
		},
	})
}

// TestAccDestination_webhook covers the two-attribute secret-bearing kind and
// the one case where the credential lives next to a non-secret url.
func TestAccDestination_webhook(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_destination" "hook" {
  kind   = "webhook"
  name   = "acc-webhook"
  url    = "https://example.com/lastping"
  secret = "s3cr3t-value"
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_destination.hook", "url", "https://example.com/lastping"),
					resource.TestCheckResourceAttr("lastping_destination.hook", "secret", "s3cr3t-value"),
					// url is non-secret, so the API does report it as the target.
					resource.TestCheckResourceAttr("lastping_destination.hook", "target", "https://example.com/lastping"),
				),
			},
			{
				RefreshState: true,
				Check:        resource.TestCheckResourceAttr("lastping_destination.hook", "secret", "s3cr3t-value"),
			},
			{
				ResourceName:            "lastping_destination.hook",
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"secret"},
			},
		},
	})
}

// TestAccDestination_ntfy covers a kind with no credential at all, where the
// whole configuration is recoverable from the API.
func TestAccDestination_ntfy(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_destination" "ntfy" {
  kind      = "ntfy"
  name      = "acc-ntfy"
  topic_url = "https://ntfy.sh/acc-alerts"
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_destination.ntfy", "topic_url", "https://ntfy.sh/acc-alerts"),
					resource.TestCheckResourceAttr("lastping_destination.ntfy", "target", "https://ntfy.sh/acc-alerts"),
					resource.TestCheckResourceAttr("lastping_destination.ntfy", "verified", "true"),
				),
			},
			{
				ResourceName:      "lastping_destination.ntfy",
				ImportState:       true,
				ImportStateVerify: true,
			},
		},
	})
}

// TestAccDestination_ntfyToken covers the optional ntfy bearer token, which is
// what authenticated and self-hosted ntfy servers require. Two things are being
// pinned. First, the token must be accepted at all: `token` is pushover's
// required attribute, so a validator that keys purely off the attribute name
// would reject it here as belonging to another kind. Second — the reason this
// test changes topic_url — the API replaces `config` wholesale on PATCH, so an
// update that rebuilds the payload without the token silently wipes a
// credential the API will never hand back.
func TestAccDestination_ntfyToken(t *testing.T) {
	const token = "tk_acc_ntfy_token"

	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: fmt.Sprintf(`
resource "lastping_destination" "auth_ntfy" {
  kind      = "ntfy"
  name      = "acc-ntfy-auth"
  topic_url = "https://ntfy.example.com/acc-private"
  token     = %q
}`, token),
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_destination.auth_ntfy", "token", token),
					resource.TestCheckResourceAttr("lastping_destination.auth_ntfy", "topic_url",
						"https://ntfy.example.com/acc-private"),
					resource.TestCheckResourceAttr("lastping_destination.auth_ntfy", "target",
						"https://ntfy.example.com/acc-private"),
					resource.TestCheckResourceAttr("lastping_destination.auth_ntfy", "verified", "true"),
				),
			},
			{
				// The API never returns config, so a refresh must not null the
				// token out; and the following plan must be empty.
				RefreshState: true,
				Check:        resource.TestCheckResourceAttr("lastping_destination.auth_ntfy", "token", token),
			},
			{
				Config: fmt.Sprintf(`
resource "lastping_destination" "auth_ntfy" {
  kind      = "ntfy"
  name      = "acc-ntfy-auth"
  topic_url = "https://ntfy.example.com/acc-private"
  token     = %q
}`, token),
				PlanOnly: true,
			},
			{
				// Change only topic_url. The token is not in this diff, and the
				// server-side config replace would drop it unless Update resends
				// it — so its survival here is the assertion.
				Config: fmt.Sprintf(`
resource "lastping_destination" "auth_ntfy" {
  kind      = "ntfy"
  name      = "acc-ntfy-auth"
  topic_url = "https://ntfy.example.com/acc-moved"
  token     = %q
}`, token),
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("lastping_destination.auth_ntfy",
							plancheck.ResourceActionUpdate),
					},
				},
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_destination.auth_ntfy", "topic_url",
						"https://ntfy.example.com/acc-moved"),
					resource.TestCheckResourceAttr("lastping_destination.auth_ntfy", "target",
						"https://ntfy.example.com/acc-moved"),
					resource.TestCheckResourceAttr("lastping_destination.auth_ntfy", "token", token),
				),
			},
			{
				// The token is a credential, so import cannot recover it.
				ResourceName:            "lastping_destination.auth_ntfy",
				ImportState:             true,
				ImportStateVerify:       true,
				ImportStateVerifyIgnore: []string{"token"},
			},
		},
	})
}

// TestAccDestination_telegram covers the kind whose target is a rendered string
// ("chat <id>") rather than the raw attribute, so chat_id has to be recovered
// from it without disturbing the write-only bot_token.
func TestAccDestination_telegram(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_destination" "tg" {
  kind      = "telegram"
  name      = "acc-telegram"
  bot_token = "123456:acc-bot-token"
  chat_id   = "-1001234567890"
}`,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_destination.tg", "chat_id", "-1001234567890"),
					resource.TestCheckResourceAttr("lastping_destination.tg", "bot_token", "123456:acc-bot-token"),
					resource.TestCheckResourceAttr("lastping_destination.tg", "target", "chat -1001234567890"),
				),
			},
			{
				RefreshState: true,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("lastping_destination.tg", "chat_id", "-1001234567890"),
					resource.TestCheckResourceAttr("lastping_destination.tg", "bot_token", "123456:acc-bot-token"),
				),
			},
		},
	})
}

// TestAccDestination_missingRequiredAttribute: a kind's required attributes are
// enforced by ConfigValidators, so the failure lands at plan time naming the
// attribute instead of as a 400 partway through an apply.
func TestAccDestination_missingRequiredAttribute(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_destination" "bad" {
  kind = "slack"
  name = "acc-invalid"
}`,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`kind=slack requires webhook_url`),
			},
			{
				Config: `
resource "lastping_destination" "bad" {
  kind   = "webhook"
  name   = "acc-invalid"
  url    = "https://example.com/lastping"
  secret = ""
}`,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`kind=webhook requires secret`),
			},
			{
				Config: `
resource "lastping_destination" "bad" {
  kind      = "pushover"
  name      = "acc-invalid"
  token     = "app-token"
}`,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`kind=pushover requires user_key`),
			},
		},
	})
}

// TestAccDestination_invalidEmailAddress: the API parses the address with
// net/mail.ParseAddress, so a typo is otherwise a 400 partway through an apply.
// The point of this test is the `PlanOnly` — the error has to land before
// anything is created.
func TestAccDestination_invalidEmailAddress(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_destination" "bad_email" {
  kind    = "email"
  name    = "acc-bad-email"
  address = "ops-at-example.com"
}`,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`(?s)Invalid email address`),
			},
			{
				// A list is a single address as far as the API is concerned, and
				// only the first would ever be delivered to.
				Config: `
resource "lastping_destination" "bad_email" {
  kind    = "email"
  name    = "acc-bad-email"
  address = "a@example.com, b@example.com"
}`,
				PlanOnly:    true,
				ExpectError: regexp.MustCompile(`(?s)Invalid email address`),
			},
		},
	})
}

// TestAccDestination_wrongKindAttribute: an attribute belonging to a different
// kind is silently dropped by the API, which would leave a configuration that
// looks correct and delivers nowhere.
func TestAccDestination_wrongKindAttribute(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{{
			Config: `
resource "lastping_destination" "mixed" {
  kind        = "slack"
  name        = "acc-mixed"
  webhook_url = "https://hooks.slack.com/services/T/B/x"
  address     = "someone@example.com"
}`,
			PlanOnly:    true,
			ExpectError: regexp.MustCompile(`(?s)address.*belongs to a different kind`),
		}},
	})
}

// TestAccDestination_kindChangeReplaces: the API refuses to change kind, so the
// provider must plan a replacement rather than an update that would 400.
func TestAccDestination_kindChangeReplaces(t *testing.T) {
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: `
resource "lastping_destination" "swap" {
  kind      = "ntfy"
  name      = "acc-swap"
  topic_url = "https://ntfy.sh/acc-swap"
}`,
			},
			{
				Config: `
resource "lastping_destination" "swap" {
  kind    = "email"
  name    = "acc-swap"
  address = "swap@example.com"
}`,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("lastping_destination.swap",
							plancheck.ResourceActionDestroyBeforeCreate),
					},
				},
				Check: resource.TestCheckResourceAttr("lastping_destination.swap", "kind", "email"),
			},
		},
	})
}

// TestAccDestination_deletedOutOfBand: a destination removed through the
// dashboard or the API must be recreated on the next apply, not fail the plan.
func TestAccDestination_deletedOutOfBand(t *testing.T) {
	const config = `
resource "lastping_destination" "gone" {
  kind      = "ntfy"
  name      = "acc-gone"
  topic_url = "https://ntfy.sh/acc-gone"
}`
	resource.Test(t, resource.TestCase{
		PreCheck:                 func() { testAccPreCheck(t) },
		ProtoV6ProviderFactories: testAccProtoV6ProviderFactories,
		Steps: []resource.TestStep{
			{
				Config: config,
				Check: resource.TestCheckResourceAttrWith("lastping_destination.gone", "id", func(id string) error {
					// Delete it behind Terraform's back, from within the check so
					// the next step's refresh sees a 404.
					return testAccDirectClient(t).DeleteDestination(t.Context(), id)
				}),
				// The post-step refresh plan is non-empty by construction: the
				// check above just deleted the destination, which is the drift
				// the next step asserts on.
				ExpectNonEmptyPlan: true,
			},
			{
				Config: config,
				ConfigPlanChecks: resource.ConfigPlanChecks{
					PreApply: []plancheck.PlanCheck{
						plancheck.ExpectResourceAction("lastping_destination.gone", plancheck.ResourceActionCreate),
					},
				},
				Check: resource.TestCheckResourceAttr("lastping_destination.gone", "name", "acc-gone"),
			},
		},
	})
}
