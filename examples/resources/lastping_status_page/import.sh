# Import by UUID:
terraform import lastping_status_page.public 550e8400-e29b-41d4-a716-446655440000

# Or by slug, which resolves against your own project only:
terraform import lastping_status_page.public acme-platform

# Slugs are globally unique, but importing one is not a way to find out who holds
# it: a slug owned by another account reports exactly the same "no status page
# found ... in this project" as one that does not exist anywhere.
