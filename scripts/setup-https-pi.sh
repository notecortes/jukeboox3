#!/bin/bash
# =====================================================================
# AuraBox - Activación de HTTPS y Redirección Automática de IP (80/443)
# Mantiene docker-compose.yml estrictamente en el rango 4000 (4080/4443)
# y redirige las peticiones a la IP sin puerto hacia https://<IP>.
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

if [ "${1:-}" = "--disable" ]; then
    echo "Desactivando redirección de puertos 80/443 en systemd..."
    sudo systemctl disable --now aurabox-port80.socket aurabox-port443.socket 2>/dev/null || true
    sudo systemctl stop aurabox-port80.service aurabox-port443.service 2>/dev/null || true
    sudo rm -f /etc/systemd/system/aurabox-port80.* /etc/systemd/system/aurabox-port443.*
    sudo systemctl daemon-reload
    if [ -f .env ]; then
        upsert_env .env "FORCE_HTTPS" "false"
        upsert_env .env "HTTPS_PUBLIC_PORT" "4443"
    fi
    sudo COMPOSE_PARALLEL_LIMIT=1 docker compose up -d aurabox
    echo "Redirección 80/443 eliminada correctamente. Acceso HTTP en :4080 y HTTPS en :4443."
    exit 0
fi

if [ ! -f "docker-compose.yml" ]; then
    echo "Error: ejecuta este script desde el repositorio de AuraBox."
    exit 1
fi

PROXYD_BIN=""
for candidate in /lib/systemd/systemd-socket-proxyd /usr/lib/systemd/systemd-socket-proxyd; do
    if [ -x "$candidate" ]; then
        PROXYD_BIN="$candidate"
        break
    fi
done

if [ -z "$PROXYD_BIN" ]; then
    echo "Error: no se encontró systemd-socket-proxyd en este sistema."
    exit 1
fi

# Comprobar que ningún otro servidor web (Apache/Nginx/Caddy) ocupa ya el 80 o 443
if command -v ss >/dev/null 2>&1; then
    CONFLICTS=$(sudo ss -tlnpH 'sport = :80 or sport = :443' 2>/dev/null | grep -v 'systemd' || true)
    if [ -n "$CONFLICTS" ]; then
        echo "AVISO: Los puertos 80 o 443 ya están ocupados por otro proceso:"
        echo "$CONFLICTS"
        echo "Libera los puertos 80/443 o configura un reverse proxy manual hacia 127.0.0.1:4443."
        exit 1
    fi
fi

# Preparar .env con la IP LAN y dominio .local reales de la Raspberry Pi
if [ ! -f ".env" ]; then
    cp .env.example .env
fi

LAN_IP=$(hostname -I 2>/dev/null | awk '{print $1}')
HOST_LOCAL="$(hostname 2>/dev/null || echo aurabox).local"
EXTRA_HOSTS="$HOST_LOCAL,aurabox.local"
if [ -n "$LAN_IP" ]; then
    EXTRA_HOSTS="${LAN_IP},${EXTRA_HOSTS}"
fi

upsert_env ".env" "HTTPS_PORT" "4443"
upsert_env ".env" "HTTPS_PUBLIC_PORT" "443"
upsert_env ".env" "FORCE_HTTPS" "true"
upsert_env ".env" "TLS_EXTRA_HOSTS" "$EXTRA_HOSTS"

echo "[1/4] Configurando unidades systemd (80 -> 4080 y 443 -> 4443)..."
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
ExecStart=${PROXYD_BIN} --exit-idle-time=10m 127.0.0.1:4080

[Install]
WantedBy=multi-user.target
EOF

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
ExecStart=${PROXYD_BIN} --exit-idle-time=10m 127.0.0.1:4443

[Install]
WantedBy=multi-user.target
EOF

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
for _ in $(seq 1 20); do
    if curl -sf http://127.0.0.1:4081/health >/dev/null 2>&1; then
        break
    fi
    sleep 1
done

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

