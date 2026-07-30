# A route has no id of its own — it is identified by the monitor it belongs to
# and the event it covers — so the import ID carries both, separated by a colon:
terraform import lastping_route.backup_down 550e8400-e29b-41d4-a716-446655440000:down

# The event type must be one of down, recovery, fail or every-run. Anything
# else, or a missing colon, is rejected with the expected shape rather than a
# lookup miss.
