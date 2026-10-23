#!/bin/sh
# Tests the UCI -> YAML translation in homelab-ingress.init without OpenWrt.
#
# Stubs the uci/procd helpers that /etc/rc.common provides, runs start_service
# against fixtures and checks the generated YAML. Runs on any POSIX sh (CI and
# developer machines included); no root and no network.
set -u

INIT=${INIT:-$(dirname "$0")/homelab-ingress.init}
T=$(mktemp -d) || exit 1
trap 'rm -rf "$T"' EXIT INT TERM

export GENERATED_CONFIG="$T/generated.yaml"
export SERVICE_BIN=/usr/bin/true
export GEOIP_CACHE_DIR="$T/geocache"
export UCI_DIR="$T/uci"
mkdir -p "$UCI_DIR"

FAILS=0
ok() { printf 'ok   %s\n' "$*"; }
bad() {
	printf 'FAIL %s\n' "$*"
	FAILS=$((FAILS + 1))
}
check() { # check <description> <command...>
	_desc=$1
	shift
	if "$@"; then ok "$_desc"; else bad "$_desc"; fi
}

# --- fixture helpers -------------------------------------------------------
uci_reset() {
	rm -rf "$UCI_DIR"
	mkdir -p "$UCI_DIR"
}
set_opt() { printf '%s' "$2" > "$UCI_DIR/$1"; }
add_list() { printf '%s\n' "$2" >> "$UCI_DIR/$1"; }

# --- uci / procd stubs -----------------------------------------------------
uci_val() { if [ -f "$UCI_DIR/$1" ]; then cat "$UCI_DIR/$1"; else printf ''; fi; }

config_load() { :; }

config_get() { # config_get <var> <section> <option> [default]
	_v=$(uci_val "$3")
	[ -n "$_v" ] || _v=$4
	eval "$1=\$_v"
}

config_list_foreach() { # config_list_foreach <section> <option> <callback>
	[ -f "$UCI_DIR/$2" ] || return 0
	uci_val "$2" > "$T/list"
	while IFS= read -r _item; do
		[ -n "$_item" ] || continue
		"$3" "$_item"
	done < "$T/list"
}

PROCD_CMD=
procd_open_instance() { :; }
procd_close_instance() { :; }
procd_set_param() {
	if [ "$1" = command ]; then
		shift
		PROCD_CMD=$*
	fi
	return 0
}

# shellcheck disable=SC1090
. "$INIT"

yaml() { cat "$GENERATED_CONFIG"; }
has() { yaml | grep -qF -- "$1"; }

# ---------------------------------------------------------------------------
echo "== full configuration with geo-blocking"
uci_reset
set_opt base_interface br-wan.201
set_opt state_dir "$T/state"
set_opt grpc_listen 0.0.0.0:50051
set_opt ws_listen 0.0.0.0:50052
set_opt auth_issuer 'https://issuer.example.com/dex#frag'
set_opt auth_audience "it's: tricky #really"
set_opt auth_trusted_root_ca /etc/homelab-ingress/oidc-ca.pem
add_list auth_allowed_subjects 'system:serviceaccount:homelab-ingress:homelab-ingress-controller'
add_list auth_allowed_subjects 'user name with spaces'
add_list masquerade 'br-lan.10,br-lan.20'
add_list masquerade 'br-lan.30,br-lan.40'
set_opt geoip_provider dbip
set_opt geoip_refresh_interval 12h
set_opt geoip_stale_after 840h

if start_service; then
	ok "start_service succeeded"
	check "issuer keeps the fragment" has "issuer: 'https://issuer.example.com/dex#frag'"
	check "embedded quote is doubled" has "audience: 'it''s: tricky #really'"
	check "subject with spaces stays one item" has "- 'user name with spaces'"
	check "two masquerade rules emitted" test "$(yaml | grep -c input_device)" -eq 2
	check "geoip provider emitted" has "provider: 'dbip'"
	check "duration passed through" has "refresh_interval: '12h'"
	check "geoip cache defaults off flash" has "cache_path: '$T/geocache/geo.mmdb'"
	check "generated file is not world-readable" sh -c "case \"\$(ls -l '$GENERATED_CONFIG')\" in -rw-------*) exit 0 ;; *) exit 1 ;; esac"
	check "state dir passed as a flag" test "$PROCD_CMD" = "/usr/bin/true -config $GENERATED_CONFIG -state-dir $T/state"
else
	bad "start_service succeeded"
fi

echo "== daemon defaults are not mirrored in the script"
uci_reset
set_opt base_interface br-lan
set_opt auth_issuer https://issuer.example.com
set_opt auth_audience homelab-ingress
add_list auth_allowed_subjects svc-account
if start_service; then
	ok "start_service succeeded"
	check "grpc block omitted" sh -c "! grep -q '^grpc:' '$GENERATED_CONFIG'"
	check "ws block omitted" sh -c "! grep -q '^ws:' '$GENERATED_CONFIG'"
	check "geoip block omitted" sh -c "! grep -q '^geoip:' '$GENERATED_CONFIG'"
	check "no -state-dir flag" sh -c "case '$PROCD_CMD' in *state-dir*) exit 1 ;; *) exit 0 ;; esac"
else
	bad "start_service succeeded"
fi

# ::validation-failures::
echo "== validation failures abort before writing a config"
uci_reset
set_opt base_interface br-lan
set_opt auth_issuer https://issuer.example.com
set_opt auth_audience homelab-ingress
add_list auth_allowed_subjects svc-account

expect_refusal() { # expect_refusal <description>
	rm -f "$GENERATED_CONFIG"
	if start_service 2>/dev/null; then
		bad "$1 (started)"
	elif [ -f "$GENERATED_CONFIG" ]; then
		bad "$1 (config written)"
	else
		ok "$1"
	fi
}

set_opt geoip_provider maxmind
expect_refusal "maxmind without a license key"

set_opt geoip_provider maxmindbad
set_opt geoip_license_key k
expect_refusal "unknown geoip provider"

set_opt geoip_provider ''
set_opt geoip_license_key ''
add_list masquerade 'br-lan.30'
expect_refusal "masquerade entry without an output device"

uci_reset
set_opt base_interface br-lan
set_opt auth_issuer https://issuer.example.com
add_list auth_allowed_subjects svc-account
expect_refusal "missing auth audience"

set_opt auth_audience homelab-ingress
rm -f "$UCI_DIR/auth_allowed_subjects"
expect_refusal "no allowed subjects"

uci_reset
set_opt auth_issuer https://issuer.example.com
set_opt auth_audience homelab-ingress
add_list auth_allowed_subjects svc-account
expect_refusal "missing base interface"

echo
if [ "$FAILS" -eq 0 ]; then
	echo "ALL CHECKS PASSED"
else
	echo "$FAILS CHECK(S) FAILED"
	exit 1
fi
