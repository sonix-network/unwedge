#!/bin/sh
# Generate a self-signed CA plus server and client certificates for wrtremoted
# mutual TLS. For a small single-controller deployment this is sufficient; for
# anything larger use your real PKI.
#
# Usage: scripts/gen-certs.sh <server-hostname-or-ip> [output-dir]
#
# Produces in <output-dir> (default ./certs):
#   ca.crt / ca.key           - the CA (keep ca.key offline/secret)
#   server.crt / server.key   - for wrtremoted   (grpc.tls.cert_file/key_file)
#   client-ca.crt             - copy of ca.crt    (grpc.tls.client_ca_file)
#   client.crt / client.key   - for wrtremotectl / CI (mutual TLS)
set -eu

SERVER_NAME="${1:?usage: gen-certs.sh <server-hostname-or-ip> [output-dir]}"
OUT="${2:-certs}"
DAYS="${DAYS:-825}"

mkdir -p "$OUT"
cd "$OUT"

# Decide whether SERVER_NAME is an IP or a DNS name for the SAN.
if printf '%s' "$SERVER_NAME" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$'; then
  SAN="IP:$SERVER_NAME"
else
  SAN="DNS:$SERVER_NAME"
fi

echo "==> CA"
openssl genrsa -out ca.key 4096
openssl req -x509 -new -nodes -key ca.key -sha256 -days "$DAYS" \
  -subj "/CN=wrt-remote-ca" -out ca.crt

gen_leaf() {
  name="$1"; subj="$2"; ext="$3"
  openssl genrsa -out "$name.key" 2048
  openssl req -new -key "$name.key" -subj "$subj" -out "$name.csr"
  openssl x509 -req -in "$name.csr" -CA ca.crt -CAkey ca.key -CAcreateserial \
    -sha256 -days "$DAYS" -out "$name.crt" -extfile "$ext"
  rm -f "$name.csr"
}

echo "==> server cert for $SERVER_NAME ($SAN)"
cat > server.ext <<EOF
basicConstraints=CA:FALSE
keyUsage=digitalSignature,keyEncipherment
extendedKeyUsage=serverAuth
subjectAltName=$SAN
EOF
gen_leaf server "/CN=$SERVER_NAME" server.ext

echo "==> client cert (mutual TLS)"
cat > client.ext <<EOF
basicConstraints=CA:FALSE
keyUsage=digitalSignature
extendedKeyUsage=clientAuth
EOF
gen_leaf client "/CN=wrt-remote-client" client.ext

cp ca.crt client-ca.crt
rm -f server.ext client.ext ca.srl

echo
echo "Done. In config.yaml set:"
echo "  grpc.tls.cert_file:      $OUT/server.crt"
echo "  grpc.tls.key_file:       $OUT/server.key"
echo "  grpc.tls.client_ca_file: $OUT/client-ca.crt"
echo "Client (wrtremotectl / CI) uses: ca.crt, client.crt, client.key"
