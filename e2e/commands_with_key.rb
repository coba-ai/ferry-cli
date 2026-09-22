# frozen_string_literal: true

# How many `commands` rows carry one caller idempotency key (AC62).
#
#   bin/rails runner /path/to/ferry-cli/e2e/commands_with_key.rb <key>
#
# Run in the API repository, from the CLI's e2e suite. It answers one JSON
# object on stdout and nothing else.
#
# == Why it is a count and not an existence check
#
# AC62's property is "one row per key". Existence would pass for two rows, and
# two rows under one key is precisely the defect the idempotency contract exists
# to prevent — the CLI minting a fresh key on resume (mutation M3) produces two
# rows under two keys, but a FERRY that ignored the key would produce two under
# one. The count distinguishes them; `exists?` does not.
#
# == Why it reads with `unscoped` SQL rather than through the model
#
# `commands` is RLS-protected and environment-scoped, and this script has no
# tenant context: it is asking a question about the whole database, deliberately,
# because "exactly one row anywhere carries this key" is a stronger claim than
# "exactly one row in this tenant does". A count taken inside a tenant scope
# could not see a duplicate written under a different environment, which is one
# of the ways a key could be honoured in name only.

require "json"

key = ARGV[0].to_s.strip
abort "usage: bin/rails runner commands_with_key.rb <caller_idempotency_key>" if key.empty?

count = ActiveRecord::Base.connection.select_value(
  ActiveRecord::Base.sanitize_sql_array(
    [ "SELECT count(*) FROM commands WHERE caller_idempotency_key = ?", key ]
  )
).to_i

$stdout.puts JSON.generate(key: key, count: count)
