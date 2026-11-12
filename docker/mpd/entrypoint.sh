#!/bin/sh
# =====================================================================
# AuraBox MPD Entrypoint — Autodetección dinámica ALSA / Pulse / HTTP
# =====================================================================
set -e

mkdir -p /run/mpd /var/lib/mpd/playlists
chmod 755 /run/mpd
touch /var/lib/mpd/state /var/lib/mpd/sticker.sql

# Enlace simbólico fuera de music_directory para permitir reproducción inmediata
# vía URI file:///rawmusic/... por socket Unix mientras MPD termina de indexar.
ln -sfn /music /rawmusic

CONF_OUT="/tmp/mpd.runtime.conf"
RELOAD_FLAG="/run/mpd/reload_outputs"
ALSA_DIAG_FILE="/run/mpd/alsa_cards.txt"
DSP_CONFIG_FILE="/run/mpd/dsp_config"
ENABLE_HTTPD="yes"
LAST_CARDS_SIG=""
MPD_PID=""
STOP_REQUESTED=0

# sanitize_mpd_value elimina comillas, barras invertidas y saltos de línea para evitar inyección en mpd.conf.
sanitize_mpd_value() {
    printf '%s' "$1" | tr -d '"\r\n\\'
}

# read_preferred_active lee de /run/mpd/dsp_config la salida seleccionada por el usuario si existe.
read_preferred_active() {
    [ -f "$DSP_CONFIG_FILE" ] || return 0
    awk -F= '$1 == "ACTIVE" { sub(/^ACTIVE=/, ""); print; exit }' "$DSP_CONFIG_FILE" 2>/dev/null || true
}

# read_dsp_preset consulta el perfil DSP asignado a una salida concreta en /run/mpd/dsp_config.
read_dsp_preset() {
    TARGET_OUT="$1"
    [ -f "$DSP_CONFIG_FILE" ] || { printf 'flat\n'; return 0; }
    PRESET_VAL=$(awk -F '\t' -v target="$TARGET_OUT" '$1 == "DSP" && $2 == target { print $3; exit }' "$DSP_CONFIG_FILE" 2>/dev/null || true)
    case "$PRESET_VAL" in
        night|voice|livingroom|bass|flat)
            printf '%s\n' "$PRESET_VAL"
            ;;
        eq:*)
            if printf '%s' "$PRESET_VAL" | grep -Eq '^eq:-?[0-9]{1,2}(,-?[0-9]{1,2}){4}$'; then
                printf '%s\n' "$PRESET_VAL"
            else
                printf 'flat\n'
            fi
            ;;
        *)
            printf 'flat\n'
            ;;
    esac
}

# parse_aplay_cards extrae tarjetas y dispositivos ALSA reales tolerando espacios o símbolos en el ID.
parse_aplay_cards() {
    awk '
    /^card [0-9]+: / {
        line = $0
        sub(/^card /, "", line)
        cnum = line
        sub(/:.*/, "", cnum)
        sub(/^[0-9]+: /, "", line)

        pos = index(line, "], device ")
        if (pos == 0) next
        left = substr(line, 1, pos - 1)
        right = substr(line, pos + 10)

        bpos = 0
        for (i = length(left) - 1; i >= 1; i--) {
            if (substr(left, i, 2) == " [") {
                bpos = i
                break
            }
        }
        if (bpos > 0) {
            cid = substr(left, 1, bpos - 1)
            cname = substr(left, bpos + 2)
        } else {
            cid = left
            cname = left
        }
        gsub(/^ +| +$/, "", cid)
        gsub(/^ +| +$/, "", cname)

        dnum = right
        sub(/:.*/, "", dnum)
        dname = ""
        dpcm = right
        sub(/^[0-9]+: */, "", dpcm)
        dbpos = index(right, "[")
        depos = index(right, "]")
        if (dbpos > 0 && depos > dbpos) {
            dname = substr(right, dbpos + 1, depos - dbpos - 1)
        }
        dpcmp = index(dpcm, " [")
        if (dpcmp > 0) {
            dpcm = substr(dpcm, 1, dpcmp - 1)
        }

        gsub(/\t/, " ", cid)
        gsub(/\t/, " ", cname)
        gsub(/\t/, " ", dname)
        gsub(/\t/, " ", dpcm)
        print cnum "\t" cid "\t" cname "\t" dnum "\t" dname "\t" dpcm
    }
    '
}

# generate_conf escanea /dev/snd mediante aplay -l y genera /tmp/mpd.runtime.conf actualizado.
generate_conf() {
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
zeroconf_enabled        "no"
auto_update             "no"

audio_buffer_size       "8192"
buffer_before_play      "10%"
max_output_buffer_size  "32768"
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

    RAW_APLAY=$(LC_ALL=C aplay -l 2>&1 || true)
    printf '%s\n' "$RAW_APLAY" > "$ALSA_DIAG_FILE"
    chmod 644 "$ALSA_DIAG_FILE" 2>/dev/null || true

    TMP_CARDS="/tmp/aplay_cards.tsv"
    printf '%s\n' "$RAW_APLAY" | grep '^card ' | parse_aplay_cards > "$TMP_CARDS" || true
    LAST_CARDS_SIG=$(printf '%s\n' "$RAW_APLAY" | grep '^card ' || true)

    # Determinar salida física preferida si no hay PulseAudio:
    # Prioridad: 1) DAC USB / I2S HAT -> 2) Jack 3.5mm / Analógico -> 3) Digital / HDMI
    DEFAULT_CLASS=""
    if [ "$HAS_PULSE" -eq 0 ] && [ -s "$TMP_CARDS" ]; then
        if grep -viE 'hdmi|vc4hdmi|vc4-hdmi' "$TMP_CARDS" | grep -qiE 'usb|dac|codec|uac|xmos|amanero|sabre|es90|ak44|cs43|pcm2|pcm5|pcm17|cm10|burr|topping|fiio|smsl|(^|[^a-z0-9])ifi([^a-z0-9]|$)|focusrite|scarlett|audient|dragonfly|behringer|khadas|chord|schiit|cambridge|hifiberry|allo|iqaudio|justboom|pisound|dacberry|innomaker|rpi-dac|boss|wm8960|sndrpi|snd_rpi'; then
            DEFAULT_CLASS="usb_or_hat"
        elif grep -qiE 'headphone|bcm2835|analog|alc|pch|realtek|built-in|speaker' "$TMP_CARDS"; then
            DEFAULT_CLASS="analog"
        else
            DEFAULT_CLASS="any"
        fi
    fi

    TMP_OUTPUTS="/tmp/alsa_outputs.tsv"
    : > "$TMP_OUTPUTS"
    SEEN_HDMI_CARD=""
    TAB=$(printf '\t')
    PREFERRED_ACTIVE=$(sanitize_mpd_value "$(read_preferred_active)")
    HAS_PREFERRED_MATCH=0

    while IFS="$TAB" read -r CARD_NUM CARD_ID CARD_NAME DEV_NUM DEV_NAME DEV_PCM; do
        CARD_NUM=$(printf '%s' "$CARD_NUM" | tr -cd '0-9')
        DEV_NUM=$(printf '%s' "$DEV_NUM" | tr -cd '0-9')
        [ -z "$CARD_NUM" ] && continue
        [ -z "$DEV_NUM" ] && DEV_NUM="0"
        CARD_ID=$(sanitize_mpd_value "$CARD_ID")
        CARD_NAME=$(sanitize_mpd_value "$CARD_NAME")
        DEV_NAME=$(sanitize_mpd_value "$DEV_NAME")
        [ -z "$CARD_NAME" ] && CARD_NAME="$CARD_ID"
        [ -z "$CARD_NAME" ] && CARD_NAME="Tarjeta $CARD_NUM"

        LOWER=$(printf '%s %s %s %s' "$CARD_ID" "$CARD_NAME" "$DEV_NAME" "$DEV_PCM" | tr 'A-Z' 'a-z')

        KIND="other"
        DISPLAY_NAME=""
        case "$LOWER" in
            *hifiberry*|*allo*|*iqaudio*|*justboom*|*pisound*|*dacberry*|*innomaker*|*wm8960*|*sndrpi*|*snd_rpi*|*rpi-dac*|*pcm51*|*pcm17*|*boss*dac*|*boss*)
                KIND="usb_or_hat"
                DISPLAY_NAME="DAC I2S HAT (${CARD_NAME})"
                ;;
            *hdmi*|*vc4hdmi*|*vc4-hdmi*)
                # Evitar listar múltiples puertos HDMI vacíos en la misma tarjeta (y evitar falsos positivos como i2s-hifi-0)
                case " $SEEN_HDMI_CARD " in
                    *" $CARD_NUM "*) continue ;;
                esac
                SEEN_HDMI_CARD="$SEEN_HDMI_CARD $CARD_NUM"
                KIND="hdmi"
                DISPLAY_NAME="HDMI Audio (${CARD_NAME})"
                ;;
            *"usb audio"*|*usb*|*dac*|*codec*|*uac*|*xmos*|*amanero*|*sabre*|*es90*|*ak44*|*cs43*|*pcm2*|*cm10*|*burr*|*topping*|*fiio*|*smsl*|*" ifi"*|ifi-*|*"zen dac"*|*focusrite*|*scarlett*|*audient*|*dragonfly*|*behringer*|*khadas*|*chord*|*schiit*|*cambridge*)
                KIND="usb_or_hat"
                DISPLAY_NAME="DAC USB (${CARD_NAME})"
                ;;
            *headphone*|*bcm2835*|*analog*|*built-in*|*speaker*)
                KIND="analog"
                DISPLAY_NAME="Jack 3.5mm / Analógico (${CARD_NAME})"
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

        if [ "$DEV_NUM" != "0" ] && [ -n "$DEV_NAME" ] && [ "$DEV_NAME" != "$CARD_NAME" ] && [ "$KIND" != "hdmi" ]; then
            DISPLAY_NAME="${DISPLAY_NAME} - ${DEV_NAME}"
        fi
        DISPLAY_NAME=$(sanitize_mpd_value "$DISPLAY_NAME")

        # Usar nombre ALSA si es identificador seguro; si contiene espacios/símbolos usar índice numérico estable
        case "$CARD_ID" in
            *[!A-Za-z0-9_.-]*|"")
                ALSA_DEV="plughw:${CARD_NUM},${DEV_NUM}"
                ;;
            *)
                ALSA_DEV="plughw:CARD=${CARD_ID},DEV=${DEV_NUM}"
                ;;
        esac

        if [ -n "$PREFERRED_ACTIVE" ] && [ "$DISPLAY_NAME" = "$PREFERRED_ACTIVE" ]; then
            HAS_PREFERRED_MATCH=1
        fi
        printf '%s\t%s\t%s\n' "$KIND" "$DISPLAY_NAME" "$ALSA_DEV" >> "$TMP_OUTPUTS"
    done < "$TMP_CARDS"
    rm -f "$TMP_CARDS"

    ADDED_ALSA=0
    ENABLED_ONE=0
    while IFS="$TAB" read -r KIND DISPLAY_NAME ALSA_DEV; do
        OUT_ENABLED="no"
        if [ "$HAS_PULSE" -eq 0 ] && [ "$ENABLED_ONE" -eq 0 ]; then
            if [ "$HAS_PREFERRED_MATCH" -eq 1 ]; then
                if [ "$DISPLAY_NAME" = "$PREFERRED_ACTIVE" ]; then
                    OUT_ENABLED="yes"
                    ENABLED_ONE=1
                fi
            elif [ "$DEFAULT_CLASS" = "usb_or_hat" ] && [ "$KIND" = "usb_or_hat" ]; then
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

        DSP_PRESET=$(read_dsp_preset "$DISPLAY_NAME")
        SAFE_PIPE_DEV="no"
        case "$ALSA_DEV" in
            plughw:*[!A-Za-z0-9_.,=-]*) SAFE_PIPE_DEV="no" ;;
            plughw:*) SAFE_PIPE_DEV="yes" ;;
        esac

        if [ "$DSP_PRESET" != "flat" ] && [ "$SAFE_PIPE_DEV" = "yes" ] && command -v sox >/dev/null 2>&1; then
            cat << EOF >> "$CONF_OUT"
audio_output {
    type        "pipe"
    name        "${DISPLAY_NAME}"
    command     "/usr/local/bin/aurabox-dsp-pipe '${ALSA_DEV}' '${DSP_PRESET}'"
    format      "48000:32:2"
    mixer_type  "software"
    enabled     "${OUT_ENABLED}"
}

EOF
        else
            cat << EOF >> "$CONF_OUT"
audio_output {
    type        "alsa"
    name        "${DISPLAY_NAME}"
    device      "${ALSA_DEV}"
    mixer_type  "software"
    enabled     "${OUT_ENABLED}"
}

EOF
        fi
        ADDED_ALSA=$((ADDED_ALSA + 1))
    done < "$TMP_OUTPUTS"
    rm -f "$TMP_OUTPUTS"

    # Si no hay hardware físico ni PulseAudio, agregar salida nula para mantener el stream HTTP operativo
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
}

# start_mpd lanza MPD en segundo plano supervisado para permitir recarga en caliente de DACs.
start_mpd() {
    generate_conf
    mpd --no-daemon --stdout "$CONF_OUT" &
    MPD_PID=$!
}

# stop_mpd detiene MPD limpiamente guardando su archivo de estado antes de reiniciar o salir.
stop_mpd() {
    if [ -n "$MPD_PID" ] && kill -0 "$MPD_PID" 2>/dev/null; then
        kill -TERM "$MPD_PID" 2>/dev/null || true
        wait "$MPD_PID" 2>/dev/null || true
    fi
    MPD_PID=""
}

trap 'STOP_REQUESTED=1; stop_mpd; exit 0' TERM INT

rm -f "$RELOAD_FLAG"
start_mpd

while [ "$STOP_REQUESTED" -eq 0 ]; do
    sleep 2 &
    SLEEP_PID=$!
    wait "$SLEEP_PID" 2>/dev/null || true
    [ "$STOP_REQUESTED" -eq 1 ] && break

    if [ -f "$RELOAD_FLAG" ]; then
        echo "[AuraBox MPD] Recargando salidas de audio bajo demanda..."
        rm -f "$RELOAD_FLAG"
        stop_mpd
        start_mpd
        continue
    fi

    if [ -z "$MPD_PID" ] || ! kill -0 "$MPD_PID" 2>/dev/null; then
        echo "[AuraBox MPD] Proceso MPD finalizado inesperadamente; reiniciando..."
        sleep 1
        start_mpd
        continue
    fi

    # Detectar conexión o encendido posterior de DACs USB sin reiniciar el contenedor
    CUR_RAW=$(LC_ALL=C aplay -l 2>&1 || true)
    CUR_SIG=$(printf '%s\n' "$CUR_RAW" | grep '^card ' || true)
    if [ "$CUR_SIG" != "$LAST_CARDS_SIG" ]; then
        if [ -z "$CUR_SIG" ] && [ -n "$LAST_CARDS_SIG" ]; then
            sleep 1
            CUR_RAW=$(LC_ALL=C aplay -l 2>&1 || true)
            CUR_SIG=$(printf '%s\n' "$CUR_RAW" | grep '^card ' || true)
            [ "$CUR_SIG" = "$LAST_CARDS_SIG" ] && continue
        fi
        printf '%s\n' "$CUR_RAW" > "$ALSA_DIAG_FILE"
        chmod 644 "$ALSA_DIAG_FILE" 2>/dev/null || true
        if ! MPD_HOST=/run/mpd/socket mpc status 2>/dev/null | grep -q '\[playing\]'; then
            echo "[AuraBox MPD] Cambio detectado en hardware ALSA en reposo; recargando salidas..."
            stop_mpd
            start_mpd
        else
            LAST_CARDS_SIG="$CUR_SIG"
            echo "[AuraBox MPD] Cambio ALSA detectado mientras suena música (pulsa Redetectar DAC para aplicar)."
        fi
    fi
done
