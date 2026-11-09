#!/bin/sh
# =====================================================================
# AuraBox MPD Entrypoint — Autodetección de salidas ALSA / Pulse / HTTP
# =====================================================================
set -e

mkdir -p /run/mpd /var/lib/mpd/playlists
chmod 755 /run/mpd
touch /var/lib/mpd/state /var/lib/mpd/sticker.sql

# Enlace simbólico fuera de music_directory para permitir reproducción inmediata
# vía URI file:///rawmusic/... por socket Unix mientras MPD termina de indexar.
ln -sfn /music /rawmusic

CONF_OUT="/tmp/mpd.runtime.conf"
ENABLE_HTTPD="yes"

cat << 'EOF' > "$CONF_OUT"
# =====================================================================
# AuraBox MPD Runtime Config (Generado automáticamente al arranque)
# =====================================================================
music_directory         "/music"
playlist_directory      "/var/lib/mpd/playlists"
db_file                 "/var/lib/mpd/database"
log_file                "/var/lib/mpd/log"
pid_file                "/var/lib/mpd/pid"
state_file              "/var/lib/mpd/state"
sticker_file            "/var/lib/mpd/sticker.sql"

user                    "root"
bind_to_address         "/run/mpd/socket"
bind_to_address         "0.0.0.0"
port                    "6600"
log_level               "default"
auto_update             "yes"
auto_update_depth       "10"

audio_buffer_size       "4096"
buffer_before_play      "20%"
max_output_buffer_size  "16384"
connection_timeout      "300"
max_connections         "30"

replaygain              "auto"
replaygain_preamp       "0"
volume_normalization    "yes"

EOF

HAS_PULSE=0
if [ -S /tmp/pulse-socket ]; then
    HAS_PULSE=1
    cat << 'EOF' >> "$CONF_OUT"
audio_output {
    type        "pulse"
    name        "Altavoces del Sistema (PipeWire / PulseAudio)"
    server      "unix:/tmp/pulse-socket"
    mixer_type  "software"
    enabled     "yes"
}

EOF
fi

# Descubrir dispositivos ALSA reales mediante ioctls de /dev/snd (aplay -l)
TMP_CARDS="/tmp/aplay_cards.txt"
LC_ALL=C aplay -l 2>/dev/null | grep '^card ' > "$TMP_CARDS" || true

# Determinar qué tipo de salida física debe quedar activa por defecto si no hay PulseAudio:
# Prioridad: 1) USB / I2S HAT -> 2) Jack 3.5mm / Analógico -> 3) Digital / HDMI
DEFAULT_CLASS=""
if [ "$HAS_PULSE" -eq 0 ] && [ -s "$TMP_CARDS" ]; then
    if grep -qiE 'usb|dac|topping|fiio|smsl|focusrite|scarlett|dragonfly|behringer|hifiberry|allo|iqaudio|justboom|pisound|dacberry|pcm51|es90|sndrpi' "$TMP_CARDS"; then
        DEFAULT_CLASS="usb_or_hat"
    elif grep -qiE 'headphone|bcm2835|analog|alc|pch|realtek|built-in|speaker' "$TMP_CARDS"; then
        DEFAULT_CLASS="analog"
    else
        DEFAULT_CLASS="any"
    fi
fi

ADDED_ALSA=0
ENABLED_ONE=0
SEEN_HDMI_CARD=""

while IFS= read -r line; do
    # Ejemplo de línea: card 0: PCH [HDA Intel PCH], device 0: ALC897 Analog [ALC897 Analog]
    CARD_NUM=$(printf '%s' "$line" | sed -n 's/^card \([0-9][0-9]*\):.*/\1/p')
    CARD_ID=$(printf '%s' "$line" | sed -n 's/^card [0-9][0-9]*: \([^ ]*\) \[.*/\1/p')
    CARD_NAME=$(printf '%s' "$line" | sed -n 's/^card [0-9][0-9]*: [^ ]* \[\([^]]*\)\], device.*/\1/p')
    DEV_NUM=$(printf '%s' "$line" | sed -n 's/.*, device \([0-9][0-9]*\):.*/\1/p')
    DEV_NAME=$(printf '%s' "$line" | sed -n 's/.*, device [0-9][0-9]*: [^[]*\[\([^]]*\)\].*/\1/p')

    [ -z "$CARD_ID" ] && continue
    [ -z "$DEV_NUM" ] && DEV_NUM="0"
    [ -z "$CARD_NAME" ] && CARD_NAME="$CARD_ID"

    LOWER=$(printf '%s %s %s' "$CARD_ID" "$CARD_NAME" "$DEV_NAME" | tr 'A-Z' 'a-z')

    KIND="other"
    DISPLAY_NAME=""
    case "$LOWER" in
        *hifiberry*|*allo*|*iqaudio*|*justboom*|*pisound*|*dacberry*|*wm8960*|*sndrpi*|*pcm51*|*es90*|*boss*dac*)
            KIND="usb_or_hat"
            DISPLAY_NAME="DAC I2S HAT (${CARD_NAME})"
            ;;
        *"usb audio"*|*usb*|*dac*|*topping*|*fiio*|*smsl*|*focusrite*|*scarlett*|*dragonfly*|*behringer*|*khadas*|*chord*|*schiit*)
            KIND="usb_or_hat"
            DISPLAY_NAME="DAC USB (${CARD_NAME})"
            ;;
        *headphone*|*bcm2835*|*analog*|*built-in*|*speaker*)
            KIND="analog"
            DISPLAY_NAME="Jack 3.5mm / Analógico (${CARD_NAME})"
            ;;
        *hdmi*|*vc4hdmi*|*vc4-hdmi*)
            # Evitar listar múltiples puertos HDMI vacíos en la misma tarjeta
            case " $SEEN_HDMI_CARD " in
                *" $CARD_NUM "*) continue ;;
            esac
            SEEN_HDMI_CARD="$SEEN_HDMI_CARD $CARD_NUM"
            KIND="hdmi"
            DISPLAY_NAME="HDMI Audio (${CARD_NAME})"
            ;;
        *digital*|*spdif*|*iec958*)
            KIND="digital"
            DISPLAY_NAME="Salida Digital S/PDIF (${CARD_NAME})"
            ;;
        *)
            KIND="other"
            DISPLAY_NAME="Salida ALSA (${CARD_NAME})"
            ;;
    esac

    OUT_ENABLED="no"
    if [ "$HAS_PULSE" -eq 0 ] && [ "$ENABLED_ONE" -eq 0 ]; then
        if [ "$DEFAULT_CLASS" = "usb_or_hat" ] && [ "$KIND" = "usb_or_hat" ]; then
            OUT_ENABLED="yes"
            ENABLED_ONE=1
        elif [ "$DEFAULT_CLASS" = "analog" ] && [ "$KIND" = "analog" ]; then
            OUT_ENABLED="yes"
            ENABLED_ONE=1
        elif [ "$DEFAULT_CLASS" = "any" ]; then
            OUT_ENABLED="yes"
            ENABLED_ONE=1
        fi
    fi

    cat << EOF >> "$CONF_OUT"
audio_output {
    type        "alsa"
    name        "${DISPLAY_NAME}"
    device      "plughw:CARD=${CARD_ID},DEV=${DEV_NUM}"
    mixer_type  "software"
    enabled     "${OUT_ENABLED}"
}

EOF
    ADDED_ALSA=$((ADDED_ALSA + 1))
done < "$TMP_CARDS"
rm -f "$TMP_CARDS"

# Si no hay ningún hardware de audio físico ni PulseAudio, agregar salida virtual para que httpd funcione
if [ "$HAS_PULSE" -eq 0 ] && [ "$ADDED_ALSA" -eq 0 ]; then
    cat << 'EOF' >> "$CONF_OUT"
audio_output {
    type        "null"
    name        "Salida Virtual (Sin DAC/Altavoces detectados)"
    mixer_type  "software"
    enabled     "yes"
}

EOF
fi

cat << EOF >> "$CONF_OUT"
audio_output {
    type            "httpd"
    name            "Transmisión Web HTTP (Hi-Fi Stream)"
    encoder         "lame"
    port            "4082"
    bind_to_address "0.0.0.0"
    bitrate         "320"
    format          "44100:16:2"
    max_clients     "10"
    always_on       "yes"
    tags            "no"
    enabled         "${ENABLE_HTTPD}"
}
EOF

echo "[AuraBox MPD] Configuración generada ($ADDED_ALSA dispositivos ALSA detectados, PulseAudio=$HAS_PULSE)"
exec mpd --no-daemon --stdout "$CONF_OUT"
