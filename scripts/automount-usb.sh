#!/bin/bash
# =====================================================================
# AuraBox USB Auto-Mounter & Instant Library Rescan Trigger
# Diseñado para udev en Raspberry Pi OS / Linux
# =====================================================================

ACTION=$1
DEVNAME=$2
MOUNT_BASE="/music/usb"

if [ -z "$DEVNAME" ]; then
    echo "Uso: $0 [add|remove] /dev/sdX1"
    exit 1
fi

ID_FS_LABEL=$(blkid -s LABEL -o value "$DEVNAME")
if [ -z "$ID_FS_LABEL" ]; then
    ID_FS_LABEL=$(basename "$DEVNAME")
fi

TARGET_DIR="${MOUNT_BASE}/${ID_FS_LABEL}"

case "$ACTION" in
    add)
        echo "[AuraBox USB] Detectada nueva unidad USB: $DEVNAME ($ID_FS_LABEL)"
        mkdir -p "$TARGET_DIR"
        
        # Intentar montaje con soporte para NTFS, exFAT, FAT32 y ext4
        mount -o iocharset=utf8,ro "$DEVNAME" "$TARGET_DIR" 2>/dev/null || \
        mount -o ro "$DEVNAME" "$TARGET_DIR" 2>/dev/null
        
        if [ $? -eq 0 ]; then
            echo "[AuraBox USB] Montado correctamente en $TARGET_DIR"
            # Disparar re-escaneo automático en AuraBox Web
            curl -s -X POST http://localhost:4080/api/library/scan > /dev/null 2>&1
        else
            echo "[AuraBox USB] Error al montar $DEVNAME"
            rmdir "$TARGET_DIR" 2>/dev/null
        fi
        ;;

    remove)
        echo "[AuraBox USB] Desconectando unidad: $DEVNAME"
        umount -l "$TARGET_DIR" 2>/dev/null
        rmdir "$TARGET_DIR" 2>/dev/null
        # Notificar a AuraBox para sincronizar catálogo
        curl -s -X POST http://localhost:4080/api/library/scan > /dev/null 2>&1
        ;;

    *)
        echo "Acción no reconocida: $ACTION"
        exit 1
        ;;
esac

