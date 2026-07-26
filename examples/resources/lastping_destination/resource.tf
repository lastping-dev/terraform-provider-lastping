# Every credential attribute below is write-only: the LastPing API stores it but
# never returns it. Terraform keeps the configured value in state and refreshes
# only the non-secret fields, so a secret in state is the value you wrote — not
# a value read back from the server. Feed them in from a variable or a secret
# manager rather than committing them.

# Slack, Discord, Microsoft Teams and Google Chat all take a single incoming
# webhook URL, which is itself the credential.
resource "lastping_destination" "oncall_slack" {
  kind        = "slack"
  name        = "#oncall"
  webhook_url = var.slack_webhook_url
}

# Email is the one kind that needs confirming: creating it sends a verification
# link to the address, and `verified` stays false until someone clicks it. No
# alerts are delivered before then. Changing `address` later re-sends the link
# and clears `verified`; changing only `name` does not.
resource "lastping_destination" "ops_email" {
  kind    = "email"
  name    = "Ops mailing list"
  address = "ops@example.com"
}

# A custom webhook: `url` is the endpoint, `secret` signs the payload so your
# receiver can verify the delivery really came from LastPing.
resource "lastping_destination" "internal_hook" {
  kind   = "webhook"
  name   = "Internal alert bus"
  url    = "https://alerts.internal.example.com/lastping"
  secret = var.webhook_signing_secret
}

resource "lastping_destination" "phone" {
  kind      = "telegram"
  name      = "Telegram on-call"
  bot_token = var.telegram_bot_token
  chat_id   = "-1001234567890"
}

# ntfy needs no credential at all — the topic URL is the address.
resource "lastping_destination" "ntfy" {
  kind      = "ntfy"
  name      = "ntfy alerts"
  topic_url = "https://ntfy.sh/my-lastping-alerts"
}

resource "lastping_destination" "pushover" {
  kind     = "pushover"
  name     = "Pushover"
  token    = var.pushover_app_token
  user_key = var.pushover_user_key
}

variable "slack_webhook_url" {
  type      = string
  sensitive = true
}

variable "webhook_signing_secret" {
  type      = string
  sensitive = true
}

variable "telegram_bot_token" {
  type      = string
  sensitive = true
}

variable "pushover_app_token" {
  type      = string
  sensitive = true
}

variable "pushover_user_key" {
  type      = string
  sensitive = true
}
