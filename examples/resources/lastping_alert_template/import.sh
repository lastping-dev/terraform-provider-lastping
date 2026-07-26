# Templates belong to a monitor and have no id of their own, so import by the
# monitor's UUID. Whatever templates it currently has come into state.
terraform import lastping_alert_template.checkout_api 550e8400-e29b-41d4-a716-446655440000
