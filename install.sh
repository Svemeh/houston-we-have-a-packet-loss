#!/bin/sh
# Installs Houston as a background service on macOS or Linux, from the latest
# GitHub release, and opens the dashboard.
#
#   curl -fsSL https://raw.githubusercontent.com/Svemeh/houston-we-have-a-packet-loss/main/install.sh | sh
#
# Run it again to update. To remove (your data is kept):
#
#   curl -fsSL https://raw.githubusercontent.com/Svemeh/houston-we-have-a-packet-loss/main/install.sh | sh -s -- uninstall
#
# macOS: a LaunchAgent for your user, no sudo. Starts when you log in.
#   app + data  ~/Library/Application Support/Houston
#   log         ~/Library/Logs/Houston/houston.log
# Linux: a systemd service, needs sudo. Starts at boot.
#   binary      /usr/local/bin/houston
#   data        /var/lib/houston
#   log         journalctl -u houston

set -eu

REPO="Svemeh/houston-we-have-a-packet-loss"
# Override to test against a local copy of a release.
DOWNLOAD_URL="${HOUSTON_DOWNLOAD_URL:-https://github.com/$REPO/releases/latest/download}"
PORT=2101
MAC_LABEL="com.svemeh.houston"

say() { printf '%s\n' "$*"; }
fail() {
	printf 'houston: %s\n' "$*" >&2
	exit 1
}
have() { command -v "$1" >/dev/null 2>&1; }

# ---- What to download -------------------------------------------------------

detect_asset() {
	os=$(uname -s)
	arch=$(uname -m)
	case "$os" in
	Darwin | Linux) ;;
	*) fail "unsupported OS: $os (on Windows, use install.ps1)" ;;
	esac
	case "$arch" in
	x86_64 | amd64) arch=x86_64 ;;
	arm64 | aarch64) arch=arm64 ;;
	armv7*) arch=armv7 ;;
	*) fail "unsupported CPU: $arch" ;;
	esac
	# A Terminal running under Rosetta reports x86_64 on Apple silicon.
	if [ "$os" = Darwin ] && [ "$arch" = x86_64 ] &&
		[ "$(sysctl -n hw.optional.arm64 2>/dev/null || echo 0)" = 1 ]; then
		arch=arm64
	fi
	ASSET="houston_${os}_${arch}.tar.gz"
	OS="$os"
}

# download_and_verify leaves the binary at $WORK/houston.
download_and_verify() {
	have curl || fail "needs curl"
	have tar || fail "needs tar"
	WORK=$(mktemp -d)
	trap 'rm -rf "$WORK"' EXIT

	say "Downloading $ASSET ..."
	curl -fsSL -o "$WORK/$ASSET" "$DOWNLOAD_URL/$ASSET" ||
		fail "could not download $DOWNLOAD_URL/$ASSET"
	curl -fsSL -o "$WORK/checksums.txt" "$DOWNLOAD_URL/checksums.txt" ||
		fail "could not download checksums.txt"

	expected=$(awk -v name="$ASSET" '$2 == name { print $1 }' "$WORK/checksums.txt")
	[ -n "$expected" ] || fail "$ASSET is not in checksums.txt"
	if have sha256sum; then
		actual=$(sha256sum "$WORK/$ASSET" | awk '{ print $1 }')
	elif have shasum; then
		actual=$(shasum -a 256 "$WORK/$ASSET" | awk '{ print $1 }')
	else
		fail "needs sha256sum or shasum to check the download"
	fi
	[ "$expected" = "$actual" ] || fail "checksum mismatch for $ASSET, not installing"

	tar -xzf "$WORK/$ASSET" -C "$WORK" houston
	chmod 755 "$WORK/houston"
}

# ---- Shared ------------------------------------------------------------------

port_in_use() {
	if have lsof; then
		lsof -nP -iTCP:"$PORT" -sTCP:LISTEN >/dev/null 2>&1
	elif have ss; then
		ss -ltn "sport = :$PORT" 2>/dev/null | grep -q LISTEN
	else
		return 1
	fi
}

wait_for_dashboard() {
	tries=0
	while [ $tries -lt 15 ]; do
		curl -fs -o /dev/null "http://localhost:$PORT/" && return 0
		tries=$((tries + 1))
		sleep 1
	done
	return 1
}

# ---- macOS -------------------------------------------------------------------

MAC_APP_DIR="$HOME/Library/Application Support/Houston"
MAC_LOG_DIR="$HOME/Library/Logs/Houston"
MAC_PLIST="$HOME/Library/LaunchAgents/$MAC_LABEL.plist"

mac_stop() {
	launchctl bootout "gui/$(id -u)/$MAC_LABEL" 2>/dev/null || true
}

mac_install() {
	mac_stop
	sleep 1 # let the old process let go of the port before checking it
	if port_in_use; then
		say "Note: something else is using port $PORT (an earlier 'go run .'?)."
		say "      Stop it; Houston keeps retrying and takes over the port."
	fi

	mkdir -p "$MAC_APP_DIR" "$MAC_LOG_DIR" "$HOME/Library/LaunchAgents"
	mv -f "$WORK/houston" "$MAC_APP_DIR/houston"

	# ProcessType Interactive keeps macOS from coalescing timers, which
	# would add its own delay to the round-trip times.
	cat >"$MAC_PLIST" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>$MAC_LABEL</string>
	<key>ProgramArguments</key>
	<array>
		<string>$MAC_APP_DIR/houston</string>
	</array>
	<key>WorkingDirectory</key>
	<string>$MAC_APP_DIR</string>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>ThrottleInterval</key>
	<integer>10</integer>
	<key>ProcessType</key>
	<string>Interactive</string>
	<key>StandardOutPath</key>
	<string>$MAC_LOG_DIR/houston.log</string>
	<key>StandardErrorPath</key>
	<string>$MAC_LOG_DIR/houston.log</string>
</dict>
</plist>
EOF

	# bootout returns before the old job is fully gone; retry briefly.
	tries=0
	until launchctl bootstrap "gui/$(id -u)" "$MAC_PLIST" 2>/dev/null; do
		tries=$((tries + 1))
		[ $tries -lt 5 ] || fail "launchctl could not start Houston (see $MAC_LOG_DIR/houston.log)"
		sleep 1
	done

	say "Installed $("$MAC_APP_DIR/houston" -version). Starts when you log in."
	say "  data: $MAC_APP_DIR/data"
	say "  log:  $MAC_LOG_DIR/houston.log"
	if wait_for_dashboard; then
		open "http://localhost:$PORT/"
	fi
	say "Dashboard: http://localhost:$PORT"
}

mac_uninstall() {
	mac_stop
	rm -f "$MAC_PLIST" "$MAC_APP_DIR/houston"
	say "Houston removed. Your data is still in $MAC_APP_DIR/data"
}

# ---- Linux -------------------------------------------------------------------

LINUX_BIN=/usr/local/bin/houston
LINUX_UNIT=/etc/systemd/system/houston.service

linux_sudo() {
	if [ "$(id -u)" -eq 0 ]; then
		"$@"
	else
		have sudo || fail "needs root: run as root or install sudo"
		sudo "$@"
	fi
}

have_systemd() {
	have systemctl && [ -d /run/systemd/system ]
}

linux_install() {
	say "Installing to $LINUX_BIN ..."
	if have_systemd; then
		linux_sudo systemctl stop houston 2>/dev/null || true
	fi
	linux_sudo install -m 755 "$WORK/houston" "$LINUX_BIN"

	if ! have_systemd; then
		say "Installed $("$LINUX_BIN" -version). No systemd here, so start it yourself:"
		say "  cd <a folder for its data> && houston"
		return
	fi

	if port_in_use; then
		say "Note: something else is using port $PORT (an earlier 'go run .'?)."
		say "      Stop it; the service retries every 5 seconds."
	fi

	# DynamicUser runs it as a throwaway unprivileged user; StateDirectory
	# gives that user /var/lib/houston, which survives updates.
	linux_sudo tee "$LINUX_UNIT" >/dev/null <<EOF
[Unit]
Description=Houston, We Have a Packet Loss (Starlink monitor)
Documentation=https://github.com/$REPO
Wants=network-online.target
After=network-online.target

[Service]
ExecStart=$LINUX_BIN
DynamicUser=yes
StateDirectory=houston
WorkingDirectory=/var/lib/houston
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
EOF
	linux_sudo systemctl daemon-reload
	linux_sudo systemctl enable --now houston >/dev/null 2>&1 ||
		fail "systemd could not start Houston (journalctl -u houston)"

	say "Installed $("$LINUX_BIN" -version). Starts at boot."
	say "  data: /var/lib/houston/data"
	say "  log:  journalctl -u houston"
	if wait_for_dashboard && [ -n "${DISPLAY:-}${WAYLAND_DISPLAY:-}" ] && have xdg-open; then
		xdg-open "http://localhost:$PORT/" >/dev/null 2>&1 || true
	fi
	address=$(hostname -I 2>/dev/null | awk '{ print $1 }')
	say "Dashboard: http://localhost:$PORT${address:+ (from other devices: http://$address:$PORT)}"
}

linux_uninstall() {
	if have_systemd && [ -f "$LINUX_UNIT" ]; then
		linux_sudo systemctl disable --now houston 2>/dev/null || true
		linux_sudo rm -f "$LINUX_UNIT"
		linux_sudo systemctl daemon-reload
	fi
	linux_sudo rm -f "$LINUX_BIN"
	say "Houston removed. Your data is still in /var/lib/houston/data"
}

# ---- Main --------------------------------------------------------------------

main() {
	action="${1:-install}"
	detect_asset
	case "$action" in
	install)
		download_and_verify
		if [ "$OS" = Darwin ]; then mac_install; else linux_install; fi
		;;
	uninstall)
		if [ "$OS" = Darwin ]; then mac_uninstall; else linux_uninstall; fi
		;;
	*) fail "unknown action: $action (install or uninstall)" ;;
	esac
}

main "$@"
