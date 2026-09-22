# frozen_string_literal: true

# Bootstrap for the CLI end-to-end suite (PLAN §5.14, AC61–AC63).
#
# This file is Ruby living in the Go repository, which needs a word of
# explanation. A400 split the CLI out of the API repository, and the app these
# tests drive stayed behind. The e2e suite is the CLI's, so its bootstrap is
# the CLI's too; it is handed to the *API* repository's Rails to run:
#
#   cd /path/to/ferry
#   RAILS_ENV=test FERRY_OMS_BACKEND=fixture \
#     bin/rails runner /path/to/ferry-cli/e2e/bootstrap.rb
#
# It creates the smallest organization a money command can run in — a user, an
# organization with both environments and their policy rows, and a personal
# access token — and prints one JSON object on stdout for the Go side to read.
# Nothing else goes to stdout, because the Go side parses all of it.
#
# == The two refusals
#
# PLAN §5.14: this script refuses `Rails.env.production?` and any
# FERRY_OMS_BACKEND other than fixture/mock. Both are here rather than in the
# workflow because a workflow guard protects the workflow, and the thing worth
# protecting is an operator with a shell. It mints a credential and provisions
# an organization; against a production database with a live OMS attached that
# is not a test fixture, it is an incident.
#
# The backend check reads ENV rather than `Ferry::Oms.gateway`: the gateway is
# resolved per call and not memoised, so asking it here would answer for this
# process and say nothing about the server the CLI will actually talk to. ENV
# is what both processes read, so ENV is what is asserted.

require "json"
require "open3"
require "securerandom"

module CliE2EBootstrap
  # What the PAT carries, and the one scope it cannot.
  #
  # `money:execute` is in `Ferry::Scopes::DANGEROUS`, and the step-up closure
  # (§5.4) refuses it on a personal access token outright — `ferry:pat:issue`
  # aborts with STEP_UP_REQUIRED, not with a validation error. So a PAT is
  # `read keys:manage money:simulate`, and the `money:execute` the happy path
  # needs arrives on the sandbox API key the PAT mints, which is the
  # control-plane split working as designed rather than a limitation.
  PAT_SCOPES = "read keys:manage money:simulate"

  # The scopes AC61 mints an API key with, as the CLI passes them.
  API_KEY_SCOPES = "read,money:simulate,money:execute"

  PAT_LIFETIME_DAYS = 1

  # Devise's :validatable floor is six characters. This password authenticates
  # nothing the CLI touches — there is no session login in this slice — so it
  # exists only to satisfy the model.
  PASSWORD = "cli-e2e-bootstrap-password"

  module_function

  def run
    refuse_production!
    refuse_a_real_oms!

    suffix = ENV.fetch("FERRY_E2E_SUFFIX") { SecureRandom.hex(4) }

    email = "cli-e2e-#{suffix}@example.com"
    slug = "cli-e2e-#{suffix}"

    user = User.create!(email: email, password: PASSWORD)
    organization = Organizations::Provision.call(name: "CLI E2E #{suffix}", slug: slug, owner: user)

    $stdout.puts JSON.generate(
      suffix: suffix,
      email: email,
      organization_slug: slug,
      organization_id: organization.id,
      user_id: user.id,
      pat: issue_pat(email, slug),
      api_key_scopes: API_KEY_SCOPES
    )
  end

  def refuse_production!
    return unless Rails.env.production?

    abort "bootstrap.rb refuses RAILS_ENV=production: it provisions an organization and mints a " \
          "credential, and does so with no credential to authenticate with."
  end

  def refuse_a_real_oms!
    backend = ENV["FERRY_OMS_BACKEND"].to_s.strip

    return if %w[fixture mock].include?(backend)

    abort "bootstrap.rb refuses FERRY_OMS_BACKEND=#{backend.inspect}: the e2e suite executes transfers, " \
          "and only the fixture and mock backends answer without reaching a real OMS. Set " \
          "FERRY_OMS_BACKEND=fixture."
  end

  # The PAT comes from the rake task and not from `PersonalAccessTokens::Issue`
  # directly, for the reason `FerryPatTask.assert_invoked_from_the_command_line!`
  # exists: the task refuses to mint from anything but a command line, and
  # calling the service from here would bypass a control the API repository put
  # there on purpose. So this shells out, and the guard stays live.
  # The scope list carries no quotes around it. `docs/runbooks/credentials.md`
  # writes `'read keys:manage'` because a shell is typing it and the quotes stop
  # the shell splitting on the space; rake itself splits a task's arguments on
  # commas only and hands the rest through verbatim, so a quote that survived
  # the shell arrives inside the string and `Credential#scopes_are_permitted`
  # rejects `'read` as an unknown scope. Open3 passes argv directly with no
  # shell in between, so there is nothing to quote against.
  def issue_pat(email, slug)
    task = "ferry:pat:issue[#{email},#{slug},cli-e2e,#{PAT_SCOPES},#{PAT_LIFETIME_DAYS}]"

    out, err, status = Open3.capture3("bin/rails", task, chdir: Rails.root.to_s)

    unless status.success?
      abort "ferry:pat:issue failed (#{status.exitstatus}):\n#{err}#{out}"
    end

    # `report_issued` prints the token on a line of its own, and PAT_PATTERN in
    # lib/ferry/tokens.rb is what the API itself matches. Anchored to the whole
    # line so a token quoted inside a sentence cannot be picked up instead.
    token = out.lines.map(&:strip).find { |line| line.match?(/\Aferry_pat_[0-9A-Za-z]{43}\z/) }

    abort "ferry:pat:issue printed no token:\n#{out}" if token.nil?

    token
  end
end

CliE2EBootstrap.run
