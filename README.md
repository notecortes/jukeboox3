# AuraBox — Hi-Fi Audio Jukebox & Embedded Linux Music Player

**AuraBox** es un sistema integral, ultrarrápido y de bajo consumo (<50MB de RAM) diseñado para transformar una **Raspberry Pi (3B+, 4 o 5)** o cualquier máquina Linux en un reproductor de audio audiófilo y Jukebox inteligente.

Cuenta con una **interfaz PWA nativa en HTML5, CSS3 y JavaScript vanilla** (sin Node.js, Angular, React ni Tailwind), diseñada con ergonomía **Thumb-First** (control con una sola mano desde smartphone), un motor de recomendación **Smart Mix** con fallback offline, soporte para sincronización de letras en tiempo real vía **LRCLIB**, protección acústica de volumen y conmutación de salidas de audio entre Jack 3.5mm, DAC USB e I2S HAT.

---

## 🚀 Arquitectura y Puertos (Docker)

La aplicación está totalmente containerizada mediante `docker-compose.yml` y utiliza estrictamente puertos en el rango 4000 para evitar colisiones:

| Puerto | Protocolo | Descripción |
|---|---|---|
| **4080** | HTTP | Interfaz Web Principal PWA, API REST y flujo SSE |
| **4081** | HTTP | Métricas del sistema y verificación de salud (`/health`, `/metrics`) |
| **4082** | HTTP | Transmisión de Audio Hi-Fi MP3 320kbps (para escuchar en el móvil) |
| **4600** | TCP | Puerto de control MPD mapeado (aislado del 6600 del host) |

### Puntos de Montaje de Audio y Almacenamiento:
- `/dev/snd`: Acceso directo al bus de sonido ALSA del host (grupo `audio`).
- `/music/local`: Música almacenada en la tarjeta SD o disco local.
- `/music/nas`: Recursos de red montados (SMB/NFS).
- `/music/usb`: Pendrives y discos duros USB extraíbles.
- `/app/data`: Persistencia de la base de datos SQLite (`aurabox.db`) y caché de carátulas.

---

## 📱 Ergonomía Móvil y UX Thumb-First

Optimizada para pantallas de smartphones (360px a 430px) y operable cómodamente con el pulgar:

1. **Barra de Navegación Inferior (Dock):** 5 secciones clave (`Biblioteca`, `Buscar`, `Mixes`, `Cola`, `Ajustes`) con blancos táctiles ≥ 48px.
2. **Mini-Player Persistente Flotante:**
   - Dockeado inmediatamente encima del menú inferior.
   - Visible en el 100% de las vistas si hay reproducción activa.
   - **Gesto Tap / Swipe Up:** Despliega la vista **Now Playing** a pantalla completa.
   - **Gesto Swipe Horizontal:** Salta de canción (siguiente/anterior) con animación optimista.
3. **Now Playing Envolvente:**
   - Carátula dominante en alta resolución con fondo dinámico reactivo desenfocado.
   - Insignia técnica Hi-Fi (ej. `FLAC 96kHz / 24-bit` o `MP3 320kbps`).
   - **Gesto Swipe Down:** Cierra el modal y regresa a la vista previa.
   - **Letras Sincronizadas (LRCLIB):** Acceso en 1 toque. Muestra la letra línea por línea desplazándose y centrándose automáticamente al compás de la canción. Permite tocar cualquier verso para saltar a ese segundo del tema.
4. **Índice Alfabético Vertical (A-Z Slider):** En el margen derecho para saltar instantáneamente a cualquier letra en álbumes o artistas.
5. **Salvaguarda de Volumen:** Deslizador con atenuación progresiva, botones directos `+` y `-` (pasos de 5%) y límite de ganancia máxima (por defecto 85%) para impedir saltos bruscos al 100%.
6. **Cola Jukebox Interactiva:**
   - Reordenación táctil mediante **Drag & Drop**.
   - Eliminación rápida por **Swipe lateral** (deslizar hacia la izquierda).
   - Barajado inteligente anti-repetición.
7. **MediaSession API:** Control total desde la pantalla de bloqueo de Android/iOS y auriculares Bluetooth.
8. **Sincronización Multi-Dispositivo (<50ms):** Conexión vía **Server-Sent Events (SSE)**. Cualquier cambio en un móvil se refleja al instante en todos los demás.

---

## ⚡ Motor "Smart Mix" de Artistas Locales

Al seleccionar **«Crear Smart Mix de [Artista]»**:
1. **Consulta Online:** Consulta la API abierta de MusicBrainz / ListenBrainz para descubrir artistas acústicamente afines.
2. **Fallback Offline Inteligente:** Si no hay internet, analiza la base SQLite local cruzando géneros coincidentes, época (año de lanzamiento dentro de ±10 años) y tempo (BPM).
3. **Filtrado contra Almacenamiento:** Restringe los candidatos únicamente a los temas que físicamente existen en `/music/local`, `/music/nas` o `/music/usb`.
4. **Mezcla Balanceada y Armónica:**
   - **40%** de canciones destacadas del artista semilla.
   - **60%** de canciones de artistas afines locales.
   - Intercalado natural: `[Semilla, Afín1, Afín2, Semilla, Afín3, ...]` evitando que dos temas del mismo artista suenen seguidos.

---

## 🎛️ Audio Hi-Fi y Hardware

- **Gapless Playback:** Transición perfecta entre pistas sin silencios artificiales.
- **Normalización de Volumen ReplayGain:** Modo pista / modo álbum configurable.
- **Crossfade Configurable:** Fundido cruzado entre canciones ajustable de 0 a 15 segundos.
- **Selector de Salidas en Caliente:** Conmuta directamente entre:
  * Jack analógico 3.5mm integrado (`snd_bcm2835`).
  * DAC USB externo (`hw:1,0`).
  * DAC I2S HAT (`sndrpihifiberry` / Allo / IQaudio).
  * Transmisión Web HTTP (Hi-Fi Stream en puerto 4082).
- **Temporizador de Apagado (Sleep Timer):** 15, 30, 45, 60 minutos o cancelación.

---

## 🛠️ Puesta en Marcha en Raspberry Pi y Otros Equipos

> 📖 **Guía Completa Detallada:** Consulta el manual paso a paso con diagramas de cableado de amplificador, montaje NAS, configuración de DACs Hi-Fi y despliegue en PC/Mac en [`docs/instalacion.md`](docs/instalacion.md).

### Opción 1: Instalación Automatizada en Raspberry Pi (Recomendada)
Clona el repositorio y ejecuta el script instalador en tu Raspberry Pi para configurar Docker, permisos de audio, montaje del NAS y auto-arranque 24/7 en un solo paso:
```bash
git clone https://github.com/notecortes/jukeboox3.git ~/aurabox
cd ~/aurabox
chmod +x scripts/install-rpi.sh
./scripts/install-rpi.sh
```

### Opción 2: Despliegue Manual con Docker Compose
```bash
# 1. Crear archivo privado de credenciales desde la plantilla
cp .env.example .env
# Edita .env con la IP, usuario y contraseña de tu NAS (excluido en .gitignore)
nano .env

# 2. Iniciar contenedores en segundo plano
docker compose up -d
```

### Verificación y Acceso
```bash
./scripts/test-aurabox.sh
```
- **Interfaz Web PWA:** `http://<IP_DE_TU_RASPBERRY_PI>:4080`
- **Métricas del Sistema:** `http://<IP_DE_TU_RASPBERRY_PI>:4081/health`
- **Audio Stream HTTP:** `http://<IP_DE_TU_RASPBERRY_PI>:4082`

### 4. Configurar Auto-Montaje de Pendrives USB (`udev`)
Copia la regla de `udev` para auto-montaje instantáneo y re-escaneo:
```bash
sudo cp scripts/automount-usb.sh /usr/local/bin/aurabox-usb
sudo chmod +x /usr/local/bin/aurabox-usb
```
Añade a `/etc/udev/rules.d/99-aurabox-usb.rules`:
```udev
ACTION=="add", SUBSYSTEM=="block", KERNEL=="sd[a-z][0-9]", RUN+="/usr/local/bin/aurabox-usb add /dev/%k"
ACTION=="remove", SUBSYSTEM=="block", KERNEL=="sd[a-z][0-9]", RUN+="/usr/local/bin/aurabox-usb remove /dev/%k"
```
Recarga `udev`:
```bash
sudo udevadm control --reload-rules
```

---

## 📡 API REST y Flujos SSE

- `GET /api/status`: Estado del reproductor y salidas activas.
- `GET /api/events`: Stream SSE bidireccional en tiempo real (<50ms).
- `POST /api/player/play`, `/pause`, `/toggle`, `/next`, `/prev`, `/seek`.
- `POST /api/player/volume?value=X`: Ajuste con salvaguarda acústica.
- `POST /api/queue/add`: Encolar canciones (`play_now`, `play_next`, `append`).
- `POST /api/queue/move?from=X&to=Y`: Reordenar cola (Drag & Drop).
- `POST /api/queue/remove?index=X`: Eliminar canción (Swipe-to-delete).
- `POST /api/smartmix`: Generar sesión Smart Mix balanceada.
- `GET /api/lyrics?title=...&artist=...`: Letras sincronizadas LRCLIB.
- `POST /api/library/scan`: Disparar escaneo de bibliotecas en segundo plano.

