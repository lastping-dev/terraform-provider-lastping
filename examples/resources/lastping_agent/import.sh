# Import by slug (preferred — stable, human-readable, and what the API accepts
# anywhere an agent is referenced):
terraform import lastping_agent.nightly_etl nightly-etl-bot

# Or by UUID:
terraform import lastping_agent.nightly_etl 6ba7b810-9dad-11d1-80b4-00c04fd430c8

# The slug resolves against your own project only. An agent in another project
# reports exactly the same "No agent found with slug or ID ... in this project"
# as one that does not exist anywhere, so import is never an oracle for other
# tenants' agent names.
