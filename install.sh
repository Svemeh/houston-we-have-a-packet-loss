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
#   Over SSH, or with nobody logged in on the screen, a LaunchAgent can't
#   start, so it becomes a LaunchDaemon instead: still runs as you, starts at
#   boot, needs sudo once.
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
MAC_AGENT_PLIST="$HOME/Library/LaunchAgents/$MAC_LABEL.plist"
MAC_DAEMON_PLIST="/Library/LaunchDaemons/$MAC_LABEL.plist"

# mac_plist prints the job definition. With a user name it's for a
# LaunchDaemon, which would otherwise run as root.
mac_plist() {
	run_as="$1"
	# ProcessType Interactive keeps macOS from coalescing timers, which
	# would add its own delay to the round-trip times.
	cat <<EOF
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
EOF
	if [ -n "$run_as" ]; then
		cat <<EOF
	<key>UserName</key>
	<string>$run_as</string>
EOF
	fi
	cat <<EOF
</dict>
</plist>
EOF
}

mac_stop() {
	launchctl bootout "gui/$(id -u)/$MAC_LABEL" 2>/dev/null || true
	if [ -f "$MAC_DAEMON_PLIST" ]; then
		as_root launchctl bootout "system/$MAC_LABEL" 2>/dev/null || true
	fi
}

# mac_bootstrap loads a plist into a launchd domain. bootout returns before
# the old job is fully gone, so a busy job is retried briefly. Returns 2 when
# the domain isn't there (nobody logged in on the screen, or over SSH).
mac_bootstrap() {
	domain="$1"
	plist="$2"
	tries=0
	while :; do
		if [ "$domain" = system ]; then
			output=$(as_root launchctl bootstrap system "$plist" 2>&1) && return 0
		else
			output=$(launchctl bootstrap "$domain" "$plist" 2>&1) && return 0
		fi
		case "$output" in
		*"125:"* | *"Could not find domain"*) return 2 ;;
		esac
		tries=$((tries + 1))
		if [ $tries -ge 5 ]; then
			say "launchctl: $output" >&2
			return 1
		fi
		sleep 1
	done
}

mac_install_daemon() {
	rm -f "$MAC_AGENT_PLIST" # or it would start a second copy at the next login
	mac_plist "$(id -un)" >"$WORK/daemon.plist"
	# launchd only loads daemons owned by root and not writable by others.
	as_root install -m 644 -o root -g wheel "$WORK/daemon.plist" "$MAC_DAEMON_PLIST"
	mac_bootstrap system "$MAC_DAEMON_PLIST" ||
		fail "launchctl could not start Houston (see $MAC_LOG_DIR/houston.log)"
	MAC_STARTS="Starts at boot."
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

	# Already a system service from an earlier install: keep it one.
	if [ -f "$MAC_DAEMON_PLIST" ]; then
		mac_install_daemon
	else
		mac_plist "" >"$MAC_AGENT_PLIST"
		status=0
		mac_bootstrap "gui/$(id -u)" "$MAC_AGENT_PLIST" || status=$?
		case $status in
		0) MAC_STARTS="Starts when you log in." ;;
		2)
			say "Can't start a login item from here (SSH, or nobody logged in on the screen)."
			say "Installing a system service instead: runs as $(id -un), starts at boot."
			mac_install_daemon
			;;
		*) fail "launchctl could not start Houston (see $MAC_LOG_DIR/houston.log)" ;;
		esac
	fi

	say "Installed $("$MAC_APP_DIR/houston" -version). $MAC_STARTS"
	say "  data: $MAC_APP_DIR/data"
	say "  log:  $MAC_LOG_DIR/houston.log"
	if wait_for_dashboard && [ -z "${SSH_CONNECTION:-}" ]; then
		open "http://localhost:$PORT/" >/dev/null 2>&1 || true
	fi
	address=$(ipconfig getifaddr en0 2>/dev/null || ipconfig getifaddr en1 2>/dev/null || true)
	say "Dashboard: http://localhost:$PORT${address:+ (from other devices: http://$address:$PORT)}"
}

mac_uninstall() {
	mac_stop
	rm -f "$MAC_AGENT_PLIST" "$MAC_APP_DIR/houston"
	if [ -f "$MAC_DAEMON_PLIST" ]; then
		as_root rm -f "$MAC_DAEMON_PLIST"
	fi
	say "Houston removed. Your data is still in $MAC_APP_DIR/data"
}

# ---- Linux -------------------------------------------------------------------

LINUX_BIN=/usr/local/bin/houston
LINUX_UNIT=/etc/systemd/system/houston.service

as_root() {
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
		as_root systemctl stop houston 2>/dev/null || true
	fi
	as_root install -m 755 "$WORK/houston" "$LINUX_BIN"

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
	as_root tee "$LINUX_UNIT" >/dev/null <<EOF
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
	as_root systemctl daemon-reload
	as_root systemctl enable --now houston >/dev/null 2>&1 ||
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
		as_root systemctl disable --now houston 2>/dev/null || true
		as_root rm -f "$LINUX_UNIT"
		as_root systemctl daemon-reload
	fi
	as_root rm -f "$LINUX_BIN"
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
