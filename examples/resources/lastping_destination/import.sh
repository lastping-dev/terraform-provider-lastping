# Import by UUID:
terraform import lastping_destination.oncall_slack 550e8400-e29b-41d4-a716-446655440000

# Or by name, when exactly one destination in the project has it (an ambiguous
# name is an error, not an arbitrary pick):
terraform import lastping_destination.oncall_slack '#oncall'

# Importing cannot recover credentials — the API never returns them. A destination
# with a secret attribute (secret, bot_token, webhook_url, token, user_key) comes
# back with that attribute empty, and the first apply after the import writes the
# configured value back. That apply is an in-place update, not a replacement.
