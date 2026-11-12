#!/bin/bash
# =====================================================================
# AuraBox USB Auto-Mounter & Instant Library Rescan Trigger
# Diseñado para udev en Raspberry Pi OS / Linux
# =====================================================================

ACTION="${1:-}"
DEVNAME="${2:-}"
MOUNT_BASE="/music/usb"

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
    code=$(curl -s -o /dev/null -w "%{http_code}" -X POST http://127.0.0.1:4080/api/library/scan 2>/dev/null || true)
    if [ "$code" != "200" ] && [ "$code" != "202" ]; then
        curl -sk -o /dev/null -X POST https://127.0.0.1:4443/api/library/scan >/dev/null 2>&1 || true
    fi
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
        if mount -o iocharset=utf8,ro,nosuid,nodev,noexec "$DEVNAME" "$TARGET_DIR" 2>/dev/null || \
           mount -o ro,nosuid,nodev,noexec "$DEVNAME" "$TARGET_DIR" 2>/dev/null; then
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

