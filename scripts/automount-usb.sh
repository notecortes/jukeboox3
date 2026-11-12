#!/bin/bash
# =====================================================================
# AuraBox USB Auto-Mounter & Instant Library Rescan Trigger
# Diseñado para udev en Raspberry Pi OS / Linux
# =====================================================================

ACTION="${1:-}"
DEVNAME="${2:-}"
CONFIG_FILE="/etc/default/aurabox-usb"
MOUNT_BASE="${AURABOX_USB_MOUNT_BASE:-/music/usb}"
AURABOX_PORT="${AURABOX_PORT:-4080}"
AURABOX_HTTPS_PORT="${AURABOX_HTTPS_PORT:-4443}"

if [ -r "$CONFIG_FILE" ]; then
    CFG_MOUNT=$(awk -F= '$1 == "MOUNT_BASE" { sub(/^MOUNT_BASE=/, ""); gsub(/["'\''\r ]/, ""); print; exit }' "$CONFIG_FILE" 2>/dev/null || true)
    if [ -n "$CFG_MOUNT" ] && [ "${CFG_MOUNT#/}" != "$CFG_MOUNT" ]; then
        MOUNT_BASE="$CFG_MOUNT"
    fi
fi

if [ -z "$DEVNAME" ]; then
    echo "Uso: $0 [add|remove] /dev/sdX1"
    exit 1
fi

case "$DEVNAME" in
    /dev/*) ;;
    *)
        echo "[AuraBox USB] Dispositivo no válido: $DEVNAME"
        exit 1
        ;;
esac

case "$DEVNAME" in
    *..*)
        echo "[AuraBox USB] Ruta de dispositivo no permitida: $DEVNAME"
        exit 1
        ;;
esac

# resolve_safe_target valida que cualquier punto de montaje resida estrictamente bajo /music/usb/.
resolve_safe_target() {
    local candidate="$1"
    local resolved
    resolved=$(realpath -m "$candidate" 2>/dev/null) || return 1
    case "$resolved" in
        "${MOUNT_BASE}/"*)
            printf '%s\n' "$resolved"
            return 0
            ;;
        *)
            return 1
            ;;
    esac
}

# sanitize_label limpia la etiqueta del USB dejando solo caracteres seguros para directorio.
sanitize_label() {
    local raw="$1"
    local clean
    clean=$(printf '%s' "$raw" | tr -cd 'A-Za-z0-9._-' | cut -c1-48)
    if [ -z "$clean" ] || [ "$clean" = "." ] || [ "$clean" = ".." ]; then
        clean=$(basename "$DEVNAME" | tr -cd 'A-Za-z0-9._-' | cut -c1-48)
    fi
    if [ -z "$clean" ] || [ "$clean" = "." ] || [ "$clean" = ".." ]; then
        return 1
    fi
    printf '%s\n' "$clean"
}

# trigger_rescan solicita a AuraBox re-escanear la biblioteca vía HTTP local o fallback HTTPS local.
trigger_rescan() {
    local code
    code=$(curl -s -m 3 -o /dev/null -w "%{http_code}" -X POST "http://127.0.0.1:${AURABOX_PORT}/api/library/scan" 2>/dev/null || true)
    if [ "$code" != "200" ] && [ "$code" != "202" ]; then
        code=$(curl -sk -m 3 -o /dev/null -w "%{http_code}" -X POST "https://127.0.0.1:${AURABOX_HTTPS_PORT}/api/library/scan" 2>/dev/null || true)
    fi
    if [ "$code" != "200" ] && [ "$code" != "202" ]; then
        curl -s -m 3 -o /dev/null -X POST "http://127.0.0.1/api/library/scan" >/dev/null 2>&1 || true
    fi
}

# mount_readonly_usb monta la partición USB en el namespace global (vía systemd-mount si está disponible) en solo lectura.
mount_readonly_usb() {
    local dev="$1"
    local target="$2"
    if command -v systemd-mount >/dev/null 2>&1; then
        if systemd-mount --collect -o iocharset=utf8,ro,nosuid,nodev,noexec "$dev" "$target" 2>/dev/null || \
           systemd-mount --collect -o ro,nosuid,nodev,noexec "$dev" "$target" 2>/dev/null; then
            return 0
        fi
    fi
    mount -o iocharset=utf8,ro,nosuid,nodev,noexec "$dev" "$target" 2>/dev/null || \
    mount -o ro,nosuid,nodev,noexec "$dev" "$target" 2>/dev/null
}

case "$ACTION" in
    add)
        RAW_LABEL=$(blkid -s LABEL -o value "$DEVNAME" 2>/dev/null || true)
        SAFE_LABEL=$(sanitize_label "$RAW_LABEL") || {
            echo "[AuraBox USB] No se pudo derivar un nombre seguro para $DEVNAME"
            exit 1
        }
        TARGET_DIR=$(resolve_safe_target "${MOUNT_BASE}/${SAFE_LABEL}") || {
            echo "[AuraBox USB] Punto de montaje fuera de ${MOUNT_BASE}"
            exit 1
        }

        echo "[AuraBox USB] Detectada nueva unidad USB: $DEVNAME ($SAFE_LABEL)"
        mkdir -p "$TARGET_DIR"

        # Montaje de solo lectura y sin ejecución/suid/dispositivos para proteger el host
        if mount_readonly_usb "$DEVNAME" "$TARGET_DIR"; then
            echo "[AuraBox USB] Montado correctamente en $TARGET_DIR"
            trigger_rescan
        else
            echo "[AuraBox USB] Error al montar $DEVNAME"
            rmdir "$TARGET_DIR" 2>/dev/null || true
            exit 1
        fi
        ;;

    remove)
        echo "[AuraBox USB] Desconectando unidad: $DEVNAME"
        MOUNTED_TARGET=$(awk -v dev="$DEVNAME" -v base="${MOUNT_BASE}/" '$1 == dev && index($2, base) == 1 { print $2; exit }' /proc/mounts 2>/dev/null || true)
        if [ -z "$MOUNTED_TARGET" ]; then
            FALLBACK_LABEL=$(sanitize_label "" 2>/dev/null || true)
            if [ -n "$FALLBACK_LABEL" ]; then
                MOUNTED_TARGET="${MOUNT_BASE}/${FALLBACK_LABEL}"
            fi
        fi

        if [ -n "$MOUNTED_TARGET" ] && SAFE_UNMOUNT=$(resolve_safe_target "$MOUNTED_TARGET"); then
            if mountpoint -q "$SAFE_UNMOUNT" 2>/dev/null || grep -qsF " ${SAFE_UNMOUNT} " /proc/mounts 2>/dev/null; then
                umount -l "$SAFE_UNMOUNT" 2>/dev/null || true
            fi
            rmdir "$SAFE_UNMOUNT" 2>/dev/null || true
        fi
        trigger_rescan
        ;;

    *)
        echo "Acción no reconocida: $ACTION"
        exit 1
        ;;
esac

