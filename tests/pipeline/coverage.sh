#!/usr/bin/env bash
# Coverage gate for selftunnel.
#
# Runs the full test suite under the race detector and enforces a minimum
# statement coverage of ./internal/... . Coverage from the end-to-end
# tests is attributed to the internal packages via -coverpkg, which is the
# honest measure for this project (the e2e suite is the main vehicle).
#
# Override the floor with COVERAGE_MIN (e.g. COVERAGE_MIN=65 ./coverage.sh).
set -euo pipefail
cd "$(dirname "$0")/../.."

MIN="${COVERAGE_MIN:-70}"

echo "coverage: running go test -race (this covers the race gate too)"
go test -race -coverpkg=./internal/... -coverprofile=coverage.out ./... >/dev/null

TOTAL="$(go tool cover -func=coverage.out | tail -1 | awk '{print $NF}')"
PCT="${TOTAL%\%}"
rm -f coverage.out

echo "coverage: ${PCT}% of ./internal/... statements (minimum ${MIN}%)"
if ! awk -v p="$PCT" -v m="$MIN" 'BEGIN { exit (p+0 >= m+0) ? 0 : 1 }'; then
  echo "coverage gate FAILED: ${PCT}% is below the ${MIN}% floor" >&2
  exit 1
fi
echo "coverage: gate passed"
