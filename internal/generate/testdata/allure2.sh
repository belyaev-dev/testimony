#!/bin/sh
set -eu

if [ "$1" != "generate" ]; then
  echo "unexpected command: $*" >&2
  exit 2
fi

RESULTS_DIR="$2"
if [ "$3" != "--clean" ] || [ "$4" != "-o" ]; then
  echo "unexpected args: $*" >&2
  exit 3
fi
OUTPUT_DIR="$5"

if [ ! -d "$RESULTS_DIR/history" ]; then
  echo "missing injected history dir" >&2
  exit 4
fi

mkdir -p "$OUTPUT_DIR/history"
cp -R "$RESULTS_DIR/history/." "$OUTPUT_DIR/history/"
printf '<html>allure2</html>' > "$OUTPUT_DIR/index.html"
