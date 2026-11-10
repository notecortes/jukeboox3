#!/bin/bash
# =====================================================================
# AuraBox - Gestión de Puertos Directos (80/443) y HTTPS en Raspberry Pi
# Mantiene docker-compose.yml estrictamente en el rango 4000 (4080/4443)
# y expone puertos 80/443 en el host mediante systemd-socket-proxyd.
#
# Modos:
#   ./scripts/setup-https-pi.sh            -> Activa HTTPS (80 -> 443)
#   ./scripts/setup-https-pi.sh --http     -> Solo HTTP directo por IP (80 -> 4080)
#   ./scripts/setup-https-pi.sh --disable  -> Quita 80/443 y vuelve a :4080 / :4443
# =====================================================================

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
cd "$REPO_DIR"

# upsert_env actualiza o añade una clave en .env preservando el resto de credenciales.
upsert_env() {
    local file="$1"
    local key="$2"
    local val="$3"
    if grep -qE "^${key}=" "$file" 2>/dev/null; then
        awk -v k="$key" -v v="$val" 'BEGIN{FS=OFS="="} $1==k {$0=k"="v} {print}' "$file" > "${file}.tmp"
        mv "${file}.tmp" "$file"
    else
        printf '%s=%s\n' "$key" "$val" >> "$file"
    fi
}

# show_usage muestra las opciones soportadas por el asistente de puertos y TLS.
show_usage() {
    cat << 'EOF'
Uso: ./scripts/setup-https-pi.sh [OPCIÓN]

Opciones:
  (sin opción)  Activa HTTPS en puerto 443 y redirige puerto 80 hacia HTTPS.
  --http        Activa solo HTTP en puerto 80 (http://<IP> -> 127.0.0.1:4080)
                sin redirecciones a HTTPS ni avisos de certificado.
  --disable     Elimina la redirección de puertos 80 y 443 y vuelve a
                http://<IP>:4080 y https://<IP>:4443.
  -h, --help    Muestra esta ayuda.
EOF
}

# find_proxyd_bin localiza el binario nativo systemd-socket-proxyd en Debian/Raspberry Pi OS.
find_proxyd_bin() {
    local candidate
    for candidate in /lib/systemd/systemd-socket-proxyd /usr/lib/systemd/systemd-socket-proxyd; do
        if [ -x "$candidate" ]; then
            echo "$candidate"
            return 0
        fi
    done
    return 1
}

# check_port_conflicts verifica que ningún otro servidor web ocupe los puertos indicados.
check_port_conflicts() {
    local filter="$1"
    if command -v ss >/dev/null 2>&1; then
        local conflicts
        conflicts=$(sudo ss -tlnpH "$filter" 2>/dev/null | grep -v 'systemd' || true)
        if [ -n "$conflicts" ]; then
            echo "AVISO: Hay puertos ya ocupados por otro proceso:"
            echo "$conflicts"
            echo "Libera esos puertos o configura tu reverse proxy manual hacia AuraBox."
            exit 1
        fi
    fi
}

# remove_port_units detiene, deshabilita y elimina las unidades systemd indicadas.
remove_port_units() {
    local unit
    for unit in "$@"; do
        sudo systemctl disable --now "${unit}.socket" 2>/dev/null || true
        sudo systemctl stop "${unit}.service" 2>/dev/null || true
        sudo rm -f "/etc/systemd/system/${unit}.socket" "/etc/systemd/system/${unit}.service"
    done
    sudo systemctl daemon-reload
}

# write_port80_units crea el proxy de systemd del puerto 80 hacia 127.0.0.1:4080.
write_port80_units() {
    local proxyd_bin="$1"

    cat << 'EOF' | sudo tee /etc/systemd/system/aurabox-port80.socket >/dev/null
[Unit]
Description=AuraBox HTTP Port 80 Socket

[Socket]
ListenStream=0.0.0.0:80
FreeBind=yes
ReusePort=yes

[Install]
WantedBy=sockets.target
EOF

    cat << EOF | sudo tee /etc/systemd/system/aurabox-port80.service >/dev/null
[Unit]
Description=AuraBox HTTP Port 80 Forwarder to 4080
Requires=aurabox-port80.socket
After=aurabox-port80.socket docker.service

[Service]
ExecStart=${proxyd_bin} --exit-idle-time=10m 127.0.0.1:4080

[Install]
WantedBy=multi-user.target
EOF
}

# write_port443_units crea el proxy de systemd del puerto 443 hacia 127.0.0.1:4443.
write_port443_units() {
    local proxyd_bin="$1"

    cat << 'EOF' | sudo tee /etc/systemd/system/aurabox-port443.socket >/dev/null
[Unit]
Description=AuraBox HTTPS Port 443 Socket

[Socket]
ListenStream=0.0.0.0:443
FreeBind=yes
ReusePort=yes

[Install]
WantedBy=sockets.target
EOF

    cat << EOF | sudo tee /etc/systemd/system/aurabox-port443.service >/dev/null
[Unit]
Description=AuraBox HTTPS Port 443 Forwarder to 4443
Requires=aurabox-port443.socket
After=aurabox-port443.socket docker.service

[Service]
ExecStart=${proxyd_bin} --exit-idle-time=10m 127.0.0.1:4443

[Install]
WantedBy=multi-user.target
EOF
}

# wait_for_aurabox_health espera hasta 20 segundos a que el panel /health de AuraBox responda OK.
wait_for_aurabox_health() {
    local _
    for _ in $(seq 1 20); do
        if curl -sf http://127.0.0.1:4081/health >/dev/null 2>&1; then
            return 0
        fi
        sleep 1
    done
    return 0
}

MODE="${1:-https}"
if [ "$MODE" = "-h" ] || [ "$MODE" = "--help" ]; then
    show_usage
    exit 0
fi

if [ "$#" -gt 1 ]; then
    echo "Error: se esperaba como máximo un argumento."
    show_usage
    exit 1
fi

if [ ! -f "docker-compose.yml" ]; then
    echo "Error: ejecuta este script desde el repositorio de AuraBox."
    exit 1
fi

LAN_IP=$(hostname -I 2>/dev/null | awk '{print $1}')
HOST_LOCAL="$(hostname 2>/dev/null || echo aurabox).local"
DISPLAY_IP="${LAN_IP:-<IP_RASPBERRY>}"

case "$MODE" in
    --disable)
        echo "Desactivando redirección de puertos 80/443 en systemd..."
        remove_port_units aurabox-port80 aurabox-port443
        if [ -f .env ]; then
            upsert_env .env "HTTPS_PORT" "4443"
            upsert_env .env "HTTPS_PUBLIC_PORT" "4443"
            upsert_env .env "FORCE_HTTPS" "false"
        fi
        sudo COMPOSE_PARALLEL_LIMIT=1 docker compose up -d aurabox
        wait_for_aurabox_health
        echo ""
        echo "=========================================================="
        echo "       Redirección 80/443 desactivada correctamente       "
        echo "=========================================================="
        echo "Los puertos 80 y 443 ya no están en escucha. Entra ahora en:"
        echo "  ▶ HTTP:  http://${DISPLAY_IP}:4080"
        echo "  ▶ HTTPS: https://${DISPLAY_IP}:4443"
        echo ""
        echo "Si quieres entrar escribiendo solo http://${DISPLAY_IP} (sin puerto"
        echo "y sin HTTPS), ejecuta: ./scripts/setup-https-pi.sh --http"
        echo ""
        echo "Nota PWA (Firefox): si antes abriste https://${DISPLAY_IP}/ y"
        echo "ves caché antigua, limpia la web en about:serviceworkers."
        echo "=========================================================="
        exit 0
        ;;
    --http)
        PROXYD_BIN="$(find_proxyd_bin || true)"
        if [ -z "$PROXYD_BIN" ]; then
            echo "Error: no se encontró systemd-socket-proxyd en este sistema."
            exit 1
        fi

        check_port_conflicts 'sport = :80'

        if [ ! -f ".env" ]; then
            cp .env.example .env
        fi

        upsert_env ".env" "HTTPS_PORT" "off"
        upsert_env ".env" "HTTPS_PUBLIC_PORT" "4443"
        upsert_env ".env" "FORCE_HTTPS" "false"

        echo "[1/4] Desactivando puerto 443 y configurando puerto 80 -> 4080..."
        remove_port_units aurabox-port443
        write_port80_units "$PROXYD_BIN"

        echo "[2/4] Activando socket HTTP en puerto 80..."
        sudo systemctl daemon-reload
        sudo systemctl enable --now aurabox-port80.socket

        if command -v ufw >/dev/null 2>&1 && sudo ufw status 2>/dev/null | grep -q "Status: active"; then
            echo "[2b/4] Abriendo puerto 80 en UFW..."
            sudo ufw allow 80/tcp >/dev/null
        fi

        echo "[3/4] Reiniciando AuraBox en modo HTTP directo..."
        sudo COMPOSE_PARALLEL_LIMIT=1 docker compose up -d aurabox

        echo "[4/4] Verificando respuesta HTTP directa por IP..."
        wait_for_aurabox_health
        HTTP_CODE=$(curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1/ || true)

        echo ""
        echo "=========================================================="
        echo "         ¡Acceso Directo HTTP por IP (Puerto 80) Activo! "
        echo "=========================================================="
        echo "Respuesta HTTP (80): HTTP ${HTTP_CODE}"
        echo ""
        echo "Abre directamente en tu navegador (sin puerto ni certificado):"
        echo "  ▶ http://${DISPLAY_IP}"
        echo "  ▶ http://${HOST_LOCAL}"
        echo "  ▶ http://${DISPLAY_IP}:4080"
        echo ""
        echo "Nota (Firefox): asegúrate de usar http:// (no https://) y,"
        echo "si antes instalaste la PWA HTTPS, bórrala en about:serviceworkers."
        echo "=========================================================="
        exit 0
        ;;
    https)
        ;;
    *)
        echo "Error: opción desconocida '$MODE'."
        show_usage
        exit 1
        ;;
esac

PROXYD_BIN="$(find_proxyd_bin || true)"
if [ -z "$PROXYD_BIN" ]; then
    echo "Error: no se encontró systemd-socket-proxyd en este sistema."
    exit 1
fi

check_port_conflicts 'sport = :80 or sport = :443'

# Preparar .env con la IP LAN y dominio .local reales de la Raspberry Pi
if [ ! -f ".env" ]; then
    cp .env.example .env
fi

EXTRA_HOSTS="$HOST_LOCAL,aurabox.local"
if [ -n "$LAN_IP" ]; then
    EXTRA_HOSTS="${LAN_IP},${EXTRA_HOSTS}"
fi

upsert_env ".env" "HTTPS_PORT" "4443"
upsert_env ".env" "HTTPS_PUBLIC_PORT" "443"
upsert_env ".env" "FORCE_HTTPS" "true"
upsert_env ".env" "TLS_EXTRA_HOSTS" "$EXTRA_HOSTS"

echo "[1/4] Configurando unidades systemd (80 -> 4080 y 443 -> 4443)..."
write_port80_units "$PROXYD_BIN"
write_port443_units "$PROXYD_BIN"

echo "[2/4] Activando sockets de redirección automática..."
sudo systemctl daemon-reload
sudo systemctl enable --now aurabox-port80.socket aurabox-port443.socket

if command -v ufw >/dev/null 2>&1 && sudo ufw status 2>/dev/null | grep -q "Status: active"; then
    echo "[2b/4] Abriendo puertos 80 y 443 en UFW..."
    sudo ufw allow 80/tcp >/dev/null
    sudo ufw allow 443/tcp >/dev/null
fi

echo "[3/4] Reconstruyendo e iniciando AuraBox con soporte HTTPS..."
sudo COMPOSE_PARALLEL_LIMIT=1 docker compose up -d --build aurabox

echo "[4/4] Verificando respuesta HTTPS y redirección por IP..."
wait_for_aurabox_health

REDIRECT_CHECK=$(curl -s -o /dev/null -w "%{http_code} -> %{redirect_url}" -H "Host: ${LAN_IP:-127.0.0.1}" http://127.0.0.1/ || true)
HTTPS_CODE=$(curl -sk -o /dev/null -w "%{http_code}" https://127.0.0.1/api/security/status || true)

echo ""
echo "=========================================================="
echo "          ¡HTTPS y Acceso Directo por IP Activos!         "
echo "=========================================================="
echo "Redirección HTTP (80):  ${REDIRECT_CHECK}"
echo "Respuesta HTTPS (443):  HTTP ${HTTPS_CODE}"
echo ""
if [ -n "$LAN_IP" ]; then
    echo "Abre directamente en tu navegador o móvil (sin poner puerto):"
    echo "  ▶ https://${LAN_IP}"
    echo "  ▶ https://${HOST_LOCAL}"
    echo ""
    echo "Para instalar la CA local (evitar el aviso de certificado en el navegador):"
    echo "  ▶ https://${LAN_IP}/api/security/ca.crt"
fi
echo "=========================================================="

