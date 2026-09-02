#!/usr/bin/env bash
# Development-only material: never use anything under ./certs outside a local machine.
set -euo pipefail

readonly VALIDITY_DAYS=90
readonly KEY_CURVE=prime256v1

repo_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
certs_dir="$repo_root/certs"

force=false
case "${1-}" in
	"") ;;
	--force) force=true ;;
	*)
		printf 'usage: %s [--force]\n' "$0" >&2
		exit 2
		;;
esac

certs_dir_has_material() {
	set -- "$certs_dir"/*.pem
	[ -e "$1" ]
}

issue_leaf() {
	local name=$1 common_name=$2 extensions=$3
	local key="$work_dir/$name-key.pem"

	openssl ecparam -name "$KEY_CURVE" -genkey -noout -out "$key"
	openssl req -new -key "$key" -sha256 -subj "/CN=$common_name" \
		-out "$work_dir/$name.csr"
	printf '%s\n' "$extensions" >"$work_dir/$name.ext"
	openssl x509 -req -in "$work_dir/$name.csr" -CA "$ca_cert" -CAkey "$ca_key" \
		-CAserial "$work_dir/ca.srl" -CAcreateserial -sha256 \
		-days "$VALIDITY_DAYS" -extfile "$work_dir/$name.ext" \
		-out "$work_dir/$name.pem"
}

if certs_dir_has_material && [ "$force" = false ]; then
	printf '%s already holds certificates; rerun with --force to replace them.\n' \
		"$certs_dir" >&2
	exit 1
fi

umask 077
work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT
ca_cert="$work_dir/ca.pem"
ca_key="$work_dir/ca-key.pem"

openssl ecparam -name "$KEY_CURVE" -genkey -noout -out "$ca_key"
openssl req -new -x509 -key "$ca_key" -sha256 -days "$VALIDITY_DAYS" \
	-subj '/CN=lore development CA' -out "$ca_cert" \
	-addext 'basicConstraints=critical,CA:TRUE,pathlen:0' \
	-addext 'keyUsage=critical,keyCertSign,cRLSign' \
	-addext 'subjectKeyIdentifier=hash'

issue_leaf server 'lore development server' 'basicConstraints=critical,CA:FALSE
keyUsage=critical,digitalSignature
extendedKeyUsage=serverAuth
subjectAltName=DNS:localhost,IP:127.0.0.1,IP:::1'

issue_leaf client 'lore development client' 'basicConstraints=critical,CA:FALSE
keyUsage=critical,digitalSignature
extendedKeyUsage=clientAuth'

mkdir -p -m 700 "$certs_dir"
mv "$work_dir"/*.pem "$certs_dir"

cat <<'YAML'
server:
  mtls:
    cert: ./certs/server.pem
    key: ./certs/server-key.pem
    client_ca: ./certs/ca.pem
YAML
