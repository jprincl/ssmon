#!/bin/sh
# Download the current DB-IP Lite City + ASN databases (CC BY 4.0) and tell
# ssmon to reload them. DB-IP publishes a new edition at the start of each
# month. Run from cron, e.g.:  17 4 3 * * /opt/ssmon/update-geoip.sh
set -eu

DIR=/opt/ssmon/geoip
MONTH=$(date -u +%Y-%m)
PREV=$(date -u -d "$(date -u +%Y-%m-01) -1 day" +%Y-%m)
mkdir -p "$DIR"

fetch() { # $1 = city|asn
    for m in "$MONTH" "$PREV"; do
        url="https://download.db-ip.com/free/dbip-$1-lite-$m.mmdb.gz"
        if curl -fsSL "$url" -o "$DIR/$1.mmdb.gz.tmp"; then
            gunzip -c "$DIR/$1.mmdb.gz.tmp" > "$DIR/dbip-$1-lite.mmdb.tmp"
            mv "$DIR/dbip-$1-lite.mmdb.tmp" "$DIR/dbip-$1-lite.mmdb"
            rm -f "$DIR/$1.mmdb.gz.tmp"
            echo "updated $1 database ($m)"
            return 0
        fi
    done
    echo "failed to download $1 database" >&2
    return 1
}

fetch city
fetch asn
chmod 644 "$DIR"/*.mmdb
systemctl reload ssmon 2>/dev/null || true
