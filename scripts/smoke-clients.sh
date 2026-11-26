#!/bin/sh
# The smoke check for a bump of the agent CLI pins (CLR-261, decision
# 2026-11-26-client-smoke-check): per client, short real turns through the
# runner's own code with the pinned binaries. It prints one PASS, FAIL or
# SKIP line per check per client and exits non-zero on any fail. The checks
# and the keys each client needs are in internal/client/smoke_test.go; a
# client without its key is skipped.
#
#   ANTHROPIC_API_KEY=... GEMINI_API_KEY=... scripts/smoke-clients.sh
#   scripts/smoke-clients.sh claude gemini   # only these clients
#
# Needs Docker and the network, and costs a few cents per client. The whole
# output, agent output included, stays in LOG. No client run is a fail.
set -u
cd "$(dirname "$0")/.."
LOG=${LOG:-${TMPDIR:-/tmp}/smoke-clients.log}
run=TestSmokeClients
[ $# -eq 0 ] || run="$run/^($(echo "$*" | tr ' ' '|'))\$"
go test -tags smoke -count=1 -timeout 45m -run "$run" -v ./internal/client/ >"$LOG" 2>&1
status=$?
grep -E '^(PASS|FAIL|SKIP) ' "$LOG"
if [ $status -eq 0 ] && ! grep -q '^PASS ' "$LOG"; then
	echo "no client ran: set a client's key" >&2
	status=1
fi
if [ $status -eq 0 ]; then
	echo "smoke check passed"
else
	echo "smoke check FAILED (exit $status): see $LOG" >&2
fi
exit $status
