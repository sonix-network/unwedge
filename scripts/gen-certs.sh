#!/bin/sh
# Generate a self-signed CA plus server and client certificates for unwedged
# mutual TLS. For a small single-controller deployment this is sufficient; for
# anything larger use your real PKI.
#
# Usage: scripts/gen-certs.sh <name>[,<name>...] [output-dir]
#
# <name> is a hostname or IP the server cert should be valid for. Pass several,
# comma-separated, to cover multiple devices on one controller — clients that
# discover a device via SRV verify TLS against the *device* name, not the
# controller, so the cert must carry each device name. A wildcard works too:
#
#   scripts/gen-certs.sh 'controller.lab,*.lab.example.com'
#   scripts/gen-certs.sh 'dut1.lab,dut2.lab,10.0.0.2'
#
# The first name becomes the certificate CN; all names become SANs.
#
# Produces in <output-dir> (default ./certs):
#   ca.crt / ca.key           - the CA (keep ca.key offline/secret)
#   server.crt / server.key   - for unwedged     (grpc.tls.cert_file/key_file)
#   client-ca.crt             - copy of ca.crt    (grpc.tls.client_ca_file)
#   client.crt / client.key   - for unwedge / CI  (mutual TLS)
set -eu

NAMES="${1:?usage: gen-certs.sh <name>[,<name>...] [output-dir]}"
OUT="${2:-certs}"
DAYS="${DAYS:-825}"

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

echo "==> CA"
openssl genrsa -out ca.key 4096
openssl req -x509 -new -nodes -key ca.key -sha256 -days "$DAYS" \
  -subj "/CN=unwedge-ca" -out ca.crt

gen_leaf() {
  name="$1"; subj="$2"; ext="$3"
  openssl genrsa -out "$name.key" 2048
  openssl req -new -key "$name.key" -subj "$subj" -out "$name.csr"
  openssl x509 -req -in "$name.csr" -CA ca.crt -CAkey ca.key -CAcreateserial \
    -sha256 -days "$DAYS" -out "$name.crt" -extfile "$ext"
  rm -f "$name.csr"
}

echo "==> server cert for $CN ($SAN)"
cat > server.ext <<EOF
basicConstraints=CA:FALSE
keyUsage=digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectAltName=$SAN
EOF
gen_leaf server "/CN=$CN" server.ext

echo "==> client cert (mutual TLS)"
cat > client.ext <<EOF
basicConstraints=CA:FALSE
keyUsage=digitalSignature
extendedKeyUsage=clientAuth
EOF
gen_leaf client "/CN=unwedge-client" client.ext

cp ca.crt client-ca.crt
rm -f server.ext client.ext ca.srl

echo
echo "Done. In each instance's config set:"
echo "  grpc.tls.cert_file:      $OUT/server.crt"
echo "  grpc.tls.key_file:       $OUT/server.key"
echo "  grpc.tls.client_ca_file: $OUT/client-ca.crt"
echo "Client (unwedge / CI) uses: ca.crt, client.crt, client.key"
