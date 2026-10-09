#!/bin/sh
# 03-geolite-mock.sh — write the mock GeoLite2 TSV fixture for e2e geo-blocking tests.
#
# The ingress daemon reads this file when maxmind.license_key == "mock".
# The TSV maps CIDR → ISO-3166 alpha-2 code. Each row is read by the
# maxmind mock reader as a literal mapping (LookupCountry returns the
# code for any IP in the CIDR range).
#
# Carve the hl-br0 bridge subnet (10.44.0.0/24) into four /26 ranges:
#   10.44.0.0/26    US      # bridge itself, test process source (10.44.0.1)
#   10.44.0.64/26   CA      # reserved for future multi-country tests
#   10.44.0.128/26  EU      # ns-external netns veth endpoint
#   10.44.0.192/26  XX      # reserved
#
# The Linux kernel drops packets sourced from 127.0.0.0/8 on non-lo
# interfaces, so the EU range is carved from the existing bridge subnet
# (10.44.0.0/24). This also avoids adding a second network prefix to
# the VM's bridge routing table.
#
# The ingress daemon's config expects cache_path (default:
# /var/lib/homelab-ingress/geolite2-country.mmdb) to point at the mock
# file. In the mock code path, cache_path is set to this TSV path:
#   /var/lib/homelab-ingress/mock-geo.tsv

set -eu

MOCK_DIR="/var/lib/homelab-ingress"
MOCK_TSV="${MOCK_DIR}/mock-geo.tsv"

mkdir -p "$MOCK_DIR"

cat > "$MOCK_TSV" <<'EOF'
10.44.0.0/26	US
10.44.0.64/26	CA
10.44.0.128/26	EU
10.44.0.192/26	XX
EOF

chmod 644 "$MOCK_TSV"
echo "03-geolite-mock: wrote $MOCK_TSV"