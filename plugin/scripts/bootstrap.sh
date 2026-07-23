#!/bin/bash
# claude-semaphore bootstrap — runs async on SessionStart. Ensures the tray
# app is downloaded, registered to start at login, and running right now.
# Must never block or break a Claude session: every failure path exits 0.

set -u

REPO="TaulantSela/claude-semaphore"
BIN_DIR="$HOME/.claude/semaphore-tray"
STATE_DIR="$HOME/.claude/semaphore"
mkdir -p "$BIN_DIR" "$STATE_DIR" 2>/dev/null

case "$(uname -s)" in
  Darwin)               OS=darwin ;;
  Linux)                OS=linux ;;
  MINGW*|MSYS*|CYGWIN*) OS=windows ;;
  *)                    exit 0 ;;
esac

case "$(uname -m)" in
  arm64|aarch64) ARCH=arm64 ;;
  x86_64|amd64)  ARCH=amd64 ;;
  *)             exit 0 ;;
esac

BIN="$BIN_DIR/claude-semaphore"
[ "$OS" = windows ] && BIN="$BIN.exe"
VERFILE="$BIN_DIR/.version"
PLIST="$HOME/Library/LaunchAgents/com.claude-semaphore.plist"

# The plugin version, read from the manifest beside this script. The binary is
# tagged with it so a plugin update also pulls a matching tray binary.
WANT_VER=$(sed -n 's/.*"version"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' \
  "$(dirname "$0")/../.claude-plugin/plugin.json" 2>/dev/null | head -1)
HAVE_VER=$(cat "$VERFILE" 2>/dev/null)

# 1. Download the tray binary on first run, or refresh it when the plugin
#    version changed — so updating the plugin also updates the binary, instead
#    of leaving an old tray in place forever (it was only ever fetched once).
if [ ! -x "$BIN" ] || { [ -n "$WANT_VER" ] && [ "$WANT_VER" != "$HAVE_VER" ]; }; then
  UPDATING=0
  [ -x "$BIN" ] && UPDATING=1

  # Stop the running tray so its file can be replaced (mandatory on Windows,
  # where a running .exe is locked) and the new binary can take over.
  if [ "$UPDATING" = 1 ]; then
    case "$OS" in
      windows) taskkill //F //IM "$(basename "$BIN")" >/dev/null 2>&1 ;;
      *)       pkill -f "$BIN" 2>/dev/null ;;
    esac
    sleep 1
  fi

  ASSET="claude-semaphore-$OS-$ARCH"
  [ "$OS" = windows ] && ASSET="$ASSET.exe"

  # Prefer the asset tagged with the plugin version so binary and hooks match;
  # fall back to the latest release if that tag has no asset yet.
  GOT=""
  if [ -n "$WANT_VER" ] &&
     curl -fsSL --retry 2 -o "$BIN.tmp" \
       "https://github.com/$REPO/releases/download/v$WANT_VER/$ASSET" 2>/dev/null &&
     [ -s "$BIN.tmp" ]; then
    GOT="$WANT_VER"
  elif curl -fsSL --retry 2 -o "$BIN.tmp" \
       "https://github.com/$REPO/releases/latest/download/$ASSET" 2>/dev/null &&
     [ -s "$BIN.tmp" ]; then
    GOT="latest"
  fi

  if [ -n "$GOT" ]; then
    chmod +x "$BIN.tmp" 2>/dev/null
    if mv "$BIN.tmp" "$BIN" 2>/dev/null; then
      # Record the version only when we got the exact one asked for, so a
      # latest-fallback keeps trying for the pinned build next session.
      [ "$GOT" = "$WANT_VER" ] && printf '%s' "$WANT_VER" > "$VERFILE" 2>/dev/null
      # macOS: a launchd job pins a code requirement to the binary it was first
      # bootstrapped with and kills a replaced binary with OS_REASON_CODESIGNING.
      # Re-bootstrap so the requirement re-derives from the new binary.
      if [ "$UPDATING" = 1 ] && [ "$OS" = darwin ] && [ -f "$PLIST" ]; then
        launchctl bootout "gui/$(id -u)/com.claude-semaphore" 2>/dev/null
        launchctl bootstrap "gui/$(id -u)" "$PLIST" 2>/dev/null
      fi
    else
      rm -f "$BIN.tmp"
    fi
  else
    rm -f "$BIN.tmp"
    [ "$UPDATING" = 0 ] && exit 0 # first-run download failed: nothing to launch
  fi
fi

# 2. Register login autostart, once.
MARKER="$BIN_DIR/.autostart-installed"
if [ ! -f "$MARKER" ]; then
  case "$OS" in
    darwin)
      PLIST="$HOME/Library/LaunchAgents/com.claude-semaphore.plist"
      cat > "$PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.claude-semaphore</string>
    <key>ProgramArguments</key>
    <array>
        <string>$BIN</string>
    </array>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>ProcessType</key>
    <string>Interactive</string>
</dict>
</plist>
EOF
      launchctl bootstrap "gui/$(id -u)" "$PLIST" 2>/dev/null
      ;;
    linux)
      mkdir -p "$HOME/.config/autostart" 2>/dev/null
      cat > "$HOME/.config/autostart/claude-semaphore.desktop" <<EOF
[Desktop Entry]
Type=Application
Name=Claude Semaphore
Comment=Traffic light for Claude Code state
Exec=$BIN
X-GNOME-Autostart-enabled=true
EOF
      ;;
    windows)
      WIN_BIN=$(cygpath -w "$BIN" 2>/dev/null) &&
        reg.exe add 'HKCU\Software\Microsoft\Windows\CurrentVersion\Run' \
          /v ClaudeSemaphore /t REG_SZ /d "\"$WIN_BIN\"" /f >/dev/null 2>&1
      ;;
  esac
  touch "$MARKER" 2>/dev/null
fi

# 3. Start it now. The app holds a localhost port as a single-instance lock
#    and exits immediately if another copy is already running, so spawning
#    unconditionally is safe.
case "$OS" in
  windows)
    WIN_BIN=$(cygpath -w "$BIN" 2>/dev/null) &&
      cmd.exe //c start '""' "$WIN_BIN" >/dev/null 2>&1 &
    ;;
  *)
    nohup "$BIN" >/dev/null 2>&1 &
    ;;
esac
exit 0
