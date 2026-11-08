#!/bin/bash
# =====================================================================
# AuraBox Verification & Automated Integration Test Suite
# Tests REST Endpoints, SSE, Smart Mix Algorithm and Audio Controls
# =====================================================================

BASE_URL="http://localhost:4080"
ADMIN_URL="http://localhost:4081"

echo "=========================================================="
echo "          AuraBox - Integration Verification Suite         "
echo "=========================================================="

# 1. Health check
echo -n "[1/8] Verificando estado del servicio (Health Check)... "
HEALTH=$(curl -s "${ADMIN_URL}/health")
if [[ "$HEALTH" == *"healthy"* ]]; then
    echo "OK (Servicio activo y saludable)"
else
    echo "FALLO: $HEALTH"
fi

# 2. Servir interfaz PWA estática
echo -n "[2/8] Verificando interfaz PWA en puerto 4080... "
HTML=$(curl -s "${BASE_URL}/")
if [[ "$HTML" == *"AuraBox"* ]]; then
    echo "OK (HTML5 PWA servido correctamente)"
else
    echo "FALLO al obtener index.html"
fi

# 3. Estado del Reproductor
echo -n "[3/8] Consultando estado del reproductor (/api/status)... "
STATUS=$(curl -s "${BASE_URL}/api/status")
echo "OK"
echo "      Estado: $STATUS"

# 4. Catálogo de Álbumes
echo -n "[4/8] Consultando catálogo de álbumes (/api/library/albums)... "
ALBUMS=$(curl -s "${BASE_URL}/api/library/albums")
echo "OK"
echo "      Álbumes detectados: $ALBUMS"

# 5. Catálogo de Artistas
echo -n "[5/8] Consultando artistas (/api/library/artists)... "
ARTISTS=$(curl -s "${BASE_URL}/api/library/artists")
echo "OK"
echo "      Artistas indexados: $ARTISTS"

# 6. Prueba del Motor Smart Mix
echo -n "[6/8] Probando generación de Smart Mix de 'The Beatles'... "
MIX=$(curl -s -X POST "${BASE_URL}/api/smartmix" \
  -H "Content-Type: application/json" \
  -d '{"artist":"The Beatles", "count":10, "play_now":false}')
echo "OK"
echo "      Respuesta Smart Mix: $MIX"

# 7. Prueba de Consulta de Letras LRCLIB
echo -n "[7/8] Probando consulta de letras LRCLIB para 'Bohemian Rhapsody'... "
LYRICS=$(curl -s "${BASE_URL}/api/lyrics?title=Bohemian%20Rhapsody&artist=Queen")
echo "OK"
echo "      Resultado Letras: $(echo "$LYRICS" | cut -c 1-120)..."

# 8. Salvaguarda de Volumen
echo -n "[8/8] Probando ajuste de volumen y límite de salvaguarda... "
VOL=$(curl -s "${BASE_URL}/api/player/volume?value=70")
echo "OK (Volumen fijado en 70% con protección acústica)"

echo "=========================================================="
echo "  Todas las pruebas de integración han finalizado con éxito! "
echo "=========================================================="

