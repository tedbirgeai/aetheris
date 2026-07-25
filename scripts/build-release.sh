#!/usr/bin/env bash
# AETHERIS cross-compilation build betigi.
# Linux (amd64, arm64), Windows (amd64), macOS (amd64, arm64) hedeflerine
# tek binary uretir. Dis bagimlilik/CGO gerektirmez.
#
# Kullanim:
#   bash scripts/build-release.sh            # tum hedefler
#   VERSION=v0.6a bash scripts/build-release.sh
set -euo pipefail

VERSION="${VERSION:-v0.6a-turnkey}"
OUTDIR="${OUTDIR:-dist}"
LDFLAGS="-s -w -X main.Version=${VERSION}"

# hedef matrisi: GOOS GOARCH
TARGETS=(
  "linux amd64"
  "linux arm64"
  "windows amd64"
  "darwin amd64"
  "darwin arm64"
)

# uretilecek binary'ler: <ad>=<paket yolu>
declare -A BINS=(
  ["aetheris-cli"]="./cmd/aetheris-cli"
  ["aetheris-gateway"]="./cmd/gateway"
)

mkdir -p "${OUTDIR}"
echo "AETHERIS release ${VERSION} -> ${OUTDIR}/"

for name in "${!BINS[@]}"; do
  pkg="${BINS[$name]}"
  for t in "${TARGETS[@]}"; do
    goos="${t% *}"; goarch="${t#* }"
    ext=""; [ "${goos}" = "windows" ] && ext=".exe"
    out="${OUTDIR}/${name}-${VERSION}-${goos}-${goarch}${ext}"
    echo "  ${name}  ${goos}/${goarch}"
    CGO_ENABLED=0 GOOS="${goos}" GOARCH="${goarch}" \
      go build -trimpath -ldflags "${LDFLAGS}" -o "${out}" "${pkg}"
  done
done

echo "Tamamlandi. Uretilen dosyalar:"
ls -1 "${OUTDIR}/"
