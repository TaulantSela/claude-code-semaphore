#!/bin/bash
# claude-semaphore state hook — writes per-session traffic-light state files
# consumed by the claude-semaphore tray app.
#
# Usage: state.sh <action-hint>
# Claude Code delivers the hook event JSON on stdin. The event name embedded
# in the JSON (hook_event_name) takes precedence over the argv hint, so
# behavior updates apply even to sessions that captured an older hooks.json.
#
# States: red = Claude needs your input, orange = working/idle, green = finished.
#
# Runs on macOS, Linux, and Windows (Git Bash, which Claude Code requires
# there anyway). Deliberately avoids jq — only sed/grep, present everywhere.

STATE_DIR="$HOME/.claude/semaphore"
mkdir -p "$STATE_DIR"

INPUT=$(cat 2>/dev/null)

# Extract a top-level string field from single-line hook JSON. Good enough
# for our fields; not a general JSON parser.
json_field() {
  printf '%s' "$INPUT" | sed -n "s/.*\"$1\"[[:space:]]*:[[:space:]]*\"\([^\"]*\)\".*/\1/p" | head -1
}

SESSION=$(json_field session_id)
[ -n "$SESSION" ] || SESSION="unknown"
FILE="$STATE_DIR/$SESSION"

set_state() { printf '%s\n' "$1" > "$FILE"; }

EVENT=$(json_field hook_event_name)
TOOL=$(json_field tool_name)

# Map to an action: prefer the real event name, fall back to the argv hint.
case "$EVENT" in
  SessionStart|UserPromptSubmit|PermissionDenied|ElicitationResult) ACTION=working ;;
  PreToolUse)        ACTION=pre ;;
  PostToolUse)       ACTION=post ;;
  PermissionRequest) ACTION=permreq ;;
  Notification|Elicitation) ACTION=attention ;;
  Stop)              ACTION=done ;;
  SessionEnd)        ACTION=end ;;
  *)                 ACTION="$1" ;;
esac

case "$ACTION" in
  working)
    set_state orange
    ;;
  pre)
    # AskUserQuestion means Claude is showing a question dialog — that is
    # "waiting on you", not "working". Any other new tool call proves the
    # turn is unblocked, so it may clear a stale red.
    if [ "$TOOL" = "AskUserQuestion" ]; then
      set_state red
    else
      set_state orange
    fi
    ;;
  post)
    # While a permission dialog is pending the turn cannot START new tools,
    # but tools launched earlier in parallel can still FINISH. Their
    # completion must not downgrade red — except AskUserQuestion completing,
    # which means the user just answered.
    if [ "$(cat "$FILE" 2>/dev/null)" = "red" ] && [ "$TOOL" != "AskUserQuestion" ]; then
      :
    else
      set_state orange
    fi
    ;;
  permreq)
    # A permission dialog is about to be shown — the primary red trigger.
    # It fires immediately and works in frontends that never deliver
    # Notification events (e.g. the VS Code extension's native UI).
    # Verified empirically: allowlisted and sandbox-auto-approved calls do
    # NOT fire this event, and acceptEdits mode auto-approves only edits —
    # its other tool calls show real dialogs. Only skip modes that never
    # show a dialog, plus auto mode, where the classifier's evaluation can
    # fire this without a dialog (github.com/anthropics/claude-code/29212).
    case "$(json_field permission_mode)" in
      auto|bypassPermissions|dontAsk) : ;;
      *) set_state red ;;
    esac
    ;;
  attention)
    # Notifications carry a notification_type; only some types mean
    # "waiting on you". Elicitation events (no type field) fall through to
    # the default branch and turn red.
    case "$(json_field notification_type)" in
      auth_success|agent_completed)
        : ;;
      elicitation_complete|elicitation_response)
        set_state orange ;;
      idle_prompt)
        # Don't downgrade a finished (green) session for the idle reminder;
        # it does catch turns that ended with a plain-text question (orange).
        [ "$(cat "$FILE" 2>/dev/null)" = "green" ] || set_state red ;;
      *)
        # permission_prompt, elicitation_dialog, agent_needs_input, or an
        # older CLI without notification_type: legacy message-text guard.
        if [ "$(cat "$FILE" 2>/dev/null)" = "green" ] && printf '%s' "$(json_field message)" | grep -qi 'waiting for your input'; then
          :
        else
          set_state red
        fi ;;
    esac
    ;;
  done)
    set_state green
    ;;
  end)
    rm -f "$FILE"
    ;;
esac
exit 0
