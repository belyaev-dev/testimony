#!/bin/sh
set -eu

if [ "$1" != "generate" ]; then
  echo "unexpected command: $*" >&2
  exit 2
fi

CONFIG_FILE="./allurerc.json"
if [ ! -f "$CONFIG_FILE" ]; then
  echo "missing allurerc.json" >&2
  exit 3
fi

OUTPUT_DIR=$(sed -n 's/.*"output":"\([^"]*\)".*/\1/p' "$CONFIG_FILE")
HISTORY_PATH=$(sed -n 's/.*"historyPath":"\([^"]*\)".*/\1/p' "$CONFIG_FILE")
TARGET_HISTORY="$OUTPUT_DIR/history/history.jsonl"

if [ -z "$OUTPUT_DIR" ]; then
  echo "missing output in config" >&2
  exit 4
fi
if [ -z "$HISTORY_PATH" ]; then
  echo "missing historyPath in config" >&2
  exit 5
fi
if [ ! -f "$HISTORY_PATH" ]; then
  echo "missing history file" >&2
  exit 6
fi

mkdir -p "$OUTPUT_DIR/history"
if [ "$HISTORY_PATH" != "$TARGET_HISTORY" ]; then
  cp "$HISTORY_PATH" "$TARGET_HISTORY"
fi
printf '<html>allure3</html>' > "$OUTPUT_DIR/index.html"
