#!/bin/bash
# Generate kata-containers config-settings.go from template
# This script should be run after `go mod vendor` to fix kata-containers build

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(dirname "$SCRIPT_DIR")"
KATA_UTILS_DIR="$ROOT_DIR/vendor/github.com/kata-containers/kata-containers/src/runtime/pkg/katautils"

TEMPLATE_FILE="$KATA_UTILS_DIR/config-settings.go.in"
OUTPUT_FILE="$KATA_UTILS_DIR/config-settings.go"

if [ ! -f "$TEMPLATE_FILE" ]; then
    echo "Template file not found: $TEMPLATE_FILE"
    echo "Make sure you have run 'go mod vendor' first."
    exit 1
fi

echo "Generating $OUTPUT_FILE from template..."

cp "$TEMPLATE_FILE" "$OUTPUT_FILE"

sed -i \
  -e 's|^/ |// |' \
  -e 's|@RUNTIME_NAME@|kata-runtime|g' \
  -e 's|@SHIMV2_NAME@|containerd-shim-kata-v2|g' \
  -e 's|@PROJECT_NAME@|Kata Containers|g' \
  -e 's|@PROJECT_TYPE@|kata|g' \
  -e 's|@PROJECT_URL@|https://github.com/kata-containers|g' \
  -e 's|@PROJECT_ORG@|kata-containers|g' \
  -e 's|@PKGRUNDIR@|/run/kata-containers|g' \
  -e 's|@COMMIT@|unknown|g' \
  -e 's|@VERSION@|0.0.0|g' \
  -e 's|@CONFIG_PATH@|/etc/kata-containers/configuration.toml|g' \
  -e 's|@SYSCONFIG@|/usr/share/defaults/kata-containers/configuration.toml|g' \
  -e 's|@MACHINETYPE@||g' \
  -e 's|@KERNELTYPE@|uncompressed|g' \
  -e 's|@KERNELPATH_[A-Z_]*@|/usr/share/kata-containers/vmlinux.container|g' \
  -e 's|@IMAGEPATH@|/usr/share/kata-containers/kata-containers.img|g' \
  -e 's|@INITRDPATH@||g' \
  -e 's|@FIRMWAREPATH@||g' \
  -e 's|@FIRMWAREVOLUMEPATH@||g' \
  -e 's|@DEFNETWORKMODEL_[A-Z_]*@|tcfilter|g' \
  -e 's|@DEFSANDBOXCGROUPONLY@|false|g' \
  -e 's|@DEFSTATICRESOURCEMGMT_[A-Z_]*@|false|g' \
  -e 's|@DEFBINDMOUNTS@||g' \
  -e 's|@DEFVFIOMODE@|guest-kernel|g' \
  -e 's|@DEFDISABLEGUESTEMPTYDIR@|false|g' \
  -e 's|@DEFDISABLEGUESTSECCOMP@|true|g' \
  -e 's|@DEFAULTSHAREDFS@|virtio-fs|g' \
  -e 's|@DEFVIRTIOFSDAEMON@|/usr/libexec/virtiofsd|g' \
  -e 's|@DEFVIRTIOFSCACHESIZE@|0|g' \
  -e 's|@DEFVIRTIOFSQUEUESIZE@|1024|g' \
  -e 's|@DEFVIRTIOFSCACHE@|auto|g' \
  -e 's|@DEFENABLEANNOTATIONS@||g' \
  -e 's|@DEFENABLEIOTHREADS@|false|g' \
  -e 's|@DEFENABLEVHOSTUSERSTORE@|false|g' \
  -e 's|@DEFVHOSTUSERSTOREPATH@|/var/run/kata-containers/vhost-user|g' \
  -e 's|@DEFVALIDHYPERVISORPATHS@|["/usr/bin/qemu-system-x86_64"]|g' \
  "$OUTPUT_FILE"

# Verify no placeholders remain
REMAINING=$(grep -c "@.*@" "$OUTPUT_FILE" 2>/dev/null | head -1 || echo "0")
if [ "$REMAINING" != "0" ] && [ -n "$REMAINING" ]; then
    echo "Warning: $REMAINING placeholder(s) still remain in $OUTPUT_FILE"
    grep "@.*@" "$OUTPUT_FILE"
    exit 1
fi

echo "Done! Generated $OUTPUT_FILE successfully."

