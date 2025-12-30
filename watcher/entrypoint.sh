#!/bin/bash
set -e

echo "=== Clopus Watcher Starting ==="
echo "Target namespace: $TARGET_NAMESPACE"
echo "Results directory: /tmp/clopus-watcher-runs"

# === WATCHER MODE ===
WATCHER_MODE="${WATCHER_MODE:-autonomous}"
echo "Watcher mode: $WATCHER_MODE"

# === AUTHENTICATION SETUP ===
AUTH_MODE="${AUTH_MODE:-api-key}"
echo "Auth mode: $AUTH_MODE"

if [ "$AUTH_MODE" = "credentials" ]; then
    if [ -f "$HOME/.claude/.credentials.json" ]; then
        echo "Using mounted credentials.json"
    elif [ -f /secrets/credentials.json ]; then
        echo "Copying credentials from /secrets/"
        mkdir -p "$HOME/.claude"
        cp /secrets/credentials.json "$HOME/.claude/.credentials.json"
    else
        echo "ERROR: AUTH_MODE=credentials but no credentials.json found"
        exit 1
    fi
elif [ "$AUTH_MODE" = "api-key" ]; then
    if [ -z "$ANTHROPIC_API_KEY" ]; then
        echo "ERROR: AUTH_MODE=api-key but ANTHROPIC_API_KEY not set"
        exit 1
    fi
    echo "Using API key authentication"
else
    echo "ERROR: Invalid AUTH_MODE: $AUTH_MODE (use 'api-key' or 'credentials')"
    exit 1
fi

# === GENERATE RUN ID ===
# For local development, generate a simple run ID (timestamp-based)
# In production with psql, this would be database-generated
RUN_ID=$(date +%s)
echo "Created run #$RUN_ID ($(date -Iseconds))"

# === GET LAST RUN TIME ===
# For local development, check if we have a previous run record file
RESULTS_DIR="${RESULTS_DIR:-/tmp/clopus-watcher-runs}"
mkdir -p "$RESULTS_DIR"

LAST_RUN_FILE=$(ls -t "$RESULTS_DIR"/*.json 2>/dev/null | head -1)
if [ -n "$LAST_RUN_FILE" ]; then
    LAST_RUN_TIME=$(stat -f %Sm -t %Y-%m-%dT%H:%M:%SZ "$LAST_RUN_FILE" 2>/dev/null || stat -c %y "$LAST_RUN_FILE" 2>/dev/null | cut -d' ' -f1-2 | tr ' ' T)
    echo "Last run time: $LAST_RUN_TIME"
else
    LAST_RUN_TIME=""
    echo "Last run time: (first run)"
fi

# === SELECT PROMPT ===
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [ "$WATCHER_MODE" = "report" ]; then
    PROMPT_FILE="$SCRIPT_DIR/master-prompt-report.md"
else
    PROMPT_FILE="$SCRIPT_DIR/master-prompt-autonomous.md"
fi

if [ ! -f "$PROMPT_FILE" ]; then
    echo "ERROR: Prompt file not found: $PROMPT_FILE"
    cat > "$RESULTS_DIR/run_${RUN_ID}.json" <<EOF
{
  "id": $RUN_ID,
  "started_at": "$(date -Iseconds)",
  "ended_at": "$(date -Iseconds)",
  "namespace": "$TARGET_NAMESPACE",
  "mode": "$WATCHER_MODE",
  "status": "failed",
  "pod_count": 0,
  "error_count": 0,
  "fix_count": 0,
  "report": "Prompt file not found",
  "log": "ERROR: Prompt file not found at $PROMPT_FILE"
}
EOF
    exit 1
fi

PROMPT=$(cat "$PROMPT_FILE")

# Replace environment variables in prompt
PROMPT=$(echo "$PROMPT" | sed "s|\$TARGET_NAMESPACE|$TARGET_NAMESPACE|g")
PROMPT=$(echo "$PROMPT" | sed "s|\$DATABASE_URL|$DATABASE_URL|g")
PROMPT=$(echo "$PROMPT" | sed "s|\$RUN_ID|$RUN_ID|g")
PROMPT=$(echo "$PROMPT" | sed "s|\$LAST_RUN_TIME|$LAST_RUN_TIME|g")

# === RUN CLAUDE ===
echo "Starting Claude Code..."

# Use results directory for logs
LOG_FILE="$RESULTS_DIR/run_${RUN_ID}.log"
echo "=== Run #$RUN_ID started at $(date -Iseconds) ===" > "$LOG_FILE"
echo "Mode: $WATCHER_MODE | Namespace: $TARGET_NAMESPACE" >> "$LOG_FILE"
echo "----------------------------------------" >> "$LOG_FILE"

# Capture output
OUTPUT_FILE="/tmp/claude_output_$RUN_ID.txt"
claude --dangerously-skip-permissions --verbose -p "$PROMPT" 2>&1 | tee -a "$LOG_FILE" | tee "$OUTPUT_FILE"

echo "=== Run #$RUN_ID Complete ===" | tee -a "$LOG_FILE"

# === PARSE REPORT ===
REPORT=""
if grep -q "===REPORT_START===" "$OUTPUT_FILE" 2>/dev/null; then
    REPORT=$(sed -n '/===REPORT_START===/,/===REPORT_END===/p' "$OUTPUT_FILE" | grep -v "===REPORT" | tr -d '\n' | tr -s ' ')
    echo "Parsed report: $REPORT"
fi

# Extract values from report with defaults
POD_COUNT=0
ERROR_COUNT=0
FIX_COUNT=0
STATUS="ok"

if [ -n "$REPORT" ]; then
    # Parse pod_count
    PARSED=$(echo "$REPORT" | grep -o '"pod_count"[[:space:]]*:[[:space:]]*[0-9]*' | grep -o '[0-9]*$')
    [ -n "$PARSED" ] && POD_COUNT=$PARSED

    # Parse error_count
    PARSED=$(echo "$REPORT" | grep -o '"error_count"[[:space:]]*:[[:space:]]*[0-9]*' | grep -o '[0-9]*$')
    [ -n "$PARSED" ] && ERROR_COUNT=$PARSED

    # Parse fix_count
    PARSED=$(echo "$REPORT" | grep -o '"fix_count"[[:space:]]*:[[:space:]]*[0-9]*' | grep -o '[0-9]*$')
    [ -n "$PARSED" ] && FIX_COUNT=$PARSED

    # Parse status
    PARSED=$(echo "$REPORT" | grep -o '"status"[[:space:]]*:[[:space:]]*"[^"]*"' | sed 's/.*"\([^"]*\)"$/\1/')
    [ -n "$PARSED" ] && STATUS=$PARSED
fi

# Validate status is one of expected values
case "$STATUS" in
    ok|fixed|failed|issues_found|running) ;;
    *) STATUS="ok" ;;
esac

echo "Final values: pods=$POD_COUNT errors=$ERROR_COUNT fixes=$FIX_COUNT status=$STATUS"

# Read full log (limit size to prevent issues)
FULL_LOG=$(head -c 100000 "$LOG_FILE")

# === SAVE RUN RESULT TO FILE ===
# For local development, save as JSON file
# These results will be periodically imported to the database by the dashboard
cat > "$RESULTS_DIR/run_${RUN_ID}.json" <<EOF
{
  "id": $RUN_ID,
  "started_at": "$(date -d @$RUN_ID -Iseconds 2>/dev/null || date -Iseconds)",
  "ended_at": "$(date -Iseconds)",
  "namespace": "$TARGET_NAMESPACE",
  "mode": "$WATCHER_MODE",
  "status": "$STATUS",
  "pod_count": $POD_COUNT,
  "error_count": $ERROR_COUNT,
  "fix_count": $FIX_COUNT,
  "report": "$(echo "$REPORT" | sed 's/"/\\"/g')",
  "log": "$(echo "$FULL_LOG" | sed 's/"/\\"/g' | head -c 50000)"
}
EOF

echo "Run #$RUN_ID completed with status: $STATUS"
echo "Result saved to: $RESULTS_DIR/run_${RUN_ID}.json"

# Cleanup
rm -f "$OUTPUT_FILE"
