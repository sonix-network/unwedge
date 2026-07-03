#!/bin/sh
# Generate a self-signed CA plus server and client certificates for unwedged
# mutual TLS. For a small single-controller deployment this is sufficient; for
# anything larger use your real PKI.
#
# Usage: scripts/gen-certs.sh <name>[,<name>...] [output-dir]
#
# <name> is a hostname or IP the server cert should be valid for. The first name
# becomes the certificate CN; all names become SANs.
#
# Per-device certs (recommended for multiple devices on one controller): issue
# one server cert per device, each valid only for that device's name. The CA and
# client cert are created once and reused on subsequent runs; NAME picks the
# server key/cert basename so they don't clobber:
#
#   NAME=dut1 scripts/gen-certs.sh dut1.lab      # ca + client (first run) + dut1.{crt,key}
#   NAME=dut2 scripts/gen-certs.sh dut2.lab      # reuses ca + client, adds dut2.{crt,key}
#
# clients verify TLS against the *device* name (not the SRV target), so each
# instance uses its own server cert. Alternatively, one cert can cover several
# names, including a wildcard:
#
#   scripts/gen-certs.sh 'controller.lab,*.lab.example.com'
#
# Produces in <output-dir> (default ./certs):
#   ca.crt / ca.key           - the CA (keep ca.key offline/secret); reused if present
#   $NAME.crt / $NAME.key      - the server cert (NAME defaults to "server")
#   client-ca.crt             - copy of ca.crt    (grpc.tls.client_ca_file)
#   client.crt / client.key   - for unwedge / CI  (mutual TLS); reused if present
set -eu

NAMES="${1:?usage: gen-certs.sh <name>[,<name>...] [output-dir]}"
OUT="${2:-certs}"
DAYS="${DAYS:-825}"
CERT="${NAME:-server}" # server key/cert basename; set NAME per device

# Build the subjectAltName list from the comma-separated names, classifying each
# entry as an IP or a DNS name. The first entry is also used as the CN.
CN=""
SAN=""
OLDIFS="$IFS"
IFS=','
for n in $NAMES; do
  [ -n "$n" ] || continue
  [ -n "$CN" ] || CN="$n"
  if printf '%s' "$n" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$'; then
    entry="IP:$n"
  else
    entry="DNS:$n"
  fi
  if [ -n "$SAN" ]; then SAN="$SAN,$entry"; else SAN="$entry"; fi
done
IFS="$OLDIFS"
: "${CN:?no names given}"

mkdir -p "$OUT"
cd "$OUT"

gen_leaf() {
  name="$1"; subj="$2"; ext="$3"
  openssl genrsa -out "$name.key" 2048
  openssl req -new -key "$name.key" -subj "$subj" -out "$name.csr"
  # Keep ca.srl (do not delete it) so per-device certs get distinct serials.
  openssl x509 -req -in "$name.csr" -CA ca.crt -CAkey ca.key -CAcreateserial \
    -sha256 -days "$DAYS" -out "$name.crt" -extfile "$ext"
  rm -f "$name.csr"
}

# Create the CA once and reuse it, so several server certs share one trust root.
if [ -f ca.key ] && [ -f ca.crt ]; then
  echo "==> reusing existing CA (ca.crt)"
else
  echo "==> CA"
  openssl genrsa -out ca.key 4096
  openssl req -x509 -new -nodes -key ca.key -sha256 -days "$DAYS" \
    -subj "/CN=unwedge-ca" -out ca.crt
fi

echo "==> server cert '$CERT' for $CN ($SAN)"
cat > "$CERT.ext" <<EOF
basicConstraints=CA:FALSE
keyUsage=digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectAltName=$SAN
EOF
gen_leaf "$CERT" "/CN=$CN" "$CERT.ext"
rm -f "$CERT.ext"

# Create the client cert once and reuse it across per-device runs.
if [ -f client.crt ] && [ -f client.key ]; then
  echo "==> reusing existing client cert"
else
  echo "==> client cert (mutual TLS)"
  cat > client.ext <<EOF
basicConstraints=CA:FALSE
keyUsage=digitalSignature
extendedKeyUsage=clientAuth
EOF
  gen_leaf client "/CN=unwedge-client" client.ext
  rm -f client.ext
fi

cp ca.crt client-ca.crt

echo
echo "Done. In this instance's config set:"
echo "  grpc.tls.cert_file:      $OUT/$CERT.crt"
echo "  grpc.tls.key_file:       $OUT/$CERT.key"
echo "  grpc.tls.client_ca_file: $OUT/client-ca.crt"
echo "Client (unwedge / CI) uses: ca.crt, client.crt, client.key"
