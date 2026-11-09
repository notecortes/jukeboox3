#!/bin/bash
# =====================================================================
# AuraBox - Script de Instalación Automatizada para Raspberry Pi
# Configuración de Docker, Audio ALSA, Montaje NAS y Modo 24/7
# =====================================================================

set -e

echo "=========================================================="
echo "          AuraBox — Instalador para Raspberry Pi           "
echo "=========================================================="

# 1. Comprobar permisos y usuario
CURRENT_USER=$(whoami)
if [ "$CURRENT_USER" = "root" ]; then
    REAL_USER=${SUDO_USER:-root}
else
    REAL_USER=$CURRENT_USER
fi

echo "[1/6] Actualizando repositorios e instalando paquetes necesarios..."
sudo apt-get update -y
sudo apt-get install -y cifs-utils curl git alsa-utils

# 2. Instalar Docker si no está presente
echo "[2/6] Verificando instalación de Docker..."
if ! command -v docker &> /dev/null; then
    echo "      Instalando Docker Engine..."
    curl -fsSL https://get.docker.com -o /tmp/get-docker.sh
    sudo sh /tmp/get-docker.sh
    rm -f /tmp/get-docker.sh
else
    echo "      Docker ya se encuentra instalado."
fi

# 3. Asignar usuario a grupos docker y audio
echo "[3/6] Configurando permisos de usuario para Docker y dispositivos de sonido..."
sudo usermod -aG docker,audio "$REAL_USER"

# 4. Configurar variables de entorno y NAS
echo "[4/6] Configurando acceso a la unidad de red NAS..."
ENV_FILE=".env"
if [ ! -f "$ENV_FILE" ]; then
    if [ -f ".env.example" ]; then
        cp .env.example "$ENV_FILE"
    else
        cat <<EOF > "$ENV_FILE"
# Configuración de almacenamiento en red NAS (SMB / CIFS)
PORT=4080
ADMIN_PORT=4081
NAS_ALIAS=mi-nas
NAS_HOST=192.168.1.100
NAS_SHARE=music
NAS_USER=tu_usuario_nas
NAS_PASS=tu_contraseña_nas
NAS_OPTIONS=vers=3.0,sec=ntlmssp,ro,iocharset=utf8
PULSE_SOCKET=/run/user/1000/pulse/native
PULSE_COOKIE=/dev/null
EOF
    fi
    echo "      Archivo .env creado desde la plantilla (.env.example). Edítalo con los datos de tu NAS antes de montar."
else
    echo "      Archivo .env existente preservado."
fi

# 5. Configurar regla udev para pendrives USB
echo "[5/6] Configurando regla udev para pendrives USB..."
if [ -f "scripts/automount-usb.sh" ]; then
    sudo cp scripts/automount-usb.sh /usr/local/bin/aurabox-usb
    sudo chmod +x /usr/local/bin/aurabox-usb
    
    cat << 'EOF' | sudo tee /etc/udev/rules.d/99-aurabox-usb.rules > /dev/null
ACTION=="add", SUBSYSTEM=="block", KERNEL=="sd[a-z][0-9]", RUN+="/usr/local/bin/aurabox-usb add /dev/%k"
ACTION=="remove", SUBSYSTEM=="block", KERNEL=="sd[a-z][0-9]", RUN+="/usr/local/bin/aurabox-usb remove /dev/%k"
EOF
    sudo udevadm control --reload-rules || true
fi

# 6. Despliegue de los contenedores Docker (secuencial para no saturar RAM/microSD)
echo "[6/6] Compilando e iniciando AuraBox con Docker Compose..."
sudo COMPOSE_PARALLEL_LIMIT=1 docker compose up -d --build

# Obtener dirección IP local
LOCAL_IP=$(hostname -I 2>/dev/null | awk '{print $1}')
if [ -z "$LOCAL_IP" ]; then
    LOCAL_IP="<IP_DE_TU_RASPBERRY_PI>"
fi

echo ""
echo "=========================================================="
echo "          ¡AuraBox Instalado Correctamente!               "
echo "=========================================================="
echo ""
echo "🎵 Accede desde tu móvil o PC conectado al mismo WiFi:"
echo "   ▶ http://${LOCAL_IP}:4080"
echo ""
echo "📱 Pasos finales en tu smartphone:"
echo "   1. Abre http://${LOCAL_IP}:4080 en el navegador."
echo "   2. Toca 'Ajustes' en el menú inferior y selecciona la salida"
echo "      de tu amplificador (DAC USB o Jack analógico 3.5mm)."
echo "   3. Pulsa 'Añadir a pantalla de inicio' para tener la App nativa."
echo ""
echo "=========================================================="

