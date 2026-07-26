resource "lastping_monitor" "api" {
  name          = "Public API"
  slug          = "public-api"
  schedule_kind = "simple"
  period_s      = 300
  grace_s       = 120
}

resource "lastping_monitor" "web" {
  name          = "Website"
  slug          = "website"
  schedule_kind = "simple"
  period_s      = 300
  grace_s       = 120
}

# A public page. Its slug is the public URL, and slugs are GLOBAL across every
# LastPing account — "status" or "platform" is almost certainly already someone
# else's. If create fails with a slug conflict, the page may well belong to a
# stranger; pick another name, or import it if it is yours.
#
# create_before_destroy is not optional here. `slug` forces replacement, and
# Terraform destroys before creating by default — which would release the old
# slug into a global namespace before claiming the new one. If the new slug turns
# out to be taken, you end up with neither.
resource "lastping_status_page" "public" {
  slug       = "acme-platform"
  title      = "Acme Platform Status"
  visibility = "public"
  check_ids  = [lastping_monitor.api.id, lastping_monitor.web.id]

  lifecycle {
    create_before_destroy = true
  }
}

# A private page for internal monitors. Omitting `slug` lets the server generate
# a random one, which is the right choice when the URL does not need to be
# memorable: nothing to collide with, and nothing to lose in a rename.
resource "lastping_status_page" "internal" {
  title     = "Internal jobs"
  check_ids = [lastping_monitor.api.id]
}

output "status_page_url" {
  # Empty for a private page: only a public one has an address.
  value = lastping_status_page.public.public_url
}
